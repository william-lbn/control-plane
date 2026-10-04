package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type boundsPayload struct {
	ProjectID    string `json:"project_id"`
	EndpointID   string `json:"endpoint_id"`
	WorkloadName string `json:"workload_name"`
	MinCPU       int    `json:"min_cpu_milli"`
	MaxCPU       int    `json:"max_cpu_milli"`
	MinMem       int    `json:"min_memory_mib"`
	MaxMem       int    `json:"max_memory_mib"`
}

var errDriverPanic = errors.New("worker driver panic")

func (s *server) runWorker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	workerID := newID("worker_")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.workOnce(ctx, workerID); err != nil {
				s.logger.Error("worker cycle", "error", err)
			}
		}
	}
}

func (s *server) workOnce(ctx context.Context, workerID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id, projectID, action string
	var payload []byte
	err = tx.QueryRow(ctx, `SELECT id,project_id,action,payload FROM operations
        WHERE state='queued' OR (state='running' AND lease_expires_at < now())
        ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &projectID, &action, &payload)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE operations SET state='running',started_at=COALESCE(started_at,now()),
		lease_owner=$2,lease_expires_at=now()+interval '45 seconds',attempts=attempts+1,
        error_code=NULL,error_message=NULL,finished_at=NULL WHERE id=$1`, id, workerID)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	leaseDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-leaseDone:
				return
			case <-runCtx.Done():
				return
			case <-ticker.C:
				tag, leaseErr := s.db.Exec(runCtx, `UPDATE operations SET lease_expires_at=now()+interval '45 seconds'
					WHERE id=$1 AND lease_owner=$2 AND state='running'`, id, workerID)
				if leaseErr != nil || tag.RowsAffected() != 1 {
					cancel()
					return
				}
			}
		}
	}()
	defer close(leaseDone)
	var stepErr error
	var createP createPayload
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				stepErr = errDriverPanic
				s.logger.Error("worker driver panic", "operation_id", id, "panic_type", fmt.Sprintf("%T", recovered))
			}
		}()
		switch action {
		case "create_role", "rotate_role_password", "delete_role", "create_database", "delete_database":
			var p catalogPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				stepErr = err
			} else if p.ProjectID != projectID {
				stepErr = errors.New("catalog operation project identity mismatch")
			} else {
				stepErr = s.reconcileCatalog(runCtx, id, workerID, action, p)
			}
		case "create_project", "create_branch", "create_endpoint":
			if err := json.Unmarshal(payload, &createP); err != nil {
				stepErr = err
			} else {
				stepErr = s.reconcileCreate(runCtx, id, workerID, action, createP)
			}
		case "suspend_endpoint":
			var p suspendPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				stepErr = err
			} else {
				stepErr = s.createStep(runCtx, id, workerID, 0, "verify_idle_and_delete_neonvm", func(ctx context.Context) error {
					return s.suspendCompute(ctx, p)
				})
			}
		case "update_endpoint":
			var p boundsPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				stepErr = err
			} else {
				// Operations queued before this release omitted project_id in payload.
				// The persisted Operation is authoritative for scope, including replay.
				if p.ProjectID == "" {
					p.ProjectID = projectID
				}
				if p.ProjectID != projectID {
					stepErr = errors.New("bounds operation project identity mismatch")
				}
				if stepErr == nil {
					stepErr = s.setCreateStep(runCtx, id, workerID, 0, "running", "")
				}
				if stepErr == nil {
					stepErr = s.kube.reconcileVMBounds(runCtx, p)
				}
				if stepErr == nil {
					stepErr = s.setCreateStep(runCtx, id, workerID, 0, "succeeded", "")
				}
				if stepErr == nil {
					stepErr = s.setCreateStep(runCtx, id, workerID, 1, "running", "")
				}
				if stepErr == nil {
					var metadataTx pgx.Tx
					metadataTx, stepErr = s.db.Begin(runCtx)
					if stepErr == nil {
						stepErr = assertCreateLeaseTx(runCtx, metadataTx, id, workerID)
					}
					if stepErr == nil {
						_, stepErr = metadataTx.Exec(runCtx, `UPDATE endpoints SET min_cpu_milli=$2,max_cpu_milli=$3,
                    min_memory_mib=$4,max_memory_mib=$5,
                    version=version+CASE WHEN ROW(min_cpu_milli,max_cpu_milli,min_memory_mib,max_memory_mib)
                        IS DISTINCT FROM ROW($2::integer,$3::integer,$4::integer,$5::integer) THEN 1 ELSE 0 END,
                    updated_at=now() WHERE id=$1`,
							p.EndpointID, p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem)
					}
					if stepErr == nil {
						stepErr = metadataTx.Commit(runCtx)
					} else if metadataTx != nil {
						_ = metadataTx.Rollback(runCtx)
					}
					if stepErr == nil {
						stepErr = s.setCreateStep(runCtx, id, workerID, 1, "succeeded", "")
					} else {
						_ = s.setCreateStep(runCtx, id, workerID, 1, "failed", "metadata update failed")
					}
				} else {
					_ = s.setCreateStep(runCtx, id, workerID, 0, "failed", "Kubernetes patch failed")
				}
			}
		default:
			stepErr = fmt.Errorf("unsupported action %s", action)
		}
	}()
	state, code, message, retryable := "succeeded", "", "", false
	if stepErr != nil {
		state = "failed"
		code = "reconcile_failed"
		message = "External reconciliation failed; inspect structured service logs by operation ID"
		retryable = true
		s.logger.Error("operation failed", "operation_id", id, "error_type", fmt.Sprintf("%T", stepErr))
		var execution sqlExecutionError
		if errors.As(stepErr, &execution) {
			failure := classifySQLFailure(stepErr)
			s.logger.Error("operation SQL failure", "operation_id", id, "stage", failure.stage, "sqlstate", failure.sqlState, "error_class", failure.class)
		}
		var dependency kubeError
		if errors.As(stepErr, &dependency) {
			s.logger.Error("operation dependency failure", "operation_id", id, "http_status", dependency.Status)
		}
	}
	var waitErr vmWaitError
	if errors.As(stepErr, &waitErr) {
		code = "compute_not_ready"
		message = "NeonVM did not become Running; inspect VM, Pod and scheduler events before retrying"
		s.logger.Warn("compute readiness failed", "operation_id", id, "vm_phase", waitErr.phase)
	}
	if runCtx.Err() != nil && ctx.Err() == nil {
		state, retryable = "failed", true
		if code != "compute_not_ready" {
			code, message = "reconcile_timeout", "Reconciliation timed out"
		}
	}
	if errors.Is(stepErr, errDriverPanic) {
		state, code, message, retryable = "failed", "driver_panic", "Worker driver failed internally; inspect operation ID and repair before retry", false
	}
	// Commit the result and any failure cleanup under the same locked lease.
	// A stale worker must not change resource state after a successor has claimed
	// the operation. The service context is used because runCtx may have timed out.
	finishTx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer finishTx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, finishTx, id, workerID); err != nil {
		return err
	}
	if state == "failed" {
		if _, err = finishTx.Exec(ctx, `UPDATE operation_steps SET state='failed',
			detail='Reconciliation interrupted',updated_at=now()
			WHERE operation_id=$1 AND state='running'`, id); err != nil {
			return err
		}
		if action == "create_project" {
			if _, err = finishTx.Exec(ctx, `UPDATE projects SET state='error',updated_at=now()
				WHERE id=$1 AND state<>'ready'`, createP.ProjectID); err != nil {
				return err
			}
		}
		if action == "create_project" || action == "create_branch" {
			if _, err = finishTx.Exec(ctx, `UPDATE branches SET state='error'
				WHERE id=$1 AND state<>'ready'`, createP.BranchID); err != nil {
				return err
			}
		}
		if (action == "create_project" || action == "create_branch" || action == "create_endpoint") && createP.EndpointID != "" {
			if _, err = finishTx.Exec(ctx, `UPDATE endpoints SET state='error',updated_at=now()
				WHERE id=$1 AND state<>'active'`, createP.EndpointID); err != nil {
				return err
			}
		}
	}
	tag, err := finishTx.Exec(ctx, `UPDATE operations SET state=$2,error_code=NULLIF($3,''),error_message=NULLIF($4,''),
        retryable=$5,finished_at=now(),lease_owner=NULL,lease_expires_at=NULL
		WHERE id=$1 AND lease_owner=$6 AND lease_expires_at>now() AND state='running'`,
		id, state, code, message, retryable, workerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errLeaseLost
	}
	return finishTx.Commit(ctx)
}

func (s *server) retryFailed(ctx context.Context, id, project string) error {
	tag, err := s.db.Exec(ctx, `UPDATE operations SET state='queued',finished_at=NULL,error_code=NULL,error_message=NULL,
        lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND project_id=$2 AND state='failed' AND retryable`, id, project)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
