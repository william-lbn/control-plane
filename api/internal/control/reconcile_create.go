package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

var errLeaseLost = errors.New("operation lease lost")

func (s *server) assertLease(ctx context.Context, operationID, workerID string) error {
	var valid bool
	err := s.db.QueryRow(ctx, `SELECT state='running' AND lease_owner=$2 AND lease_expires_at>now()
		FROM operations WHERE id=$1`, operationID, workerID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errLeaseLost
	}
	return nil
}

func (s *server) setCreateStep(ctx context.Context, operationID, workerID string, ordinal int, state, detail string) error {
	tag, err := s.db.Exec(ctx, `UPDATE operation_steps SET state=$4,detail=NULLIF($5,''),
		attempts=attempts+CASE WHEN $4='running' THEN 1 ELSE 0 END,updated_at=now()
		WHERE operation_id=$1 AND ordinal=$3 AND EXISTS (
			SELECT 1 FROM operations WHERE id=$1 AND state='running' AND lease_owner=$2
			AND lease_expires_at>now())`, operationID, workerID, ordinal, state, detail)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errLeaseLost
	}
	return nil
}

func assertCreateLeaseTx(ctx context.Context, tx pgx.Tx, operationID, workerID string) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT state='running' AND lease_owner=$2 AND lease_expires_at>now()
		FROM operations WHERE id=$1 FOR UPDATE`, operationID, workerID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errLeaseLost
	}
	return nil
}

func (s *server) createStep(ctx context.Context, operationID, workerID string, ordinal int, name string, fn func(context.Context) error) error {
	if err := s.setCreateStep(ctx, operationID, workerID, ordinal, "running", ""); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := fn(ctx); err != nil {
		_ = s.setCreateStep(ctx, operationID, workerID, ordinal, "failed", "Reconciliation step failed")
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := s.setCreateStep(ctx, operationID, workerID, ordinal, "succeeded", ""); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (s *server) reconcileCreate(ctx context.Context, operationID, workerID, action string, p createPayload) error {
	if p.ProjectID == "" || p.BranchID == "" || p.TenantID == "" || p.TimelineID == "" || (p.EndpointID == "" && action != "create_branch") {
		return errors.New("incomplete create operation payload")
	}
	if p.EndpointID != "" {
		var err error
		p.CatalogSpec, err = s.projectComputeCatalog(ctx, p.ProjectID, p.BranchID)
		if err != nil {
			return err
		}
	}
	ordinal := 0
	if p.RestoreSource != "" {
		if action != "create_branch" || p.ParentTimelineID == "" || (p.RestoreSource != "timestamp" && p.RestoreSource != "lsn") {
			return errors.New("invalid historical branch payload")
		}
		if err := s.createStep(ctx, operationID, workerID, ordinal, "pin_restore_point", func(ctx context.Context) error {
			// A successfully-created child retains its ancestor. Retry must remain
			// possible after the source retention boundary moves past the fork point.
			child, err := s.kube.serviceRequest(ctx, "pageserver-managed", 9898, "v1/tenant/"+p.TenantID+"/timeline/"+p.TimelineID, http.MethodGet, nil)
			if err == nil {
				return validateTimelineAncestor(child, p.ParentTimelineID, p.ParentLSN)
			}
			if !kubeStatusIs(err, http.StatusNotFound) {
				return err
			}
			return s.kube.pinRestoreLSN(ctx, p.TenantID, p.ParentTimelineID, p.ParentLSN)
		}); err != nil {
			return err
		}
		ordinal++
	}
	if action == "create_project" {
		if err := s.createStep(ctx, operationID, workerID, ordinal, "create_managed_tenant", func(ctx context.Context) error {
			return s.kube.createTenant(ctx, p.TenantID)
		}); err != nil {
			return err
		}
		ordinal++
	}
	if action == "create_project" || action == "create_branch" {
		if err := s.createStep(ctx, operationID, workerID, ordinal, "create_timeline", func(ctx context.Context) error {
			return s.kube.createTimeline(ctx, p.TenantID, p.TimelineID, p.ParentTimelineID, p.ParentLSN)
		}); err != nil {
			return err
		}
		ordinal++
	}
	if p.EndpointID == "" {
		return s.createStep(ctx, operationID, workerID, ordinal, "persist_ready_state", func(ctx context.Context) error {
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err = assertCreateLeaseTx(ctx, tx, operationID, workerID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE branches SET state='ready',
				version=version+CASE WHEN state='ready' THEN 0 ELSE 1 END WHERE id=$1 AND project_id=$2`, p.BranchID, p.ProjectID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE branch_service_instances SET desired_state='active',observed_state='active',
				last_observed_at=now() WHERE branch_id=$1 AND service_kind='postgres'`, p.BranchID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		})
	}
	if err := s.createStep(ctx, operationID, workerID, ordinal, "create_neonvm", func(ctx context.Context) error {
		_, err := s.kube.createCompute(ctx, p)
		return err
	}); err != nil {
		return err
	}
	ordinal++
	if err := s.createStep(ctx, operationID, workerID, ordinal, "wait_neonvm_running", func(ctx context.Context) error {
		_, err := s.kube.waitCompute(ctx, p.ProjectID, p.EndpointID)
		return err
	}); err != nil {
		return err
	}
	ordinal++
	if err := s.createStep(ctx, operationID, workerID, ordinal, "publish_proxy_route", func(ctx context.Context) error {
		result, err := s.kube.createCompute(ctx, p)
		if err != nil {
			return err
		}
		vm, err := s.kube.waitCompute(ctx, p.ProjectID, p.EndpointID)
		if err != nil {
			return err
		}
		roles := result["roles"].(map[string]string)
		return s.kube.publishRoute(ctx, p, vm, roles)
	}); err != nil {
		return err
	}
	ordinal++
	if err := s.createStep(ctx, operationID, workerID, ordinal, "sql_probe_via_proxy", func(ctx context.Context) error {
		credential, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(p.EndpointID)+"-credentials"), nil)
		if err != nil || !owned(credential, p.ProjectID, p.EndpointID) {
			return errors.New("probe credential unavailable")
		}
		password, err := secretText(credential, "probePassword")
		if err != nil {
			return err
		}
		probeSQL := "SELECT 1"
		if p.EndpointType == "read_only" {
			probeSQL = "SELECT pg_is_in_recovery(), current_setting('primary_conninfo') <> '', " +
				"(SELECT status FROM pg_stat_wal_receiver LIMIT 1)"
		}
		// The Proxy watches a Kubernetes Secret and may briefly serve its old
		// route snapshot after publishRoute returns. Wait for propagation.
		var probeErr error
		for attempt := 0; attempt < 20; attempt++ {
			result, err := runSQL(ctx, s.proxyHost, s.proxyPort, "control_probe", password, "postgres", selector(p.EndpointID), probeSQL)
			valid := err == nil && len(result.Rows) == 1
			if valid && p.EndpointType == "read_only" {
				valid = len(result.Rows[0]) == 3 && result.Rows[0][0] == true && result.Rows[0][1] == true && result.Rows[0][2] == "streaming"
			} else if valid {
				valid = len(result.Rows[0]) == 1 && fmt.Sprint(result.Rows[0][0]) == "1"
			}
			if valid {
				if p.EndpointType != "read_only" {
					return s.claimInheritedCatalog(ctx, p)
				}
				return nil
			}
			probeErr = err
			if probeErr == nil {
				probeErr = errors.New("proxy probe returned unexpected result")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		return probeErr
	}); err != nil {
		return err
	}
	ordinal++
	return s.createStep(ctx, operationID, workerID, ordinal, "persist_ready_state", func(ctx context.Context) error {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err = assertCreateLeaseTx(ctx, tx, operationID, workerID); err != nil {
			return err
		}
		if action == "create_project" {
			if _, err = tx.Exec(ctx, `UPDATE projects SET state='ready',updated_at=now(),
				version=version+CASE WHEN state='ready' THEN 0 ELSE 1 END WHERE id=$1`, p.ProjectID); err != nil {
				return err
			}
		}
		if action != "create_endpoint" {
			if _, err = tx.Exec(ctx, `UPDATE branches SET state='ready',
				version=version+CASE WHEN state='ready' THEN 0 ELSE 1 END WHERE id=$1 AND project_id=$2`, p.BranchID, p.ProjectID); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE endpoints SET state='active',updated_at=now(),
			version=version+CASE WHEN state='active' THEN 0 ELSE 1 END WHERE id=$1 AND project_id=$2`, p.EndpointID, p.ProjectID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE branch_service_instances SET desired_state='active',observed_state='active',
			last_observed_at=now(),version=version+CASE WHEN observed_state='active' THEN 0 ELSE 1 END
			WHERE branch_id=$1 AND service_kind='postgres'`, p.BranchID); err != nil {
			return err
		}
		if p.EndpointID != "" && p.EndpointType != "read_only" {
			if err = s.queueInheritedManagedAuthTx(ctx, tx, operationID, p); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	})
}
