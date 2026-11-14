package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *server) resumeDSGAEndpoint(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("NEON_DSGA_SUSPENSION_ENABLED") != "true" || !creationEnabled() || len(s.idempotencyKey) < 32 {
		fail(w, r, 409, "dsga_lifecycle_not_available", "This Compute requires the DSGA resume driver")
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
	hash := s.createRequestHash(dsgaResumeAction+":"+project+":"+endpoint, version)
	if s.replayCreate(w, r, key, hash, "", "endpoint") {
		return
	}
	ctx, err := dsgaCreationRequestContext(r)
	if err != nil {
		fail(w, r, 403, "permission_denied", "Current project write access required")
		return
	}
	actor := userFrom(r).ID
	id, err := s.queueDSGAResume(ctx, project, endpoint, version, actor, requestID(r), key, hash)
	if err != nil {
		if s.replayCreate(w, r, key, hash, "", "endpoint") {
			return
		}
		switch {
		case errors.Is(err, errDSGASuspensionVersion):
			fail(w, r, 412, "version_mismatch", "Refresh the endpoint before resuming")
		case errors.Is(err, pgx.ErrNoRows):
			fail(w, r, 404, "not_found", "Endpoint not found")
		case errors.Is(err, errDSGACreationAuthority):
			fail(w, r, 403, "permission_denied", "Current project write access required")
		case errors.Is(err, errDSGACreationIdentity), errors.Is(err, errDSGACreationConflict), dsgaSuspensionAdmissionConflict(err):
			fail(w, r, 409, "operation_conflict", "The Compute cannot start a new resume. Retry its original resume if one exists")
		case errors.Is(err, errDSGARuntimeUnavailable), errors.Is(err, errDSGACreationPlan):
			fail(w, r, 503, "runtime_unavailable", "No compatible retained Compute release is available")
		default:
			fail(w, r, 503, "metadata_unavailable", "Could not queue the Compute resume")
		}
		return
	}
	s.audit(ctx, actor, dsgaResumeAction, "endpoint", endpoint, "accepted", requestID(r))
	s.acceptCreate(w, r, project, endpoint, id, "endpoint")
}

func (s *server) queueDSGAResume(ctx context.Context, project, endpoint string, version int64, actor, request, key, hash string) (string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	p := createPayload{ProjectID: project, EndpointID: endpoint}
	var org string
	if err = tx.QueryRow(ctx, `SELECT e.branch_id,p.org_id FROM endpoints e JOIN projects p ON p.id=e.project_id
 WHERE e.id=$1 AND p.id=$2 AND e.deleted_at IS NULL AND p.deleted_at IS NULL`, endpoint, project).Scan(&p.BranchID, &org); err != nil {
		return "", err
	}
	tenant, major, region, err := lockDSGAEndpointAuthority(ctx, tx, org, project, actor)
	if err != nil {
		return "", err
	}
	if err = lockDSGAProjectParents(ctx, tx, p); err != nil {
		return "", err
	}
	var suspended bool
	var current int64
	var minMem, maxMem int
	if err = tx.QueryRow(ctx, `SELECT workload_kind='neonvm' AND state='suspended' AND workload_name=$3,version,endpoint_type,
 min_cpu_milli,max_cpu_milli,min_memory_mib,max_memory_mib FROM endpoints WHERE project_id=$1 AND id=$2`, project, endpoint, kubeName(endpoint)).
		Scan(&suspended, &current, &p.EndpointType, &p.MinCPU, &p.MaxCPU, &minMem, &maxMem); err != nil {
		return "", err
	}
	if current != version {
		return "", errDSGASuspensionVersion
	}
	if !suspended {
		return "", errDSGACreationIdentity
	}
	p.TenantID = tenant
	p.TimelineID, err = lockDSGARetainedBranch(ctx, tx, project, p.BranchID)
	if err != nil {
		return "", err
	}
	var originalSuspension string
	var frozen []byte
	if err = tx.QueryRow(ctx, `SELECT gone.id,original.payload
 FROM endpoints e JOIN operations gone ON gone.project_id=e.project_id AND gone.resource_type='endpoint' AND gone.resource_id=e.id
 JOIN operations original ON original.id=gone.payload->>'creation_operation_id'
 WHERE e.id=$1 AND e.project_id=$2 AND gone.action='suspend_endpoint_dsga_v1' AND gone.state='succeeded'
 AND ((e.dsga_runtime_operation_id=original.id AND original.action='resume_endpoint_dsga_v1')
   OR (e.dsga_runtime_operation_id IS NULL AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')))
 AND NOT EXISTS(SELECT 1 FROM operations active WHERE active.project_id=e.project_id AND active.resource_type='endpoint' AND active.resource_id=e.id
   AND active.state IN ('queued','running','retry_wait'))`, endpoint, project).Scan(&originalSuspension, &frozen); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errDSGACreationConflict
		}
		return "", err
	}
	var previous createPayload
	if json.Unmarshal(frozen, &previous) != nil || previous.EndpointType != p.EndpointType || previous.MinCPU != p.MinCPU || previous.MaxCPU != p.MaxCPU {
		return "", errDSGACreationIdentity
	}
	p.MinMem, p.MaxMem = previous.MinMem, previous.MaxMem
	source, err := readDSGAResumeSource(ctx, tx, p, originalSuspension)
	if err != nil {
		return "", err
	}
	previousPlan, err := readDSGAResumePlan(ctx, tx, source, p)
	if err != nil {
		return "", err
	}
	policy, err := selectDSGAProjectPolicy(s.kube.namespace, major, region)
	if err != nil || p.MinCPU < policy.Bounds.MinCPU || p.MaxCPU > policy.Bounds.MaxCPU || p.MinMem < policy.Bounds.MinMem || p.MaxMem > policy.Bounds.MaxMem {
		return "", errDSGARuntimeUnavailable
	}
	plan, err := newDSGAResumePlan(p, previousPlan, policy.Creation, time.Now())
	if err != nil {
		return "", err
	}
	plan.ResumeMinMem, plan.ResumeMaxMem = minMem, maxMem
	if err := plan.validate(p); err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		createPayload
		Plan       dsgaCreationPlan `json:"dsga_creation"`
		ResumeFrom string           `json:"resume_from"`
	}{p, plan, originalSuspension})
	if err != nil {
		return "", err
	}
	id := newID("op_")
	if err = addCreateOperationJSON(ctx, tx, dsgaResumeAction, "endpoint", endpoint, project, id, actor, request, raw, key, hash, dsgaEndpointCreateSteps()); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}
