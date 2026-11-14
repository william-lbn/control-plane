package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"
)

const dsgaRetireResumeAction = "retire_failed_dsga_resume_v1"

type dsgaFailedResumeRetirement struct {
	ProjectID    string `json:"project_id"`
	BranchID     string `json:"branch_id"`
	EndpointID   string `json:"endpoint_id"`
	FailedResume string `json:"failed_resume_operation_id"`
}

func (s *server) retireFailedDSGAResume(w http.ResponseWriter, r *http.Request) {
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
	project, endpoint, failed := r.PathValue("project"), r.PathValue("endpoint"), r.PathValue("operation")
	hash := s.createRequestHash(dsgaRetireResumeAction+":"+project+":"+endpoint+":"+failed, version)
	if s.replayCreate(w, r, key, hash, "", "endpoint") {
		return
	}
	ctx, err := dsgaCreationRequestContext(r)
	if err != nil {
		fail(w, r, 403, "permission_denied", "Current project write access required")
		return
	}
	actor := userFrom(r).ID
	id, err := s.queueFailedDSGAResumeRetirement(ctx, project, endpoint, failed, version, actor, requestID(r), key, hash)
	if err != nil {
		if s.replayCreate(w, r, key, hash, "", "endpoint") {
			return
		}
		switch {
		case errors.Is(err, errDSGASuspensionVersion):
			fail(w, r, 412, "version_mismatch", "Refresh the endpoint before retiring the failed resume")
		case errors.Is(err, pgx.ErrNoRows):
			fail(w, r, 404, "not_found", "Endpoint not found")
		case errors.Is(err, errDSGACreationAuthority):
			fail(w, r, 403, "permission_denied", "Current project write access required")
		case errors.Is(err, errDSGACreationConflict), dsgaSuspensionAdmissionConflict(err):
			fail(w, r, 409, "operation_conflict", "Retirement requires the failed unpublished resume of this suspended endpoint")
		default:
			fail(w, r, 503, "metadata_unavailable", "Could not queue failed resume retirement")
		}
		return
	}
	s.audit(ctx, actor, dsgaRetireResumeAction, "endpoint", endpoint, "accepted", requestID(r))
	s.acceptCreate(w, r, project, endpoint, id, "endpoint")
}

func (s *server) queueFailedDSGAResumeRetirement(ctx context.Context, project, endpoint, failed string, version int64, actor, request, key, hash string) (string, error) {
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
	if _, _, _, err = lockDSGAEndpointAuthority(ctx, tx, org, project, actor); err != nil {
		return "", err
	}
	if err = lockDSGAProjectParents(ctx, tx, p); err != nil {
		return "", err
	}
	var current int64
	if err = tx.QueryRow(ctx, `SELECT version FROM endpoints WHERE id=$1 AND project_id=$2`, endpoint, project).Scan(&current); err != nil {
		return "", err
	}
	if current != version {
		return "", errDSGASuspensionVersion
	}
	id := newID("op_")
	payload := dsgaFailedResumeRetirement{ProjectID: project, BranchID: p.BranchID, EndpointID: endpoint, FailedResume: failed}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if err = addCreateOperationJSON(ctx, tx, dsgaRetireResumeAction, "endpoint", endpoint, project, id, actor, request, raw, key, hash, dsgaResumeRetirementSteps); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}
