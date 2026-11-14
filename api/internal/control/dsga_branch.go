package control

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"
)

func (s *server) createDSGAStorageBranch(w http.ResponseWriter, r *http.Request, body branchCreateRequest, storage *dsgaCreationPlan) {
	if os.Getenv("NEON_DSGA_CREATE_ENABLED") != "true" || s.kube == nil || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "dsga_creation_disabled", "PostgreSQL DSGA creation is not enabled")
		return
	}
	if body.CreateEndpoint == nil || *body.CreateEndpoint || body.Password != "" || body.Autoscaling != nil {
		fail(w, r, 422, "dsga_branch_compute_unavailable", "Choose a storage-only branch while DSGA branch Compute is unavailable")
		return
	}
	if body.FunctionCopyPolicy != "" && body.FunctionCopyPolicy != "omit" {
		fail(w, r, 422, "dsga_branch_option_unavailable", "Omit Function copying for this DSGA operation")
		return
	}
	if !validName(body.Name, 63) || body.ParentBranchID == "" {
		fail(w, r, 422, "invalid_branch", "Check branch name and parent branch")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	ctx, err := dsgaCreationRequestContext(r)
	if err != nil {
		s.dsgaCreationFailure(w, r, err)
		return
	}
	r = r.WithContext(ctx)
	project, actor, org := r.PathValue("project"), userFrom(r).ID, accessFrom(r).OrganizationID
	scope := "POST:/projects/" + project + "/branches"
	hash := s.createRequestHash(scope, body)
	if s.replayCreate(w, r, key, hash, "", "branch") {
		return
	}
	parent, err := s.one(ctx, `SELECT p.tenant_id,b.timeline_id
 FROM projects p JOIN branches b ON b.project_id=p.id
 WHERE p.id=$1 AND b.id=$2 AND b.state='ready' AND b.deleted_at IS NULL AND b.expires_at IS NULL`, project, body.ParentBranchID)
	if err != nil {
		fail(w, r, 409, "parent_not_ready", "A ready, retained parent branch is required")
		return
	}
	tenant, timeline := stringVal(parent["tenant_id"]), stringVal(parent["timeline_id"])
	if body.ExpectedParentTimeline != "" && body.ExpectedParentTimeline != timeline {
		fail(w, r, 412, "restore_source_changed", "Inspect the current source timeline again")
		return
	}
	var lsn, source string
	if body.ParentTimestamp != "" || body.ParentLSN != "" {
		point, err := s.observeDSGARestorePoint(ctx, storage, tenant, timeline, body.ParentTimestamp, body.ParentLSN)
		if err != nil {
			var problem restoreError
			if errors.As(err, &problem) {
				fail(w, r, problem.status, problem.code, "The requested fork point is unavailable")
			} else {
				fail(w, r, 503, "restore_point_unavailable", "Could not verify the original restore point")
			}
			return
		}
		lsn, source = point.LSN, "lsn"
		if body.ParentTimestamp != "" {
			source = "timestamp"
			body.ParentTimestamp = point.Timestamp
		}
	} else {
		window, err := s.dsgaRestoreWindow(ctx, storage, tenant, timeline)
		if err != nil || validateRestoreInput("", window.LatestLSN, time.Now()) != nil {
			fail(w, r, 503, "parent_lsn_unavailable", "Could not read the original parent fork point")
			return
		}
		lsn = window.LatestLSN
	}
	p, operation, err := s.admitDSGAStorageBranch(ctx, project, org, actor, tenant, timeline, lsn, source, body, storage, key, hash, requestID(r))
	if err != nil {
		var pgErr restoreError
		if errors.As(err, &pgErr) {
			fail(w, r, pgErr.status, pgErr.code, "The original branch source changed before acceptance")
			return
		}
		if errors.Is(err, errDSGACreationConflict) && s.replayCreate(w, r, key, hash, "", "branch") {
			return
		}
		s.dsgaCreationFailure(w, r, err)
		return
	}
	s.audit(ctx, actor, "create_branch_dsga_v1", "branch", p.BranchID, "accepted", requestID(r))
	s.acceptCreate(w, r, project, p.BranchID, operation, "branch")
}

func (s *server) admitDSGAStorageBranch(ctx context.Context, project, org, actor, tenant, parent, lsn, source string,
	body branchCreateRequest, storage *dsgaCreationPlan, key, hash, request string) (createPayload, string, error) {
	var p createPayload
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return p, "", err
	}
	defer tx.Rollback(ctx)
	lockedTenant, major, region, err := lockDSGAEndpointAuthority(ctx, tx, org, project, actor)
	if err != nil {
		return p, "", err
	}
	lockedParent, err := lockDSGARetainedBranch(ctx, tx, project, body.ParentBranchID)
	if err != nil {
		return p, "", err
	}
	if lockedTenant != tenant || lockedParent != parent || major != storage.PostgresVersion {
		return p, "", restoreError{412, "restore_source_changed"}
	}
	if _, err = tx.Exec(ctx, "SELECT dsga_lock_creation_key($1,$2)", actor, keyHash(key)); err != nil {
		return p, "", err
	}
	var used bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM idempotency_keys WHERE actor_id=$1 AND key_hash=$2)
 OR EXISTS(SELECT 1 FROM dsga_creation_reservations WHERE actor_id=$1 AND key_hash=$2)`, actor, keyHash(key)).Scan(&used); err != nil {
		return p, "", err
	}
	if used {
		return p, "", errDSGACreationConflict
	}
	policy, err := selectDSGAProjectPolicy(s.kube.namespace, major, region)
	if err != nil || policy.Creation.Storage != storage.Storage {
		return p, "", errDSGARuntimeUnavailable
	}
	suffix := stableSuffix(actor, "POST:/projects/"+project+"/branches", key)
	p = creationPayload(project, "br_"+suffix, "", tenant, stableHex(suffix, "timeline"), policy.Bounds)
	p.ParentBranchID, p.ParentTimelineID, p.ParentLSN = body.ParentBranchID, parent, lsn
	p.RestoreSource, p.EndpointType, p.CatalogPolicy = source, "read_write", catalogPreserveInheritedSQL
	plan, err := newDSGACreationPlan(p, policy.Creation, time.Now())
	if err != nil {
		return p, "", err
	}
	operation := newID("op_")
	if _, err = tx.Exec(ctx, `INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,parent_lsn,restore_source,parent_timestamp,
 is_default,protected,state,created_at) VALUES($1,$2,$3,$4,$5,$6,COALESCE(NULLIF($7,''),'current'),NULLIF($8,'')::timestamptz,false,false,'creating',now())`,
		p.BranchID, project, body.Name, p.TimelineID, p.ParentBranchID, p.ParentLSN, source, body.ParentTimestamp); err != nil {
		return p, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version)
 VALUES($1,'postgres','provisioning','provisioning','dsga-v1')`, p.BranchID); err != nil {
		return p, "", err
	}
	if err := addDSGACreateOperation(ctx, tx, "create_branch", "branch", p.BranchID, project, operation, actor, request,
		p, plan, key, hash, []string{"create_timeline", "persist_ready_state"}); err != nil {
		return p, "", err
	}
	if err := s.captureCatalogInheritanceTx(ctx, tx, operation, p, p.ParentBranchID, source != ""); err != nil {
		return p, "", err
	}
	if err := lockDSGARequestCredentialForProject(ctx, tx, org, actor, project); err != nil {
		return p, "", err
	}
	return p, operation, tx.Commit(ctx)
}
