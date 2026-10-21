package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// Failed parent creation owns exactly its original branch and optional Writer.
// Native tenant/timeline creation may have returned an unknown result. Cleanup
// never deletes or pretends to restore that data, and grants no recovery window.
func verifyFailedParentCreation(ctx context.Context, reader failedEndpointCreationReader, kind string, p deletionPayload) error {
	if (kind != "project" && kind != "branch") || p.FailedCreateOperationID == "" || p.TombstoneID != "" || len(p.BranchIDs) != 1 || len(p.EndpointIDs) > 1 {
		return errors.New("invalid failed parent creation scope")
	}
	resource := p.ProjectID
	if kind == "branch" {
		resource = p.BranchIDs[0]
	}
	var raw []byte
	var terminal bool
	var count int
	err := reader.QueryRow(ctx, `SELECT payload,state='failed' AND finished_at IS NOT NULL AND lease_owner IS NULL AND lease_expires_at IS NULL,
 (SELECT count(*) FROM operations WHERE project_id=$2 AND resource_type=$3 AND resource_id=$4 AND action='create_'||$3)
 FROM operations WHERE id=$1 AND project_id=$2 AND resource_type=$3 AND resource_id=$4 AND action='create_'||$3`, p.FailedCreateOperationID, p.ProjectID, kind, resource).Scan(&raw, &terminal, &count)
	var original createPayload
	if err != nil || !terminal || count != 1 || json.Unmarshal(raw, &original) != nil || original.ProjectID != p.ProjectID || original.BranchID != p.BranchIDs[0] || original.TenantID == "" || original.TimelineID == "" {
		return errors.New("unique terminal original parent creation required")
	}
	if (original.EndpointID == "" && len(p.EndpointIDs) != 0) || (original.EndpointID != "" && (len(p.EndpointIDs) != 1 || p.EndpointIDs[0] != original.EndpointID)) {
		return errors.New("original creation Compute identity mismatch")
	}
	var matches bool
	err = reader.QueryRow(ctx, `SELECT p.source='managed' AND p.tenant_id=$3 AND b.timeline_id=$4
 AND (($5='project' AND b.is_default AND b.parent_branch_id IS NULL AND p.default_branch_id=b.id) OR ($5='branch' AND NOT b.is_default AND b.parent_branch_id IS NOT NULL))
 FROM projects p JOIN branches b ON b.project_id=p.id WHERE p.id=$1 AND b.id=$2`, p.ProjectID, p.BranchIDs[0], original.TenantID, original.TimelineID, kind).Scan(&matches)
	if err != nil || !matches {
		return errors.New("retained tenant/timeline identity mismatch")
	}
	return nil
}

// Called only after ordinary DELETE replay and the project lock. Returning
// false preserves the existing ready-resource admission contract unchanged.
func (s *server) admitFailedParentCleanupTx(w http.ResponseWriter, r *http.Request, tx pgx.Tx, kind, id, projectState, projectName string, projectVersion int64, projectProtected bool, confirm string, version int64, key, hash string) bool {
	var branchState, branchName string
	var branchVersion int64
	var branchProtected, root bool
	branchID := id
	if kind == "project" {
		if projectState != "error" {
			return false
		}
		if err := tx.QueryRow(r.Context(), `SELECT default_branch_id FROM projects WHERE id=$1`, id).Scan(&branchID); err != nil {
			fail(w, r, 409, "failed_creation_required", "Original failed root branch required")
			return true
		}
	} else if projectState != "ready" {
		return false
	}
	err := tx.QueryRow(r.Context(), `SELECT name,state,version,protected,is_default FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, branchID, r.PathValue("project")).Scan(&branchName, &branchState, &branchVersion, &branchProtected, &root)
	if kind == "branch" && (err != nil || branchState != "error") {
		return false
	}
	if err != nil || branchState != "error" || (kind == "project" && !root) || (kind == "branch" && root) {
		fail(w, r, 409, "failed_creation_required", "Original failed branch required")
		return true
	}
	// A never-ready project and root receive protection automatically. Their
	// untouched version-one defaults are included in the explicit failed-project
	// confirmation. Any separately changed protection remains a hard stop.
	if (kind == "project" && ((projectProtected && projectVersion != 1) || (branchProtected && branchVersion != 1))) || (kind == "branch" && branchProtected) {
		fail(w, r, 409, "resource_protected", "Protected resources cannot be cleaned up")
		return true
	}
	name, current := branchName, branchVersion
	if kind == "project" {
		name, current = projectName, projectVersion
	}
	if current != version {
		fail(w, r, 412, "version_mismatch", "Refresh resource before cleanup")
		return true
	}
	if confirm != name {
		fail(w, r, 422, "confirmation_required", "Enter the exact resource name")
		return true
	}
	p := deletionPayload{ProjectID: r.PathValue("project"), BranchIDs: []string{branchID}, EndpointIDs: []string{}}
	rows, err := tx.Query(r.Context(), `SELECT id,payload FROM operations WHERE project_id=$1 AND resource_type=$2 AND resource_id=$3 AND action='create_'||$2`, p.ProjectID, kind, id)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read original creation")
		return true
	}
	origins, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil || len(origins) != 1 {
		fail(w, r, 409, "failed_creation_required", "Unique original failed creation required")
		return true
	}
	p.FailedCreateOperationID = stringVal(origins[0]["id"])
	// Lock all original Computes under the same parent locks as create/retry.
	endpointRows, err := tx.Query(r.Context(), `SELECT id,state,workload_kind FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND deleted_at IS NULL ORDER BY id FOR UPDATE`, p.ProjectID, branchID)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock original Compute")
		return true
	}
	endpoints, err := pgx.CollectRows(endpointRows, pgx.RowToMap)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read original Compute")
		return true
	}
	for _, endpoint := range endpoints {
		if endpoint["state"] != "error" || endpoint["workload_kind"] != "neonvm" {
			fail(w, r, 409, "compute_not_ready", "Only original failed Compute may be cleaned up")
			return true
		}
		p.EndpointIDs = append(p.EndpointIDs, stringVal(endpoint["id"]))
	}
	if verifyFailedParentCreation(r.Context(), tx, kind, p) != nil {
		fail(w, r, 409, "failed_creation_required", "Original failed creation identity does not match")
		return true
	}
	var held bool
	err = tx.QueryRow(r.Context(), `SELECT
 EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait')) OR
 EXISTS(SELECT 1 FROM branches WHERE project_id=$1 AND deleted_at IS NULL AND (($3='project' AND id<>$2) OR parent_branch_id=$2)) OR
 EXISTS(SELECT 1 FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND state<>'deleted') OR
 EXISTS(SELECT 1 FROM function_instances WHERE project_id=$1 AND branch_id=$2 AND state<>'retired') OR
 EXISTS(SELECT 1 FROM data_api_instances WHERE branch_id=$2) OR
 EXISTS(SELECT 1 FROM managed_auth_instances WHERE branch_id=$2) OR
 EXISTS(SELECT 1 FROM object_storage_instances WHERE branch_id=$2) OR
 EXISTS(SELECT 1 FROM branch_service_instances WHERE branch_id=$2 AND service_kind<>'postgres' AND desired_state<>'disabled')`, p.ProjectID, branchID, kind).Scan(&held)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read failed creation dependencies")
		return true
	}
	if held {
		fail(w, r, 409, "cleanup_dependencies_held", "Active work, descendants or Backend dependencies prevent cleanup")
		return true
	}
	op, action := newID("op_"), "cleanup_failed_"+kind
	raw, _ := json.Marshal(p)
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,$3,$4,$5,'queued',$6,$7,$8)`, op, p.ProjectID, kind+"_lifecycle", id, action, userFrom(r).ID, requestID(r), raw)
	for i, name := range []string{"close_failed_creation_admission", "retire_failed_creation_compute", "observe_failed_creation_runners_gone", "retire_failed_creation_services", "persist_failed_creation_tombstone"} {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, op, i, name)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE branches SET state='deleting',deletion_operation_id=$2,version=version+1 WHERE id=$1`, branchID, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE endpoints SET state='deleting',deletion_operation_id=$2,scale_to_zero=false,version=version+1,updated_at=now() WHERE id=ANY($1::text[])`, p.EndpointIDs, op)
	}
	if err == nil && kind == "project" {
		_, err = tx.Exec(r.Context(), `UPDATE projects SET state='deleting',deletion_operation_id=$2,version=version+1,updated_at=now() WHERE id=$1`, p.ProjectID, op)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.finishCreateError(w, r, tx, key, hash, "", kind, err)
		return true
	}
	s.audit(r.Context(), userFrom(r).ID, action, kind, id, "accepted", requestID(r))
	s.acceptCreate(w, r, p.ProjectID, id, op, kind)
	return true
}

func (s *server) reconcileFailedParentCleanup(ctx context.Context, id, worker, action string, p deletionPayload) error {
	kind := "branch"
	if action == "cleanup_failed_project" {
		kind = "project"
	}
	if err := verifyFailedParentCreation(ctx, s.db, kind, p); err != nil {
		return err
	}
	var valid bool
	err := s.db.QueryRow(ctx, `SELECT b.deletion_operation_id=$3 AND b.state IN ('deleting','deleted') AND
 (($4='branch' AND p.state='ready') OR ($4='project' AND p.deletion_operation_id=$3 AND p.state IN ('deleting','deleted')))
 FROM branches b JOIN projects p ON p.id=b.project_id WHERE p.id=$1 AND b.id=$2`, p.ProjectID, p.BranchIDs[0], id, kind).Scan(&valid)
	if err != nil || !valid {
		return errors.New("failed parent deletion intent changed")
	}
	endpoints := make([]endpointDeletionPayload, 0, len(p.EndpointIDs))
	unpublished := map[string]bool{}
	for _, endpoint := range p.EndpointIDs {
		endpoints = append(endpoints, endpointDeletionPayload{ProjectID: p.ProjectID, BranchID: p.BranchIDs[0], EndpointID: endpoint, FailedCreateOperationID: p.FailedCreateOperationID})
		unpublished[selector(endpoint)] = true
	}
	steps := []func(context.Context) error{
		func(ctx context.Context) error {
			return s.kube.setDeletionRoutesAllowUnpublished(ctx, p, id, false, unpublished)
		},
		func(ctx context.Context) error {
			for _, endpoint := range endpoints {
				if err := s.retireFailedEndpointCompute(ctx, id, worker, endpoint, p); err != nil {
					return err
				}
			}
			return nil
		},
		func(ctx context.Context) error {
			for _, endpoint := range endpoints {
				if err := s.observeDeletedEndpointRunners(ctx, endpoint); err != nil {
					return err
				}
			}
			return nil
		},
		func(ctx context.Context) error {
			for _, endpoint := range endpoints {
				if err := s.retireFailedEndpointService(ctx, id, worker, endpoint); err != nil {
					return err
				}
			}
			return nil
		},
		func(ctx context.Context) error { return s.commitDeletion(ctx, id, worker, action, p) },
	}
	for i, step := range steps {
		if err := s.assertLease(ctx, id, worker); err != nil {
			return err
		}
		if err := s.createStep(ctx, id, worker, i, "failed_parent_cleanup", step); err != nil {
			return err
		}
	}
	return nil
}
