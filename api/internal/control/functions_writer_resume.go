package control

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"
)

var errFunctionWriterResume = errors.New("original Function Writer resume has not published the expected generation")

type functionDeploymentOperation struct {
	functionInstanceTarget
	WriterResumeOperation string `json:"writer_resume_operation_id,omitempty"`
}

type functionWriterResume struct {
	scope     createPayload
	major     uint32
	region    string
	org       string
	ctx       context.Context
	automatic bool
}

func (s *server) lockFunctionWriterResume(r *http.Request, tx pgx.Tx, function string, resume bool) (functionWriterResume, error) {
	var w functionWriterResume
	ctx := r.Context()
	w.scope.ProjectID, w.scope.BranchID = r.PathValue("project"), r.PathValue("branch")
	var cold bool
	err := tx.QueryRow(ctx, `SELECT f.org_id,e.id,e.state='suspended' AND e.workload_kind='neonvm'
 AND EXISTS (SELECT 1 FROM branch_service_instances v WHERE v.branch_id=e.branch_id AND v.service_kind='postgres' AND v.driver_version='dsga-v1')
 FROM function_definitions f JOIN endpoints e ON e.id=f.endpoint_id AND e.project_id=f.project_id AND e.branch_id=f.branch_id
 WHERE f.id=$1 AND f.project_id=$2 AND f.branch_id=$3 AND e.deleted_at IS NULL`,
		function, w.scope.ProjectID, w.scope.BranchID).Scan(&w.org, &w.scope.EndpointID, &cold)
	if err != nil || !cold {
		return functionWriterResume{}, err
	}
	if os.Getenv("NEON_DSGA_SUSPENSION_ENABLED") != "true" || !creationEnabled() || len(s.idempotencyKey) < 32 {
		return functionWriterResume{}, errDSGARuntimeUnavailable
	}

	actor := userFrom(r).ID
	expected, _ := ctx.Value(functionExpectedIdentityKey{}).(string)
	w.automatic = expected != ""
	if w.automatic {
		if !resume || expected != function {
			return functionWriterResume{}, errDSGACreationAuthority
		}
		w.scope.TenantID, w.major, w.region, err = lockDSGAEndpointActor(ctx, tx, w.org, w.scope.ProjectID, actor)
	} else {
		ctx, err = dsgaCreationRequestContext(r)
		if err == nil {
			w.scope.TenantID, w.major, w.region, err = lockDSGAEndpointAuthority(ctx, tx, w.org, w.scope.ProjectID, actor)
		}
	}
	if err != nil {
		return functionWriterResume{}, err
	}
	w.ctx = ctx
	return w, lockDSGAProjectParents(ctx, tx, w.scope)
}

func (s *server) queueFunctionWriterResume(r *http.Request, tx pgx.Tx, w functionWriterResume, function, operation string, version int64) (string, error) {
	actor := userFrom(r).ID
	if w.automatic {
		var valid bool
		err := tx.QueryRow(w.ctx, `SELECT public_enabled AND idle_enabled AND idle_suspended AND actor_id=$2
 FROM function_http_policies WHERE function_id=$1 FOR SHARE`, function, actor).Scan(&valid)
		if isNoRows(err) || (err == nil && !valid) {
			return "", errDSGACreationAuthority
		}
		if err != nil {
			return "", err
		}
	}
	key := "function-writer-resume-" + operation
	hash := s.createRequestHash(dsgaResumeAction+":"+w.scope.ProjectID+":"+w.scope.EndpointID, version)
	return s.queueDSGAResumeTx(w.ctx, tx, w.scope, w.major, w.region, version, actor, requestID(r), key, hash)
}

func (s *server) verifyFunctionWriterResume(ctx context.Context, p functionDeploymentOperation) error {
	if p.WriterResumeOperation == "" {
		return nil
	}
	var verified bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1
 FROM function_definitions f JOIN endpoints e ON e.id=f.endpoint_id AND e.project_id=f.project_id AND e.branch_id=f.branch_id
 JOIN operations o ON o.id=e.dsga_runtime_operation_id AND o.project_id=e.project_id AND o.resource_id=e.id
 WHERE f.id=$1 AND f.project_id=$2 AND f.branch_id=$3 AND o.id=$4 AND o.resource_type='endpoint'
  AND o.action='resume_endpoint_dsga_v1' AND o.payload->>'branch_id'=f.branch_id AND o.state='succeeded'
  AND o.finished_at IS NOT NULL AND e.state='active' AND e.endpoint_type='read_write' AND e.deleted_at IS NULL)`,
		p.FunctionID, p.ProjectID, p.BranchID, p.WriterResumeOperation).Scan(&verified)
	if err != nil {
		return err
	}
	if !verified {
		return errFunctionWriterResume
	}
	return nil
}

func functionWriterResumeError(err error) (int, string, string) {
	switch {
	case errors.Is(err, errDSGACreationAuthority):
		return 403, "permission_denied", "Current Writer resume permission required"
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errDSGASuspensionVersion), errors.Is(err, errDSGACreationConflict),
		errors.Is(err, errDSGACreationIdentity), dsgaSuspensionAdmissionConflict(err):
		return 409, "function_writer_intent_changed", "The original Writer cannot resume; refresh and check its lifecycle Operation"
	case errors.Is(err, errDSGARuntimeUnavailable), errors.Is(err, errDSGACreationPlan):
		return 503, "runtime_unavailable", "No compatible retained Writer release is available"
	default:
		return 503, "metadata_unavailable", "Could not admit the Function Writer resume"
	}
}
