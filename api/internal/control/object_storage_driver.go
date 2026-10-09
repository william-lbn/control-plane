package control

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed assets/storage-schema.sql
var objectStorageSchema string

type ObjectStorageSpec struct {
	Database string `json:"database"`
}
type objectStoragePayload struct {
	ProjectID  string            `json:"project_id"`
	BranchID   string            `json:"branch_id"`
	EndpointID string            `json:"endpoint_id"`
	Generation int64             `json:"generation"`
	Spec       ObjectStorageSpec `json:"spec"`
	metadata   pgx.Tx
}

func applyObjectStorageSQLTx(ctx context.Context, tx pgx.Tx, p objectStoragePayload) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,922743))`, p.ProjectID); err != nil {
		return err
	}
	var granted bool
	if err := tx.QueryRow(ctx, `SELECT has_database_privilege(current_user,current_database(),'CREATE')`).Scan(&granted); err != nil {
		return err
	}
	if !granted {
		return errors.New("database owner must grant CREATE to control_probe before enabling Object Storage")
	}
	marker := "neon-control:object-storage:" + p.ProjectID
	var comment string
	err := tx.QueryRow(ctx, `SELECT COALESCE(obj_description(oid,'pg_namespace'),'') FROM pg_namespace WHERE nspname='neon_storage'`).Scan(&comment)
	if err != nil && !isNoRows(err) {
		return err
	}
	if err == nil && comment != marker {
		return errors.New("neon_storage schema ownership conflict")
	}
	if isNoRows(err) {
		if _, err = tx.Exec(ctx, `CREATE SCHEMA neon_storage; REVOKE ALL ON SCHEMA neon_storage FROM PUBLIC;`+objectStorageSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "COMMENT ON SCHEMA neon_storage IS "+pgLiteral(marker)); err != nil {
			return err
		}
	}
	// The marker and immutable database identity preserve inherited manifests.
	// Re-enabling and companion replay never truncate objects or clone bytes.
	var generation int64
	err = tx.QueryRow(ctx, `SELECT generation FROM neon_storage.installations WHERE branch_id=$1 FOR UPDATE`, p.BranchID).Scan(&generation)
	if err != nil && !isNoRows(err) {
		return err
	}
	if generation > p.Generation {
		return errors.New("Object Storage SQL generation advanced")
	}
	_, err = tx.Exec(ctx, `INSERT INTO neon_storage.installations(branch_id,generation) VALUES($1,$2) ON CONFLICT(branch_id) DO UPDATE SET generation=$2`, p.BranchID, p.Generation)
	return err
}
func (s *server) installObjectStorage(ctx context.Context, p objectStoragePayload) error {
	release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
	if err != nil {
		return err
	}
	defer release()
	conn, err := s.dataAPIConnection(ctx, dataAPIPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID, Spec: DataAPISpec{Database: p.Spec.Database}})
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = applyObjectStorageSQLTx(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *server) reconcileObjectStorage(ctx context.Context, id, worker, action string, p objectStoragePayload) error {
	if s.storage == nil || !dataBranchID.MatchString(p.BranchID) || !validPGIdentifier(p.Spec.Database) {
		return errors.New("Object Storage Driver unavailable")
	}
	var generation int64
	if err := s.db.QueryRow(ctx, `SELECT generation FROM object_storage_instances WHERE branch_id=$1 AND project_id=$2 AND endpoint_id=$3`, p.BranchID, p.ProjectID, p.EndpointID).Scan(&generation); err != nil || generation != p.Generation {
		return errors.New("Object Storage intent generation mismatch")
	}
	enable := action == "enable_object_storage"
	steps := []func(context.Context) error{
		func(ctx context.Context) error {
			if enable {
				return s.storage.verify(ctx)
			}
			return nil
		},
		func(ctx context.Context) error {
			if enable {
				return s.installObjectStorage(ctx, p)
			}
			return nil
		},
		func(ctx context.Context) error {
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
				return err
			}
			state := "active"
			if !enable {
				state = "disabled"
			}
			tag, err := tx.Exec(ctx, `UPDATE object_storage_instances SET state=$4,updated_at=now() WHERE branch_id=$1 AND project_id=$2 AND generation=$3`, p.BranchID, p.ProjectID, p.Generation, state)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errLeaseLost
			}
			_, err = tx.Exec(ctx, `INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version,public_endpoint,version,last_observed_at) VALUES($1,'object_storage',$2,$2,'object-rest-v1',$3,$4,now()) ON CONFLICT(branch_id,service_kind) DO UPDATE SET desired_state=$2,observed_state=$2,driver_version='object-rest-v1',public_endpoint=$3,version=$4,last_observed_at=now()`, p.BranchID, state, "/storage/v1/"+p.BranchID, p.Generation)
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		},
	}
	for i, step := range steps {
		if err := s.createStep(ctx, id, worker, i, "object_storage_reconcile", step); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) queueInheritedObjectStorageTx(ctx context.Context, tx pgx.Tx, creationID string, p createPayload) error {
	if s.storage == nil {
		return nil
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT a.spec FROM branches b JOIN object_storage_instances a ON a.branch_id=b.parent_branch_id AND a.state='active' WHERE b.id=$1 AND NOT EXISTS(SELECT 1 FROM object_storage_instances WHERE branch_id=b.id)`, p.BranchID).Scan(&raw)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	service := objectStoragePayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID, Generation: 1}
	if err = json.Unmarshal(raw, &service.Spec); err != nil {
		return err
	}
	payload, _ := json.Marshal(service)
	if _, err = tx.Exec(ctx, `INSERT INTO object_storage_instances(branch_id,project_id,endpoint_id,generation,state,spec) VALUES($1,$2,$3,1,'provisioning',$4)`, p.BranchID, p.ProjectID, p.EndpointID, raw); err != nil {
		return err
	}
	id := newID("op_")
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) SELECT $1,project_id,'object_storage',$2,'enable_object_storage','queued',actor_id,request_id,$3 FROM operations WHERE id=$4`, id, p.BranchID, payload, creationID); err != nil {
		return err
	}
	return addObjectStorageSteps(ctx, tx, id)
}
func addObjectStorageSteps(ctx context.Context, tx pgx.Tx, id string) error {
	for i, step := range []string{"verify_dedicated_blob_store", "install_branch_directory", "verify_and_commit_service"} {
		if _, err := tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, id, i, step); err != nil {
			return err
		}
	}
	return nil
}

// Admission holds shared lifecycle locks for the whole bounded I/O request.
// Disable/delete must wait for admitted requests, then close all new access.
// The SQL gate also excludes the existing idle-suspension controller.
func (s *server) openObjectStorage(r *http.Request, project, branch string) (*pgx.Conn, objectStoragePayload, func(), error) {
	p := objectStoragePayload{ProjectID: project, BranchID: branch}
	if s.storage == nil {
		return nil, p, nil, errors.New("Object Storage Driver disabled")
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		return nil, p, nil, err
	}
	cleanup := func() { _ = tx.Rollback(context.Background()) }
	var raw []byte
	// Match lifecycle's lock order: project, branch, service. This prevents a
	// disable and project deletion from waiting on each other's row locks.
	err = tx.QueryRow(r.Context(), `SELECT p.id FROM projects p JOIN branches b ON b.project_id=p.id WHERE b.id=$1 AND ($2='' OR p.id=$2) AND p.state='ready' AND p.deleted_at IS NULL FOR SHARE OF p`, branch, project).Scan(&p.ProjectID)
	if err != nil {
		cleanup()
		return nil, p, nil, err
	}
	var branchID string
	err = tx.QueryRow(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND state='ready' AND deleted_at IS NULL FOR SHARE`, branch, p.ProjectID).Scan(&branchID)
	if err != nil {
		cleanup()
		return nil, p, nil, err
	}
	err = tx.QueryRow(r.Context(), `SELECT a.endpoint_id,a.generation,a.spec FROM object_storage_instances a JOIN endpoints e ON e.id=a.endpoint_id WHERE a.branch_id=$1 AND a.project_id=$2 AND a.state='active' AND e.state='active' AND e.deleted_at IS NULL FOR SHARE OF a`, branch, p.ProjectID).Scan(&p.EndpointID, &p.Generation, &raw)
	if err != nil {
		cleanup()
		return nil, p, nil, err
	}
	if json.Unmarshal(raw, &p.Spec) != nil {
		cleanup()
		return nil, p, nil, errors.New("invalid Object Storage intent")
	}
	// Use this transaction's advisory lock instead of reserving a second pool
	// session: concurrent requests must not exhaust the pool while waiting for
	// another connection. Session and transaction locks share the same key.
	for {
		var acquired bool
		err = tx.QueryRow(r.Context(), `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1,741932))`, p.EndpointID).Scan(&acquired)
		if err != nil {
			cleanup()
			return nil, p, nil, err
		}
		if acquired {
			break
		}
		select {
		case <-r.Context().Done():
			cleanup()
			return nil, p, nil, r.Context().Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	conn, err := s.dataAPIConnection(r.Context(), dataAPIPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID, Spec: DataAPISpec{Database: p.Spec.Database}})
	if err != nil {
		cleanup()
		return nil, p, nil, err
	}
	var safe bool
	err = conn.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM pg_namespace n JOIN neon_storage.installations i ON i.branch_id=$1 AND i.generation=$2 WHERE n.nspname='neon_storage' AND obj_description(n.oid,'pg_namespace')=$3)`, p.BranchID, p.Generation, "neon-control:object-storage:"+p.ProjectID).Scan(&safe)
	if err != nil || !safe {
		_ = conn.Close(context.Background())
		cleanup()
		return nil, p, nil, errors.New("Object Storage directory identity or generation mismatch")
	}
	p.metadata = tx
	return conn, p, func() { _ = conn.Close(context.Background()); cleanup() }, nil
}
