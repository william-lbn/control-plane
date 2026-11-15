package control

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/realtime"
)

type realtimeQueryIntent struct {
	state         string
	credentialUID string
}

type realtimeQueryPlan struct {
	NodeType string              `json:"Node Type"`
	Children []realtimeQueryPlan `json:"Plans"`
}

func (p realtimeQueryPlan) readOnly() bool {
	switch p.NodeType {
	case "", "ModifyTable", "LockRows":
		return false
	}
	for _, child := range p.Children {
		if !child.readOnly() {
			return false
		}
	}
	return true
}

func (s *server) withRealtimeQueryIntent(ctx context.Context, id, worker, action string, p realtimeQueryOperation,
	use func(pgx.Tx, realtimeQueryIntent) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
			return err
		}
		var receipt realtimeQueryIntent
		err := tx.QueryRow(ctx, `SELECT q.state,COALESCE(q.credential_uid,'')
 FROM realtime_query_operations q JOIN realtime_sources s ON s.id=q.source_id AND s.project_id=q.project_id AND s.branch_id=q.branch_id
 JOIN realtime_source_operations o ON o.id=s.id AND o.state='ready'
 JOIN realtime_source_runtime a ON a.source_id=s.id AND a.writer_id=q.writer_id
 JOIN projects p ON p.id=q.project_id
 JOIN branches b ON b.id=q.branch_id AND b.project_id=q.project_id AND b.timeline_id=s.timeline_id
 JOIN endpoints e ON e.id=q.writer_id AND e.project_id=q.project_id AND e.branch_id=q.branch_id
 WHERE q.id=$1 AND q.operation_id=$2 AND q.project_id=$3 AND q.branch_id=$4 AND q.writer_id=$5 AND q.source_id=$6
 AND s.timeline_id=$7 AND s.database_name=$8 AND s.retired_at IS NULL AND q.query_name=$9 AND $10::jsonb <@ s.relations
 AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL
 AND e.state='active' AND e.deleted_at IS NULL AND e.endpoint_type='read_write' FOR UPDATE OF q`,
			p.ID, id, p.ProjectID, p.BranchID, p.WriterID, p.SourceID, p.TimelineID, p.Database, p.Spec.Name,
			realtimeRelations(p.Spec.Relations)).Scan(&receipt.state, &receipt.credentialUID)
		if err != nil ||
			action == registerRealtimeQuery && receipt.state != "registering" && receipt.state != "ready" ||
			action == retireRealtimeQuery && receipt.state != "retiring" && receipt.state != "retired" {
			return realtime.ErrSourceUnavailable
		}
		return use(tx, receipt)
	})
}

func validateRealtimeQuery(ctx context.Context, conn *pgx.Conn, spec realtimeQuerySpec) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(spec.QueryTimeoutMS)*time.Millisecond)
	defer cancel()
	conn.PgConn().Frontend().SetMaxBodyLen(realtimeSourceMaxFrameBytes)
	err := pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL search_path='pg_catalog'"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout',$1,true)", strconv.Itoa(spec.QueryTimeoutMS)+"ms"); err != nil {
			return err
		}
		desc, err := tx.Prepare(ctx, "", spec.Statement)
		if err != nil {
			return err
		}
		if len(desc.Fields) == 0 || len(desc.ParamOIDs) != spec.ParameterCount {
			return realtime.ErrQueryRejected
		}
		// EXPLAIN without ANALYZE checks the original login's plan and grants
		// without executing the submitted result query.
		args := make([]any, 1+spec.ParameterCount)
		args[0] = pgx.QueryExecModeDescribeExec
		var raw []byte
		err = tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON, COSTS OFF) "+spec.Statement, args...).Scan(&raw)
		var plans []struct{ Plan realtimeQueryPlan }
		if err != nil {
			return err
		}
		if json.Unmarshal(raw, &plans) != nil || len(plans) != 1 || !plans[0].Plan.readOnly() {
			return realtime.ErrQueryRejected
		}
		return nil
	})
	if err != nil && !errors.Is(err, realtime.ErrQueryRejected) {
		return errors.Join(realtime.ErrQueryRejected, sqlExecutionError{"query", err})
	}
	return err
}

func (s *server) reconcileRealtimeQuery(ctx context.Context, id, worker, action string, p realtimeQueryOperation) error {
	var intent realtimeQueryIntent
	fence := func(ctx context.Context, use func(pgx.Tx, realtimeQueryIntent) error) error {
		return s.withRealtimeQueryIntent(ctx, id, worker, action, p, use)
	}
	authorize := func(ctx context.Context) error {
		return fence(ctx, func(_ pgx.Tx, receipt realtimeQueryIntent) error {
			intent = receipt
			return nil
		})
	}
	steps := []func(context.Context) error{
		func(ctx context.Context) error {
			if err := authorize(ctx); err != nil {
				return err
			}
			uid, err := s.reserveRealtimeCredential(ctx, p.loginBinding(intent.credentialUID))
			if err != nil {
				return err
			}
			return fence(ctx, func(tx pgx.Tx, receipt realtimeQueryIntent) error {
				if receipt.credentialUID != "" && receipt.credentialUID != uid {
					return realtime.ErrSourceUnavailable
				}
				_, err := tx.Exec(ctx, "UPDATE realtime_query_operations SET credential_uid=$2 WHERE id=$1", p.ID, uid)
				return err
			})
		},
		func(ctx context.Context) error {
			if err := authorize(ctx); err != nil {
				return err
			}
			return s.setRealtimeServiceLogin(ctx, p.loginBinding(intent.credentialUID), p.Spec.Relations, true, authorize)
		},
		func(ctx context.Context) error {
			if err := authorize(ctx); err != nil || intent.state == "ready" {
				return err
			}
			binding := p.loginBinding(intent.credentialUID)
			conn, err := s.openRealtimeSQL(ctx, binding)
			// A failed Proxy connection has sent no query. Recheck the
			// original Operation, then resolve the bound Writer and Secret
			// again before one additional read-only attempt.
			if err != nil && ctx.Err() == nil {
				failure := classifySQLFailure(err)
				if failure.retryable {
					s.logger.Warn("retrying Realtime query Writer validation", "operation_id", id,
						"stage", failure.stage, "error_class", failure.class, "sqlstate", failure.sqlState)
					if err := authorize(ctx); err != nil {
						return err
					}
					conn, err = s.openRealtimeSQL(ctx, p.loginBinding(intent.credentialUID))
				}
			}
			if err != nil {
				return err
			}
			defer conn.Close(ctx)
			if err := validateRealtimeQuery(ctx, conn, p.Spec); err != nil {
				return err
			}
			return fence(ctx, func(tx pgx.Tx, receipt realtimeQueryIntent) error {
				if receipt.credentialUID != binding.credentialUID || receipt.credentialUID == "" {
					return realtime.ErrSourceUnavailable
				}
				_, err := tx.Exec(ctx, `INSERT INTO realtime_queries(id,source_id,project_id,branch_id,writer_id,name,statement,relations,
 parameter_count,read_role,credential_ref,credential_uid,credential_namespace,max_rows,max_result_bytes,max_frame_bytes,query_timeout_ms)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
					p.ID, p.SourceID, p.ProjectID, p.BranchID, p.WriterID, p.Spec.Name, p.Spec.Statement, realtimeRelations(p.Spec.Relations),
					p.Spec.ParameterCount, binding.role, binding.credential, receipt.credentialUID, p.CredentialNamespace,
					p.Spec.MaxRows, p.Spec.MaxResultBytes, realtimeSourceMaxFrameBytes, p.Spec.QueryTimeoutMS)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, "UPDATE realtime_query_operations SET state='ready' WHERE id=$1", p.ID)
				return err
			})
		},
	}
	if action == retireRealtimeQuery {
		steps = []func(context.Context) error{
			func(ctx context.Context) error {
				return fence(ctx, func(tx pgx.Tx, _ realtimeQueryIntent) error {
					if _, err := tx.Exec(ctx, "UPDATE realtime_queries SET retired_at=clock_timestamp() WHERE id=$1 AND retired_at IS NULL", p.ID); err != nil {
						return err
					}
					return realtime.NotifySourceChange(ctx, tx, p.SourceID)
				})
			},
			func(ctx context.Context) error {
				if err := authorize(ctx); err != nil || intent.credentialUID == "" {
					return err
				}
				return s.setRealtimeServiceLogin(ctx, p.loginBinding(intent.credentialUID), p.Spec.Relations, false, authorize)
			},
			func(ctx context.Context) error {
				if err := authorize(ctx); err != nil || intent.state == "retired" {
					return err
				}
				if err := s.retireRealtimeCredential(ctx, p.loginBinding(intent.credentialUID)); err != nil {
					return err
				}
				return fence(ctx, func(tx pgx.Tx, _ realtimeQueryIntent) error {
					_, err := tx.Exec(ctx, "UPDATE realtime_query_operations SET state='retired' WHERE id=$1", p.ID)
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
		if err := s.createStep(ctx, id, worker, i, "realtime_query_reconcile", step); err != nil {
			return err
		}
	}
	return nil
}
