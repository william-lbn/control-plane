package control

import (
	"context"
	"net/http"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
	"github.com/william-lbn/control-plane/api/internal/realtime"
)

const realtimeSourceOwner = "neon-control/realtime-source"
const realtimeSourceMaxFrameBytes = 1 << 20
const realtimeSourceRetainCommits = 512

type realtimeSourceIntent struct {
	state        string
	credential   string
	slotExpected bool
}

func (p realtimeSourceOperation) loginBinding(uid string) realtimeLoginBinding {
	return realtimeLoginBinding{key: p.sourceKey(), writer: p.WriterID, role: realtimeRole(p.ID),
		credential: "neon-rt-" + p.ID[4:], credentialUID: uid, credentialNamespace: p.CredentialNamespace,
		ownerLabel: realtimeSourceOwner, ownerID: p.ID}
}

// Lock the accepted intent under the original operation lease for every
// receipt. The frozen logical identity remains valid across Writer boots.
func (s *server) withRealtimeSourceIntent(ctx context.Context, id, worker, action string, p realtimeSourceOperation,
	use func(pgx.Tx, realtimeSourceIntent) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
			return err
		}
		var intent realtimeSourceIntent
		err := tx.QueryRow(ctx, `SELECT r.state,COALESCE(r.credential_uid,''),r.slot_create_requested
 FROM realtime_source_operations r JOIN branches b ON b.id=r.branch_id AND b.project_id=r.project_id AND b.timeline_id=r.timeline_id
 JOIN projects p ON p.id=r.project_id JOIN endpoints e ON e.id=r.writer_id AND e.project_id=r.project_id AND e.branch_id=r.branch_id
 WHERE r.id=$1 AND r.operation_id=$2 AND r.project_id=$3 AND r.branch_id=$4 AND r.timeline_id=$5 AND r.writer_id=$6 AND r.database_name=$7
 AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL
 AND e.state='active' AND e.deleted_at IS NULL AND e.endpoint_type='read_write' FOR UPDATE OF r`,
			p.ID, id, p.ProjectID, p.BranchID, p.TimelineID, p.WriterID, p.Spec.Database).
			Scan(&intent.state, &intent.credential, &intent.slotExpected)
		if err != nil ||
			action == registerRealtimeSource && intent.state != "registering" && intent.state != "ready" ||
			action == retireRealtimeSource && intent.state != "retiring" && intent.state != "retired" {
			return realtime.ErrSourceUnavailable
		}
		return use(tx, intent)
	})
}

func realtimeCredential(secret map[string]any, binding realtimeLoginBinding) (string, string, error) {
	if !owned(secret, binding.key.Project, binding.writer) || secret["immutable"] != true ||
		(binding.credentialNamespace != "" && nested(secret, "metadata", "namespace") != binding.credentialNamespace) ||
		nested(secret, "metadata", "labels", "neon-control/branch-id") != binding.key.Branch ||
		nested(secret, "metadata", "labels", binding.ownerLabel) != binding.ownerID ||
		nested(secret, "metadata", "deletionTimestamp") != nil {
		return "", "", realtime.ErrSourceUnavailable
	}
	uid := stringVal(nested(secret, "metadata", "uid"))
	role, userErr := secretText(secret, "user")
	password, pwErr := secretText(secret, "password")
	verifier, verifierErr := secretText(secret, "verifier")
	if uid == "" || binding.credentialUID != "" && binding.credentialUID != uid ||
		userErr != nil || role != binding.role || pwErr != nil || password == "" ||
		verifierErr != nil || !functionsql.ValidVerifier(verifier) {
		return "", "", realtime.ErrSourceUnavailable
	}
	return uid, verifier, nil
}

func (s *server) reserveRealtimeCredential(ctx context.Context, binding realtimeLoginBinding) (string, error) {
	kube := binding.secretKube(s.kube)
	secret, err := kube.request(ctx, http.MethodGet, kube.path("secret", binding.credential), nil)
	if kubeStatusIs(err, http.StatusNotFound) && binding.credentialUID == "" {
		password := randomToken(32)
		verifier, generateErr := scramVerifier(password)
		if generateErr != nil {
			return "", generateErr
		}
		labels := credentialLabels(binding.key.Project, binding.writer)
		labels["neon-control/branch-id"], labels[binding.ownerLabel] = binding.key.Branch, binding.ownerID
		secret, err = kube.createOwned(ctx, "secret", record{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
			"metadata":   record{"name": binding.credential, "namespace": kube.namespace, "labels": labels},
			"stringData": record{"user": binding.role, "password": password, "verifier": verifier}}, binding.key.Project, binding.writer)
	}
	if err != nil {
		return "", realtime.ErrSourceUnavailable
	}
	uid, _, err := realtimeCredential(secret, binding)
	return uid, err
}

func (s *server) setRealtimeServiceLogin(ctx context.Context, binding realtimeLoginBinding, relations []dsgaRealtimeRelation, enable bool,
	authorize func(context.Context) error) error {
	scope := binding.key
	runtime, err := s.dsgaEndpointRuntime(ctx, scope.Project, binding.writer)
	if err != nil || runtime == nil || runtime.Payload.BranchID != scope.Branch || runtime.Payload.TimelineID != scope.Timeline {
		return realtime.ErrSourceUnavailable
	}
	request := dsgaAuthLoginRequest{ProtocolVersion: 1, Service: dsgaRealtimeServiceLogin,
		ProjectID: scope.Project, BranchID: scope.Branch, EndpointID: binding.writer, Database: scope.Database,
		RealtimeID: binding.ownerID, Generation: 2, Relations: &relations}
	verifier := ""
	if enable {
		kube := binding.secretKube(s.kube)
		secret, err := kube.request(ctx, http.MethodGet, kube.path("secret", binding.credential), nil)
		if err != nil {
			return realtime.ErrSourceUnavailable
		}
		_, verifier, err = realtimeCredential(secret, binding)
		if err != nil {
			return err
		}
		request.Generation, request.Verifier = 1, &verifier
	}
	if err := s.reconcileDSGAServiceLogin(ctx, runtime, request, authorize); err != nil {
		return err
	}
	routeAuthority := func(ctx context.Context, current *dsgaEndpointRuntime) error {
		if current == nil || current.Operation != runtime.Operation || current.VMUID != runtime.VMUID {
			return realtime.ErrSourceUnavailable
		}
		return authorize(ctx)
	}
	if err := s.publishBranchServiceIdentity(ctx, scope.Project, scope.Branch, binding.writer, binding.role, verifier, enable, routeAuthority); err != nil {
		return err
	}
	return authorize(ctx)
}

func (s *server) retireRealtimeCredential(ctx context.Context, binding realtimeLoginBinding) error {
	kube := binding.secretKube(s.kube)
	secret, err := kube.request(ctx, http.MethodGet, kube.path("secret", binding.credential), nil)
	if kubeStatusIs(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	uid, _, err := realtimeCredential(secret, binding)
	if err != nil {
		return err
	}
	_, err = kube.request(ctx, http.MethodDelete, kube.path("secret", binding.credential),
		record{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": record{"uid": uid}})
	if kubeStatusIs(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (s *server) realtimeSourcePoint(ctx context.Context, p realtimeSourceOperation, sql *pgx.Conn, binding realtimeLoginBinding) (realtime.Source, error) {
	point, exists, err := realtime.ReadInactiveSlot(ctx, sql, binding.key, p.ID, p.MaxWAL)
	if err != nil || !exists {
		return realtime.Source{}, realtime.ErrSlotUnavailable
	}
	verified, err := s.realtimeLoginConfig(ctx, binding)
	if err != nil {
		return realtime.Source{}, err
	}
	verified.config.RuntimeParams["replication"] = "database"
	start, stop := context.WithTimeout(ctx, verified.timeout)
	defer stop()
	conn, err := pgconn.ConnectConfig(start, &verified.config.Config)
	if err != nil {
		return realtime.Source{}, realtime.ErrSourceUnavailable
	}
	defer conn.Close(ctx)
	original, err := pglogrepl.IdentifySystem(start, conn)
	if err != nil || original.DBName != p.Spec.Database || original.XLogPos < point || original.Timeline <= 0 {
		return realtime.Source{}, realtime.ErrSlotUnavailable
	}
	return realtime.Source{SystemID: original.SystemID, Timeline: original.Timeline, Database: p.Spec.Database,
		Slot: p.ID, Publication: p.Spec.Publication, Relations: p.relations(), ConfirmedLSN: point}, nil
}

func (s *server) reconcileRealtimeSource(ctx context.Context, id, worker, action string, p realtimeSourceOperation) error {
	var intent realtimeSourceIntent
	fence := func(call context.Context, step func(pgx.Tx, realtimeSourceIntent) error) error {
		return s.withRealtimeSourceIntent(call, id, worker, action, p, step)
	}
	authorize := func(call context.Context) error {
		return fence(call, func(_ pgx.Tx, receipt realtimeSourceIntent) error {
			intent = receipt
			return nil
		})
	}
	if err := authorize(ctx); err != nil {
		return err
	}
	withSlot := func(call context.Context, use func(*pgx.Conn, realtimeLoginBinding) error) error {
		binding := p.loginBinding(intent.credential)
		if action == registerRealtimeSource {
			binding.maxWALBytes = p.MaxWAL
		}
		sql, err := s.openRealtimeSQL(call, binding)
		if err != nil {
			return err
		}
		defer sql.Close(call)
		return use(sql, binding)
	}
	steps := []func(context.Context) error{
		func(call context.Context) error {
			if err := authorize(call); err != nil {
				return err
			}
			uid, err := s.reserveRealtimeCredential(call, p.loginBinding(intent.credential))
			if err != nil {
				return err
			}
			return fence(call, func(tx pgx.Tx, latest realtimeSourceIntent) error {
				if latest.credential != "" && latest.credential != uid {
					return realtime.ErrSourceUnavailable
				}
				_, err := tx.Exec(call, "UPDATE realtime_source_operations SET credential_uid=$2 WHERE id=$1", p.ID, uid)
				return err
			})
		},
		func(call context.Context) error {
			if err := authorize(call); err != nil {
				return err
			}
			return s.setRealtimeServiceLogin(call, p.loginBinding(intent.credential), []dsgaRealtimeRelation{}, true, authorize)
		},
		func(call context.Context) error {
			if err := authorize(call); err != nil {
				return err
			}
			return withSlot(call, func(sql *pgx.Conn, b realtimeLoginBinding) error {
				if err := realtime.VerifySlotWALRetention(call, sql, p.MaxWAL); err != nil {
					return err
				}
				if err := realtime.VerifyPublication(call, sql, p.Spec.Publication, p.relations()); err != nil {
					return err
				}
				_, exists, err := realtime.ReadInactiveSlot(call, sql, b.key, p.ID, p.MaxWAL)
				if err != nil || exists && !intent.slotExpected {
					return realtime.ErrSlotUnavailable
				}
				if !intent.slotExpected {
					if err := fence(call, func(tx pgx.Tx, found realtimeSourceIntent) error {
						if found.credential != b.credentialUID || found.slotExpected {
							return realtime.ErrSourceUnavailable
						}
						_, err := tx.Exec(call, "UPDATE realtime_source_operations SET slot_create_requested=true WHERE id=$1", p.ID)
						return err
					}); err != nil {
						return err
					}
				}
				if !exists {
					if _, err := sql.Exec(call, "SELECT * FROM pg_create_logical_replication_slot($1,'pgoutput')", p.ID); err != nil {
						return realtime.ErrSlotUnavailable
					}
				}
				_, err = s.realtimeSourcePoint(call, p, sql, b)
				return err
			})
		},
		func(call context.Context) error {
			if err := authorize(call); err != nil || intent.state == "ready" {
				return err
			}
			return withSlot(call, func(sql *pgx.Conn, b realtimeLoginBinding) error {
				if err := realtime.VerifyPublication(call, sql, p.Spec.Publication, p.relations()); err != nil {
					return err
				}
				point, err := s.realtimeSourcePoint(call, p, sql, b)
				if err != nil {
					return err
				}
				return fence(call, func(tx pgx.Tx, state realtimeSourceIntent) error {
					if !state.slotExpected || state.credential != b.credentialUID {
						return realtime.ErrSlotUnavailable
					}
					if err := realtime.RegisterSource(call, tx, b.key, point, realtimeSourceRetainCommits); err != nil {
						return err
					}
					if _, err := tx.Exec(call, `INSERT INTO realtime_source_runtime(source_id,project_id,branch_id,writer_id,
 credential_ref,credential_uid,credential_namespace,max_wal_bytes,max_frame_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
						p.ID, p.ProjectID, p.BranchID, p.WriterID, b.credential, state.credential,
						b.secretKube(s.kube).namespace, p.MaxWAL, realtimeSourceMaxFrameBytes); err != nil {
						return err
					}
					_, err := tx.Exec(call, "UPDATE realtime_source_operations SET state='ready' WHERE id=$1", p.ID)
					return err
				})
			})
		},
	}
	if action == retireRealtimeSource {
		steps = []func(context.Context) error{
			func(call context.Context) error {
				if err := authorize(call); err != nil {
					return err
				}
				var exists bool
				if err := s.db.QueryRow(call, "SELECT EXISTS(SELECT FROM realtime_sources WHERE id=$1)", p.ID).Scan(&exists); err != nil {
					return err
				}
				if exists {
					if err := (realtime.Store{DB: s.db}).Retire(call, p.sourceKey()); err != nil {
						return err
					}
				}
				_, err := s.db.Exec(call, "DELETE FROM realtime_demand WHERE source_id=$1", p.ID)
				return err
			},
			func(call context.Context) error {
				if err := authorize(call); err != nil || !intent.slotExpected {
					return err
				}
				return withSlot(call, func(sql *pgx.Conn, b realtimeLoginBinding) error {
					_, exists, err := realtime.ReadInactiveSlot(call, sql, b.key, p.ID, 0)
					if err != nil || !exists {
						return err
					}
					if _, err := sql.Exec(call, "SELECT pg_drop_replication_slot($1)", p.ID); err != nil {
						return realtime.ErrSlotUnavailable
					}
					_, exists, err = realtime.ReadInactiveSlot(call, sql, b.key, p.ID, 0)
					if err != nil || exists {
						return realtime.ErrSlotUnavailable
					}
					return nil
				})
			},
			func(call context.Context) error {
				if err := authorize(call); err != nil || intent.credential == "" {
					return err
				}
				return s.setRealtimeServiceLogin(call, p.loginBinding(intent.credential), []dsgaRealtimeRelation{}, false, authorize)
			},
			func(call context.Context) error {
				if err := authorize(call); err != nil || intent.state == "retired" {
					return err
				}
				if err := s.retireRealtimeCredential(call, p.loginBinding(intent.credential)); err != nil {
					return err
				}
				return fence(call, func(tx pgx.Tx, _ realtimeSourceIntent) error {
					_, err := tx.Exec(call, "UPDATE realtime_source_operations SET state='retired' WHERE id=$1", p.ID)
					return err
				})
			},
		}
	}
	for i, step := range steps {
		var settled bool
		if err := s.db.QueryRow(ctx, "SELECT state='succeeded' FROM operation_steps WHERE operation_id=$1 AND ordinal=$2", id, i).Scan(&settled); err != nil {
			return err
		}
		if settled {
			continue
		}
		if err := s.createStep(ctx, id, worker, i, "realtime_source_reconcile", step); err != nil {
			return err
		}
	}
	return nil
}
