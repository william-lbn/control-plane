package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var errDSGASuspensionVersion = errors.New("suspension endpoint version changed")

func (s *server) suspendDSGAEndpoint(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("NEON_DSGA_SUSPENSION_ENABLED") != "true" || !creationEnabled() || len(s.idempotencyKey) < 32 {
		fail(w, r, 409, "dsga_lifecycle_not_available", "This Compute requires the DSGA suspension driver")
		return
	}
	version, ok := lifecycleVersion(w, r)
	if !ok {
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	project, endpoint := r.PathValue("project"), r.PathValue("endpoint")
	hash := s.createRequestHash(dsgaSuspendAction+":"+project+":"+endpoint, version)
	if s.replayCreate(w, r, key, hash, "", "endpoint") {
		return
	}
	ctx, err := dsgaCreationRequestContext(r)
	if err != nil {
		fail(w, r, 403, "permission_denied", "Current project write access required")
		return
	}
	actor := userFrom(r).ID
	id, err := s.queueDSGASuspension(ctx, project, endpoint, version, actor, requestID(r), keyHash(key), hash)
	if err != nil {
		// The concurrent winner may have advanced the endpoint version before
		// this request acquired the parent locks. Check its key after rollback.
		if s.replayCreate(w, r, key, hash, "", "endpoint") {
			return
		}
		switch {
		case errors.Is(err, errDSGASuspensionVersion):
			fail(w, r, 412, "version_mismatch", "Refresh the endpoint before suspending")
		case errors.Is(err, pgx.ErrNoRows):
			fail(w, r, 404, "not_found", "Endpoint not found")
		case errors.Is(err, errDSGACreationAuthority):
			fail(w, r, 403, "permission_denied", "Current project write access required")
		case errors.Is(err, errDSGACreationIdentity), errors.Is(err, errDSGACreationConflict), dsgaSuspensionAdmissionConflict(err):
			fail(w, r, 409, "operation_conflict", "The Compute cannot start a new suspension. Retry its original suspension if one exists")
		default:
			fail(w, r, 503, "metadata_unavailable", "Could not queue the Compute suspension")
		}
		return
	}
	s.audit(ctx, actor, dsgaSuspendAction, "endpoint", endpoint, "accepted", requestID(r))
	s.acceptCreate(w, r, project, endpoint, id, "endpoint")
}

func (s *server) queueDSGASuspension(ctx context.Context, project, endpoint string, version int64, actor, request, key, hash string) (string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	p := dsgaSuspensionPayload{ProjectID: project, EndpointID: endpoint}
	var org string
	if err = tx.QueryRow(ctx, `SELECT e.branch_id,p.org_id FROM endpoints e JOIN projects p ON p.id=e.project_id
 WHERE e.id=$1 AND p.id=$2 AND e.deleted_at IS NULL AND p.deleted_at IS NULL`,
		endpoint, project).Scan(&p.BranchID, &org); err != nil {
		return "", err
	}
	if _, _, _, err = lockDSGAEndpointAuthority(ctx, tx, org, project, actor); err != nil {
		return "", err
	}
	scope := createPayload{ProjectID: project, BranchID: p.BranchID, EndpointID: endpoint}
	if err = lockDSGAProjectParents(ctx, tx, scope); err != nil {
		return "", err
	}
	var endpointReady bool
	var current int64
	err = tx.QueryRow(ctx, `SELECT workload_kind='neonvm' AND state='active' AND workload_name=$3,version
 FROM endpoints WHERE project_id=$1 AND id=$2`, project, endpoint, kubeName(endpoint)).Scan(&endpointReady, &current)
	if err != nil {
		return "", err
	}
	if current != version {
		return "", errDSGASuspensionVersion
	}
	if !endpointReady {
		return "", errDSGACreationIdentity
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$2 AND resource_type='endpoint' AND resource_id=$1
 AND state IN ('queued','running','retry_wait'))`, endpoint, project).Scan(&pending); err != nil {
		return "", err
	}
	if pending {
		return "", errDSGACreationConflict
	}
	timeline, err := lockDSGARetainedBranch(ctx, tx, project, p.BranchID)
	if err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `SELECT o.id,b.vm_uid FROM operations o
 JOIN endpoints e ON e.project_id=o.project_id AND e.id=o.payload->>'endpoint_id'
 JOIN dsga_creation_bindings b ON b.operation_id=o.id
 WHERE o.project_id=$1 AND o.payload->>'endpoint_id'=$2 AND o.payload->>'branch_id'=$3
 AND o.payload->>'timeline_id'=$4
 AND o.state='succeeded' AND o.finished_at IS NOT NULL AND b.config_uid IS NOT NULL
 AND ((e.dsga_runtime_operation_id=o.id AND o.action='resume_endpoint_dsga_v1')
 OR (e.dsga_runtime_operation_id IS NULL AND o.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')
 AND (SELECT count(*) FROM operations original WHERE original.project_id=$1 AND original.payload->>'endpoint_id'=$2
   AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')
   AND EXISTS(SELECT 1 FROM dsga_creation_bindings previous WHERE previous.operation_id=original.id))=1))
 AND EXISTS(SELECT 1 FROM dsga_creation_supply_facts f WHERE f.operation_id=o.id AND f.kind='runtime_selection')`,
		project, endpoint, p.BranchID, timeline).Scan(&p.CreationOperation, &p.VMUID)
	if isNoRows(err) {
		return "", errDSGACreationIdentity
	}
	if err != nil {
		return "", err
	}
	id := newID("op_")
	payload, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
 VALUES($1,$2,'endpoint',$3,$4,'queued',$5,$6,$7)`, id, project, endpoint, dsgaSuspendAction, actor, request, payload); err != nil {
		return "", err
	}
	for i, name := range dsgaSuspensionSteps {
		if _, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, id, i, name); err != nil {
			return "", err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`,
		actor, key, hash, id); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func dsgaSuspensionAdmissionConflict(err error) bool {
	var pgErr *pgconn.PgError
	var fault *catalogRenameFault
	return errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23514") ||
		errors.As(err, &fault) && fault.Status == 409
}
