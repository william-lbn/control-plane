package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/william-lbn/control-plane/api/internal/functions"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

type functionStopTarget struct {
	FunctionID string `json:"function_id"`
	ProjectID  string `json:"project_id"`
	BranchID   string `json:"branch_id"`
	Generation int64  `json:"generation"`
	Delete     bool   `json:"delete"`
}

// Explicit manual stop closes admission before it retires resources. It is not
// idle autoscaling: callers acknowledge that background work will end. All
// backing/data remain retained and capacity is released only after UID/Runner
// absence. No force deletion, blanket selector delete or secret GC occurs.
func (s *server) functionStop(w http.ResponseWriter, r *http.Request) {
	if !s.functionsConfigured(w, r) {
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	if err != nil || version < 1 {
		fail(w, r, 428, "version_required", "Function numeric If-Match version required")
		return
	}
	var input struct {
		Acknowledge bool `json:"acknowledge_background_work_stop"`
	}
	if readJSON(r, &input) != nil || !input.Acknowledge {
		fail(w, r, 422, "stop_acknowledgment_required", "Explicit background-work stop acknowledgment required")
		return
	}
	deleting := r.Method == http.MethodDelete
	canonical, _ := json.Marshal(record{"action": "stop_function", "project": r.PathValue("project"), "branch": r.PathValue("branch"), "slug": r.PathValue("slug"), "version": version, "delete": deleting})
	fingerprint := s.functionFingerprint(canonical)
	if s.replayFunctionOperation(w, r, key, fingerprint) {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not stop Function")
		return
	}
	defer tx.Rollback(r.Context())
	var id, state string
	var generation, currentVersion int64
	err = tx.QueryRow(r.Context(), `SELECT id,state,generation,version FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted' FOR UPDATE`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug")).Scan(&id, &state, &generation, &currentVersion)
	if err != nil {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	if currentVersion != version {
		fail(w, r, 412, "version_conflict", "Function changed before stop")
		return
	}
	var pending bool
	if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE resource_type='function' AND resource_id=$1 AND state IN ('queued','running','retry_wait'))`, id).Scan(&pending) != nil || pending {
		fail(w, r, 409, "function_operation_in_progress", "Finish or resolve the original Function Operation first")
		return
	}
	p := functionStopTarget{FunctionID: id, ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch"), Generation: generation + 1, Delete: deleting}
	action, next := "suspend_function", "suspended"
	if deleting {
		action, next = "delete_function", "deleting"
	}
	op := s.functionRequestID("op_", userFrom(r).ID, key, fingerprint)
	payload, _ := json.Marshal(p)
	_, err = tx.Exec(r.Context(), `UPDATE function_definitions SET state=$2,generation=$3,version=version+1,updated_at=now() WHERE id=$1`, id, next, p.Generation)
	if err == nil {
		// Atomic marker avoids a crash between a durable stop and reopening wake.
		automatic, _ := r.Context().Value(functionAutomaticStopKey{}).(bool)
		_, err = tx.Exec(r.Context(), `UPDATE function_http_policies SET idle_suspended=$2 WHERE function_id=$1`, id, automatic)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'function',$3,$4,'queued',$5,$6,$7)`, op, p.ProjectID, id, action, userFrom(r).ID, requestID(r), payload)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,0,'retire_original_instances','queued'),($1,1,'commit_function_stop','queued')`, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), fingerprint, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,$2,'function',$3,'accepted',$4)`, userFrom(r).ID, action, id, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayFunctionOperation(w, r, key, fingerprint) {
			return
		}
		status, code, message := functionAdmissionError(err)
		fail(w, r, status, code, message)
		return
	}
	s.acceptFunctionOperation(w, r, op)
}

func (s *server) lockFunctionStop(ctx context.Context, op, worker string, p functionStopTarget) error {
	var valid bool
	err := s.db.QueryRow(ctx, `SELECT o.state='running' AND o.lease_owner=$2 AND o.lease_expires_at>clock_timestamp()
 AND o.project_id=$3 AND o.resource_id=$4 AND o.resource_type='function'
 AND o.action=CASE WHEN $6 THEN 'delete_function' ELSE 'suspend_function' END
 AND f.project_id=$3 AND f.branch_id=$7 AND f.generation=$5
 AND f.state=CASE WHEN $6 THEN 'deleting' ELSE 'suspended' END
 FROM operations o JOIN function_definitions f ON f.id=o.resource_id WHERE o.id=$1`, op, worker, p.ProjectID, p.FunctionID, p.Generation, p.Delete, p.BranchID).Scan(&valid)
	if err != nil || !valid {
		return errLeaseLost
	}
	return nil
}

// Serialize metadata completion/recovery against both the original Operation
// and Function generation. External execution fencing remains a separate gate.
func lockFunctionStopTx(ctx context.Context, tx pgx.Tx, op, worker string, p functionStopTarget) error {
	if err := assertCreateLeaseTx(ctx, tx, op, worker); err != nil {
		return err
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT generation=$2 AND project_id=$3 AND branch_id=$4
 AND state=CASE WHEN $5 THEN 'deleting' ELSE 'suspended' END FROM function_definitions WHERE id=$1 FOR UPDATE`,
		p.FunctionID, p.Generation, p.ProjectID, p.BranchID, p.Delete).Scan(&valid)
	if err != nil || !valid {
		return errLeaseLost
	}
	return nil
}

// Recover only an initial external creation whose UID recording was lost. The
// complete reserved spec and persisted bootstrap identity must match. A known
// UID is never replaced and no arbitrary same-name resource is adopted.
func (s *server) recoverFunctionStopResources(ctx context.Context, op, worker string, target functionStopTarget, plan functionRuntimePlan) (functionInstanceRecord, error) {
	instance := plan.Instance
	for _, kind := range []string{"service", "vm"} {
		known := instance.ServiceUID
		if kind == "vm" {
			known = instance.VMUID
		}
		if known != nil {
			continue
		}
		item, err := s.kube.request(ctx, http.MethodGet, s.kube.path(kind, instance.VMName), nil)
		var missing kubeError
		if errors.As(err, &missing) && missing.Status == 404 {
			continue
		}
		if err != nil || plan.Bootstrap.UID == "" || plan.Bootstrap.MACKeyVersion != s.functions.input.KeyVersion {
			return instance, errFunctionRuntimeResource
		}
		actual, err := verifyFunctionRuntimeResource(item, plan, kind)
		if err != nil {
			return instance, err
		}
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return instance, errFunctionInstanceStore
		}
		if err = lockFunctionStopTx(ctx, tx, op, worker, target); err != nil {
			_ = tx.Rollback(ctx)
			return instance, err
		}
		column := "service_uid"
		if kind == "vm" {
			column = "vm_uid"
		}
		var saved string
		err = tx.QueryRow(ctx, `UPDATE function_instances SET `+column+`=COALESCE(`+column+`,$2),updated_at=now()
 WHERE id=$1 AND function_id=$3 AND project_id=$4 AND branch_id=$5 AND deployment_id=$6 AND generation=$7 AND state<>'retired'
 AND (`+column+` IS NULL OR `+column+`=$2) RETURNING `+column,
			instance.Scope.InstanceID, actual.UID, target.FunctionID, target.ProjectID, target.BranchID, instance.Scope.DeploymentID, instance.Scope.Generation).Scan(&saved)
		if err == nil {
			err = commitFunctionInstance(ctx, tx, op, worker)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			return instance, errFunctionInstanceConflict
		}
		if kind == "vm" {
			instance.VMUID = &saved
		} else {
			instance.ServiceUID = &saved
		}
		plan.Instance = instance
	}
	return instance, nil
}

func (s *server) retireFunctionResource(ctx context.Context, kind, name, uid string) error {
	item, err := s.kube.request(ctx, http.MethodGet, s.kube.path(kind, name), nil)
	var missing kubeError
	if errors.As(err, &missing) && missing.Status == 404 {
		return nil
	}
	if err != nil || stringVal(nested(item, "metadata", "uid")) != uid || uid == "" {
		return errFunctionRuntimeResource
	}
	_, err = s.kube.request(ctx, http.MethodDelete, s.kube.path(kind, name), record{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": record{"uid": uid, "resourceVersion": stringVal(nested(item, "metadata", "resourceVersion"))}})
	if err != nil { // Resolve only the original result by GET; do not replay DELETE.
		_, observed := s.kube.request(ctx, http.MethodGet, s.kube.path(kind, name), nil)
		if errors.As(observed, &missing) && missing.Status == 404 {
			return nil
		}
		return errFunctionRuntimeResource
	}
	return nil
}

func (s *server) reconcileFunctionStop(ctx context.Context, op, worker string, p functionStopTarget) error {
	if s.functions == nil || s.processRole != roleWorker {
		return errFunctionsPlatform
	}
	if err := s.lockFunctionStop(ctx, op, worker, p); err != nil {
		return err
	}
	if err := s.functionDeployStep(ctx, op, worker, 0, func() error {
		rows, err := s.db.Query(ctx, `SELECT i.id,i.deployment_id,i.generation,i.vm_name,i.service_name,i.bootstrap_secret_ref,i.vm_uid,i.service_uid,i.boot_id,f.slug,
 b.namespace,b.secret_uid,b.content_mac,b.mac_key_version,d.bundle_digest,f.endpoint_id
 FROM function_instances i JOIN function_definitions f ON f.id=i.function_id JOIN function_deployments d ON d.id=i.deployment_id
 LEFT JOIN function_instance_bootstraps b ON b.instance_id=i.id
 WHERE i.function_id=$1 AND i.project_id=$2 AND i.branch_id=$3 AND i.state<>'retired'`, p.FunctionID, p.ProjectID, p.BranchID)
		if err != nil {
			return errFunctionInstanceStore
		}
		type candidate struct {
			instance functionInstanceRecord
			boot     functionBootstrapReference
			bootID   *string
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			var namespace, uid, mac, version, digest *string
			if rows.Scan(&c.instance.Scope.InstanceID, &c.instance.Scope.DeploymentID, &c.instance.Scope.Generation, &c.instance.VMName, &c.instance.ServiceName, &c.instance.SecretName, &c.instance.VMUID, &c.instance.ServiceUID, &c.bootID, &c.instance.Scope.Slug,
				&namespace, &uid, &mac, &version, &digest, &c.boot.WriterID) != nil {
				rows.Close()
				return errFunctionInstanceStore
			}
			c.instance.Scope.ProjectID, c.instance.Scope.BranchID = p.ProjectID, p.BranchID
			c.instance.State = "starting"
			c.boot.Scope, c.boot.FunctionID, c.boot.Name = c.instance.Scope, p.FunctionID, c.instance.SecretName
			if namespace != nil {
				c.boot.Namespace = *namespace
			}
			if uid != nil {
				c.boot.UID = *uid
			}
			if mac != nil {
				c.boot.MAC = *mac
			}
			if version != nil {
				c.boot.MACKeyVersion = *version
			}
			if digest != nil {
				c.boot.BundleDigest = *digest
			}
			candidates = append(candidates, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return errFunctionInstanceStore
		}
		for _, c := range candidates {
			if err = s.lockFunctionStop(ctx, op, worker, p); err != nil {
				return err
			}
			plan := functionRuntimePlan{Target: functionInstanceTarget{p.FunctionID, p.ProjectID, p.BranchID, c.instance.Scope.DeploymentID, c.instance.Scope.Generation, c.instance.Scope.Slug}, Instance: c.instance, Bootstrap: c.boot, Image: s.functions.input.VMImage, NodeName: s.functions.input.NodeName}
			c.instance, err = s.recoverFunctionStopResources(ctx, op, worker, p, plan)
			if err != nil {
				return err
			}
			// A reservation with no external resource still needs exact absence.
			// Never turn a missing UID into blanket authorization to delete by name.
			if c.instance.VMUID != nil {
				if c.boot.UID == "" || c.boot.MACKeyVersion != s.functions.input.KeyVersion {
					return errFunctionBootstrap
				}
				// Expired/unavailable guest trust cannot authorize a replacement or
				// force delete. Explicit stop is authorized by the saved VM UID and
				// full owned spec; signed graceful drain is best effort before normal GC.
				bootstrap, bootstrapErr := s.kube.readFunctionBootstrap(ctx, s.functions.key, c.boot)
				plan := functionRuntimePlan{Target: functionInstanceTarget{p.FunctionID, p.ProjectID, p.BranchID, c.instance.Scope.DeploymentID, c.instance.Scope.Generation, c.instance.Scope.Slug}, Instance: c.instance, Bootstrap: c.boot, Image: s.functions.input.VMImage, NodeName: s.functions.input.NodeName}
				vm, err := s.kube.request(ctx, http.MethodGet, s.kube.path("vm", c.instance.VMName), nil)
				var ke kubeError
				if err == nil {
					if _, err = verifyFunctionRuntimeResource(vm, plan, "vm"); err != nil {
						return err
					}
				}
				if err != nil && !(errors.As(err, &ke) && ke.Status == 404) {
					return errFunctionRuntimeResource
				}
				if c.instance.ServiceUID != nil && c.bootID != nil && err == nil && bootstrapErr == nil {
					service, e := s.kube.request(ctx, http.MethodGet, s.kube.path("service", c.instance.ServiceName), nil)
					if e == nil {
						verified, e := verifyFunctionRuntimeResource(service, plan, "service")
						if e != nil {
							return e
						}
						key, e := base64.StdEncoding.Strict().DecodeString(bootstrap.Config.ManagerKey)
						if e != nil {
							return errFunctionBootstrap
						}
						client, e := functions.NewInstanceClient(c.instance.Scope, netip.MustParseAddrPort(verified.ClusterIP+":9090"), key, s.functions.guestCA)
						if e == nil {
							_ = client.Shutdown(ctx, *c.bootID)
							client.Close()
						}
					}
				}
				if err = s.lockFunctionStop(ctx, op, worker, p); err != nil {
					return err
				}
				if err = s.retireFunctionResource(ctx, "vm", c.instance.VMName, *c.instance.VMUID); err != nil {
					return err
				}
			}
			deadline := time.NewTimer(90 * time.Second)
			ticker := time.NewTicker(2 * time.Second)
			absent := false
			for !absent {
				_, observed := s.kube.request(ctx, http.MethodGet, s.kube.path("vm", c.instance.VMName), nil)
				var ke kubeError
				if errors.As(observed, &ke) && ke.Status == 404 {
					pods, e := s.kube.request(ctx, http.MethodGet, "/api/v1/namespaces/"+s.kube.namespace+"/pods?labelSelector="+url.QueryEscape("vm.neon.tech/name="+c.instance.VMName), nil)
					if e != nil {
						deadline.Stop()
						ticker.Stop()
						return errFunctionRuntimeResource
					}
					items, ok := pods["items"].([]any)
					if !ok {
						deadline.Stop()
						ticker.Stop()
						return errFunctionRuntimeResource
					}
					absent = len(items) == 0
				} else if observed != nil {
					deadline.Stop()
					ticker.Stop()
					return errFunctionRuntimeResource
				}
				if absent {
					break
				}
				select {
				case <-ctx.Done():
					deadline.Stop()
					ticker.Stop()
					return ctx.Err()
				case <-deadline.C:
					ticker.Stop()
					return errFunctionRuntimeResource
				case <-ticker.C:
				}
			}
			deadline.Stop()
			ticker.Stop()
			if c.instance.ServiceUID != nil {
				if err = s.lockFunctionStop(ctx, op, worker, p); err != nil {
					return err
				}
				if err = s.retireFunctionResource(ctx, "service", c.instance.ServiceName, *c.instance.ServiceUID); err != nil {
					return err
				}
			}
			tx, e := s.db.Begin(ctx)
			if e != nil {
				return errFunctionInstanceStore
			}
			if e = lockFunctionStopTx(ctx, tx, op, worker, p); e != nil {
				_ = tx.Rollback(ctx)
				return e
			}
			_, e = tx.Exec(ctx, `UPDATE function_instances SET state='retired',retired_at=now(),updated_at=now() WHERE id=$1 AND function_id=$2 AND state<>'retired'`, c.instance.Scope.InstanceID, p.FunctionID)
			if e == nil {
				e = commitFunctionInstance(ctx, tx, op, worker)
			} else {
				_ = tx.Rollback(ctx)
			}
			if e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return s.functionDeployStep(ctx, op, worker, 1, func() error {
		if err := s.lockFunctionStop(ctx, op, worker, p); err != nil {
			return err
		}
		if p.Delete {
			var ref functionSQLReference
			ref.Scope.FunctionID, ref.Scope.ProjectID, ref.Scope.BranchID = p.FunctionID, p.ProjectID, p.BranchID
			err := s.db.QueryRow(ctx, `SELECT credential_generation,namespace,secret_name,secret_uid,content_mac,mac_key_version,writer_id,database_name,sql_schema,provisioning_role
 FROM function_sql_credentials WHERE function_id=$1 AND project_id=$2 AND branch_id=$3 AND credential_generation=1`, p.FunctionID, p.ProjectID, p.BranchID).
				Scan(&ref.Generation, &ref.Namespace, &ref.Name, &ref.UID, &ref.MAC, &ref.MACKeyVersion, &ref.WriterID, &ref.Scope.Database, &ref.Scope.Schema, &ref.Scope.ProvisioningRole)
			if err != nil && !isNoRows(err) {
				return errFunctionSQLSecret
			}
			if err == nil {
				if !ref.valid() {
					return errFunctionSQLSecret
				}
				writer, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(ref.WriterID)+"-credentials"), nil)
				if err != nil || !owned(writer, p.ProjectID, ref.WriterID) {
					return errFunctionSQLSecret
				}
				password, err := secretText(writer, "probePassword")
				if err != nil {
					return errFunctionSQLSecret
				}
				conn, err := functionsql.ConnectProvisioner(ctx, s.functionSQLPolicy(ref), password)
				if err != nil {
					return errFunctionSQLSecret
				}
				defer conn.Close(context.Background())
				tx, err := conn.Begin(ctx)
				if err != nil {
					return errFunctionSQLSecret
				}
				defer tx.Rollback(ctx)
				if err = functionsql.DisableLoginTx(ctx, tx, ref.Scope); err != nil {
					return err
				}
				if err = s.lockFunctionStop(ctx, op, worker, p); err != nil {
					return err
				}
				if err = tx.Commit(ctx); err != nil {
					return errFunctionSQLSecret
				}
				if err = s.publishBranchServiceIdentity(ctx, p.ProjectID, p.BranchID, ref.WriterID, ref.Scope.Role(), "", false); err != nil {
					return errFunctionSQLSecret
				}
			}
		}
		// SQL data, roles and immutable backings are retained. Deleted Function
		// login/registry admission is disabled; physical GC is a separate gate.
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return errFunctionInstanceStore
		}
		defer tx.Rollback(ctx)
		if err = lockFunctionStopTx(ctx, tx, op, worker, p); err != nil {
			return err
		}
		if p.Delete {
			_, err = tx.Exec(ctx, `UPDATE function_definitions SET state='deleted',deleted_at=now(),updated_at=now() WHERE id=$1 AND generation=$2 AND state='deleting'`, p.FunctionID, p.Generation)
		}
		if err != nil {
			return err
		}
		return commitFunctionInstance(ctx, tx, op, worker)
	})
}
