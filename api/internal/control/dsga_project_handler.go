package control

import (
	"errors"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgconn"
)

// PG17公开项目创建的真实入口：保留原意图→一次性凭据→固定来源→同事务接受。
// 只接受现有HTTP创建字段，平台VM/预算/镜像来自可信配置，不允许租户自报。
// 本入口默认关闭，后续worker/初始化回执与完整预算链未验收前不启用生产。
func (s *server) createDSGAProject(w http.ResponseWriter, r *http.Request, body projectCreateRequest) {
	if os.Getenv("NEON_DSGA_CREATE_ENABLED") != "true" || s.kube == nil || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "dsga_creation_disabled", "PG17 DSGA creation is not enabled")
		return
	}
	if !validName(body.Name, 64) || !validName(body.DefaultBranchName, 63) {
		fail(w, r, 422, "invalid_project", "Check project and branch names")
		return
	}
	if _, ok := dsgaPassword(body.Password); !ok {
		fail(w, r, 422, "invalid_database_password", "A compatible 12-256 byte database password is required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	actor, org := userFrom(r).ID, r.PathValue("org")
	ctx, err := dsgaCreationRequestContext(r)
	if err != nil {
		s.dsgaCreationFailure(w, r, err)
		return
	}
	r = r.WithContext(ctx)
	scope := "POST:/organizations/" + org + "/projects"
	hash := s.createRequestHash(scope, body)
	// 已接受的原Operation沿用正式API回放；不接受无密钥legacy摘要。
	if s.replayCreate(w, r, key, hash, "", "project") {
		return
	}
	reservation, err := s.reserveDSGAProject(r.Context(), actor, org, scope, key, hash, dsgaProjectDescription{body.Name, body.DefaultBranchName, body.RegionID, body.Autoscaling})
	if err != nil {
		s.dsgaCreationFailure(w, r, err)
		return
	}
	if reservation.Accepted {
		s.acceptCreate(w, r, reservation.Draft.Payload.ProjectID, reservation.Draft.Payload.ProjectID, reservation.Operation, "project")
		return
	}
	plan := reservation.Draft.Plan
	plan.Credentials = reservation.Credentials
	frozen, err := s.kube.prepareDSGACredentials(r.Context(), reservation.Draft.Payload, plan, reservation.Operation, dsgaCredentialInput{Password: body.Password})
	if err != nil {
		fail(w, r, 503, "credential_unavailable", "Original PG17 credentials could not be confirmed; retry the same request key")
		return
	}
	reservation, err = s.pinDSGAReservationCredentials(r.Context(), reservation, frozen.Credentials)
	if err != nil {
		s.dsgaCreationFailure(w, r, err)
		return
	}
	created, err := s.acceptDSGAProjectReservation(r.Context(), reservation, key, requestID(r))
	if err != nil {
		s.dsgaCreationFailure(w, r, err)
		return
	}
	if created {
		s.audit(r.Context(), actor, "create_project_dsga_v1", "project", reservation.Draft.Payload.ProjectID, "accepted", requestID(r))
	}
	s.acceptCreate(w, r, reservation.Draft.Payload.ProjectID, reservation.Draft.Payload.ProjectID, reservation.Operation, "project")
}

func (s *server) dsgaCreationFailure(w http.ResponseWriter, r *http.Request, err error) {
	var pgerr *pgconn.PgError
	switch {
	case errors.Is(err, errDSGACreationAuthority):
		fail(w, r, 403, "creation_authority_changed", "Current organization creation permission is required")
	case errors.Is(err, errDSGACreationConflict), errors.As(err, &pgerr) && pgerr.Code == "23505":
		fail(w, r, 409, "idempotency_or_name_conflict", "Original creation key or resource name conflicts")
	case errors.Is(err, errOrganizationQuota):
		fail(w, r, 429, "organization_quota_exceeded", "Organization resources and pending creations exceed quota")
	case errors.Is(err, errDSGAProjectBounds):
		fail(w, r, 422, "invalid_dsga_bounds", "Requested CPU and memory must fit the trusted PG17 class and exact slot boundaries")
	case errors.Is(err, errDSGACreationPlan):
		fail(w, r, 503, "dsga_creation_policy_unavailable", "Original PG17 plan or trusted creation policy is unavailable")
	default:
		fail(w, r, 503, "dsga_creation_unavailable", "Original PG17 creation could not be confirmed; retry the same request key")
	}
}
