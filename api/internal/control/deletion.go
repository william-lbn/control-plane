package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// IDs are the immutable deletion scope. No credential or VM template is stored
// in an Operation: recovery uses the retained, namespace-scoped route Secret.
type deletionPayload struct {
	ProjectID   string   `json:"project_id"`
	BranchIDs   []string `json:"branch_ids"`
	EndpointIDs []string `json:"endpoint_ids"`
	TombstoneID string   `json:"tombstone_id,omitempty"`
}

func lifecycleReadAllowed(r *http.Request) bool {
	p := "/api/v1/projects/" + r.PathValue("project")
	return r.Method == http.MethodGet && (r.URL.Path == p+"/lifecycle" || strings.HasPrefix(r.URL.Path, p+"/operations")) ||
		r.Method == http.MethodPost && (r.URL.Path == p+"/recover" || strings.HasSuffix(r.URL.Path, "/retry")) ||
		r.Method == http.MethodDelete && (r.URL.Path == p || r.PathValue("branch") != "" && r.URL.Path == p+"/branches/"+r.PathValue("branch"))
}

func lifecycleVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v := r.Header.Get("If-Match")
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
		fail(w, r, 428, "version_required", "If-Match must be a quoted resource version")
		return 0, false
	}
	n, err := strconv.ParseInt(v[1:len(v)-1], 10, 64)
	if err != nil || n < 1 {
		fail(w, r, 422, "invalid_version", "Invalid resource version")
		return 0, false
	}
	return n, true
}

func (s *server) lifecycleOverview(w http.ResponseWriter, r *http.Request) {
	project, err := s.one(r.Context(), `SELECT id,name,source,state,protected,version,deleted_at,recover_until,deletion_operation_id FROM projects WHERE id=$1`, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read lifecycle")
		return
	}
	branches, err := s.many(r.Context(), `SELECT b.id,b.name,b.parent_branch_id,b.is_default,b.protected,b.state,b.version,b.deleted_at,
 (SELECT count(*) FROM branches c WHERE c.parent_branch_id=b.id AND c.deleted_at IS NULL) AS child_count,
 (SELECT count(*) FROM endpoints e WHERE e.branch_id=b.id AND e.deleted_at IS NULL) AS endpoint_count
 FROM branches b WHERE b.project_id=$1 ORDER BY b.created_at LIMIT 1000`, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read dependency graph")
		return
	}
	tombstones, err := s.many(r.Context(), `SELECT operation_id,resource_type,resource_id,deleted_at,recover_until,restored_at,physical_gc_state FROM resource_tombstones WHERE project_id=$1 ORDER BY deleted_at DESC LIMIT 100`, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read tombstones")
		return
	}
	ids, err := s.many(r.Context(), `SELECT id FROM operations WHERE project_id=$1 AND action IN ('delete_project','delete_branch','recover_project') ORDER BY created_at DESC LIMIT 100`, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read lifecycle Operations")
		return
	}
	operations := []record{}
	for _, v := range ids {
		op, e := s.operationRecord(r.Context(), stringVal(v["id"]), r.PathValue("project"))
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read lifecycle Operation")
			return
		}
		operations = append(operations, op)
	}
	jsonResponse(w, 200, record{"project": project, "branches": branches, "tombstones": tombstones, "operations": operations, "can_admin": accessFrom(r).Level >= 3, "can_edit": accessFrom(r).Level >= 2, "physical_gc_enabled": false, "distributed_fencing": false, "project_recovery_days": 7})
}

func (s *server) setResourceProtection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Protected   *bool  `json:"protected"`
		ConfirmName string `json:"confirm_name"`
	}
	if readJSON(r, &body) != nil || body.Protected == nil {
		fail(w, r, 422, "invalid_request", "Invalid protection request")
		return
	}
	version, ok := lifecycleVersion(w, r)
	if !ok {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var state string
	err = tx.QueryRow(r.Context(), `SELECT state FROM projects WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, r.PathValue("project")).Scan(&state)
	if err != nil || state != "ready" {
		fail(w, r, 409, "project_not_ready", "Ready project required")
		return
	}
	table, id := "projects", r.PathValue("project")
	if r.PathValue("branch") != "" {
		table, id = "branches", r.PathValue("branch")
	}
	var name string
	var current int64
	query := "SELECT name,version FROM projects WHERE id=$1 AND id=$2 AND deleted_at IS NULL AND state='ready' FOR UPDATE"
	if table == "branches" {
		query = "SELECT name,version FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL AND state='ready' FOR UPDATE"
	}
	err = tx.QueryRow(r.Context(), query, id, r.PathValue("project")).Scan(&name, &current)
	if err != nil {
		fail(w, r, 404, "not_found", "Ready resource not found")
		return
	}
	if current != version {
		fail(w, r, 412, "version_mismatch", "Refresh resource before editing")
		return
	}
	if body.ConfirmName != name {
		fail(w, r, 422, "confirmation_required", "Enter the exact resource name")
		return
	}
	_, err = tx.Exec(r.Context(), "UPDATE "+table+" SET protected=$2,version=version+1 WHERE id=$1", id, body.Protected)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not update protection")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "set_protection", strings.TrimSuffix(table, "s"), id, "succeeded", requestID(r))
	jsonResponse(w, 200, record{"id": id, "protected": body.Protected, "version": current + 1})
}

func (s *server) deleteResource(w http.ResponseWriter, r *http.Request) { s.admitDeletion(w, r, false) }
func (s *server) recoverProject(w http.ResponseWriter, r *http.Request) { s.admitDeletion(w, r, true) }

func (s *server) admitDeletion(w http.ResponseWriter, r *http.Request, recovering bool) {
	if !creationEnabled() || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "lifecycle_disabled", "Managed lifecycle driver disabled")
		return
	}
	var body struct {
		ConfirmName string `json:"confirm_name"`
	}
	if readJSON(r, &body) != nil {
		fail(w, r, 422, "invalid_request", "Invalid lifecycle request")
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
	project, id, kind := r.PathValue("project"), r.PathValue("project"), "project"
	if r.PathValue("branch") != "" {
		id, kind = r.PathValue("branch"), "branch"
	}
	action := "delete_" + kind
	if recovering {
		action = "recover_project"
	}
	hash := s.createRequestHash(action+":"+project+":"+id, record{"body": body, "version": version})
	if s.replayCreate(w, r, key, hash, "", kind) {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var ps, pname, source string
	var pv int64
	var protected bool
	var recoverUntil *time.Time
	var tombstone *string
	err = tx.QueryRow(r.Context(), `SELECT state,name,source,version,protected,recover_until,deletion_operation_id FROM projects WHERE id=$1 FOR UPDATE`, project).Scan(&ps, &pname, &source, &pv, &protected, &recoverUntil, &tombstone)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock project")
		return
	}
	if source != "managed" {
		fail(w, r, 422, "managed_project_required", "Imported resources cannot be deleted by this driver")
		return
	}
	p := deletionPayload{ProjectID: project, BranchIDs: []string{}, EndpointIDs: []string{}}
	if recovering {
		if ps != "deleted" || recoverUntil == nil || !time.Now().Before(*recoverUntil) || tombstone == nil {
			fail(w, r, 409, "recovery_unavailable", "Project recovery requires a completed deletion within seven days")
			return
		}
		var raw []byte
		err = tx.QueryRow(r.Context(), `SELECT snapshot FROM resource_tombstones WHERE operation_id=$1 AND restored_at IS NULL`, *tombstone).Scan(&raw)
		if err != nil || json.Unmarshal(raw, &p) != nil {
			fail(w, r, 503, "metadata_unavailable", "Recovery snapshot unavailable")
			return
		}
		p.TombstoneID = *tombstone
	} else if ps != "ready" {
		fail(w, r, 409, "project_not_ready", "Ready managed project required")
		return
	}
	name, current := pname, pv
	if kind == "branch" {
		var def, bp bool
		var state string
		err = tx.QueryRow(r.Context(), `SELECT name,version,is_default,protected,state FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, id, project).Scan(&name, &current, &def, &bp, &state)
		if err != nil {
			fail(w, r, 404, "not_found", "Branch not found")
			return
		}
		if def {
			fail(w, r, 409, "root_branch_protected", "Root branch cannot be deleted independently")
			return
		}
		if bp {
			fail(w, r, 409, "resource_protected", "Remove branch protection first")
			return
		}
		if state != "ready" {
			fail(w, r, 409, "branch_not_ready", "Ready branch required")
			return
		}
		var children bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM branches WHERE parent_branch_id=$1 AND deleted_at IS NULL)`, id).Scan(&children)
		if err != nil || children {
			fail(w, r, 409, "branch_has_children", "Delete child branches first")
			return
		}
		p.BranchIDs = []string{id}
	} else if !recovering && protected {
		fail(w, r, 409, "resource_protected", "Remove project protection first")
		return
	}
	if current != version {
		fail(w, r, 412, "version_mismatch", "Refresh resource before deleting or recovering")
		return
	}
	if body.ConfirmName != name {
		fail(w, r, 422, "confirmation_required", "Enter the exact resource name")
		return
	}
	if !recovering && kind == "project" {
		rows, e := tx.Query(r.Context(), `SELECT id,protected,state FROM branches WHERE project_id=$1 AND deleted_at IS NULL ORDER BY id FOR UPDATE`, project)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not lock dependency graph")
			return
		}
		items, e := pgx.CollectRows(rows, pgx.RowToMap)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read dependency graph")
			return
		}
		for _, b := range items {
			if b["protected"] == true {
				fail(w, r, 409, "resource_protected", "Project contains protected branches")
				return
			}
			if b["state"] != "ready" {
				fail(w, r, 409, "branch_not_ready", "All live branches must be ready")
				return
			}
			p.BranchIDs = append(p.BranchIDs, stringVal(b["id"]))
		}
	}
	var pending bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait'))`, project).Scan(&pending)
	if err != nil || pending {
		fail(w, r, 409, "operation_conflict", "Wait for active project Operations before changing lifecycle")
		return
	}
	if !recovering {
		var unsupported bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM branch_service_instances WHERE branch_id=ANY($1::text[]) AND service_kind NOT IN ('postgres','data_api','auth') AND desired_state<>'disabled')`, p.BranchIDs).Scan(&unsupported)
		if err != nil || unsupported {
			fail(w, r, 409, "service_retirement_driver_required", "Every enabled branch service must have a verified retirement driver")
			return
		}
		var invalidCompute bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM endpoints WHERE project_id=$1 AND branch_id=ANY($2::text[]) AND deleted_at IS NULL AND (state<>'active' OR workload_kind<>'neonvm'))`, project, p.BranchIDs).Scan(&invalidCompute)
		if err != nil || invalidCompute {
			fail(w, r, 409, "compute_not_ready", "All Computes must be managed and ready before deletion")
			return
		}
		rows, e := tx.Query(r.Context(), `SELECT id FROM endpoints WHERE project_id=$1 AND branch_id=ANY($2::text[]) AND deleted_at IS NULL ORDER BY id FOR UPDATE`, project, p.BranchIDs)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not lock Computes")
			return
		}
		p.EndpointIDs, e = pgx.CollectRows(rows, pgx.RowTo[string])
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read Computes")
			return
		}
	}
	op := newID("op_")
	raw, _ := json.Marshal(p)
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,$3,$4,$5,'queued',$6,$7,$8)`, op, project, kind+"_lifecycle", id, action, userFrom(r).ID, requestID(r), raw)
	steps := []string{"close_proxy_admission", "retire_branch_services", "retire_owned_compute", "persist_retained_tombstone"}
	if recovering {
		steps = []string{"verify_retained_timelines", "commit_project_recovery", "restore_suspended_proxy_routes"}
	}
	for i, step := range steps {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, op, i, step)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, op)
	}
	if err == nil && recovering {
		_, err = tx.Exec(r.Context(), `UPDATE projects SET state='recovering',version=version+1 WHERE id=$1`, project)
	}
	if err == nil && !recovering {
		_, err = tx.Exec(r.Context(), `UPDATE branches SET state='deleting',deletion_operation_id=$2,version=version+1 WHERE id=ANY($1::text[])`, p.BranchIDs, op)
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE endpoints SET state='deleting',deletion_operation_id=$2,version=version+1,updated_at=now() WHERE id=ANY($1::text[])`, p.EndpointIDs, op)
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE data_api_instances SET state='disabling',updated_at=now() WHERE branch_id=ANY($1::text[])`, p.BranchIDs)
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE backend_credentials SET revoked_at=now(),generation=generation+1 WHERE branch_id=ANY($1::text[]) AND revoked_at IS NULL`, p.BranchIDs)
		}
		if err == nil && kind == "project" {
			_, err = tx.Exec(r.Context(), `UPDATE projects SET state='deleting',deletion_operation_id=$2,version=version+1,updated_at=now() WHERE id=$1`, project, op)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.finishCreateError(w, r, tx, key, hash, "", kind, err)
		return
	}
	s.audit(r.Context(), userFrom(r).ID, action, kind, id, "accepted", requestID(r))
	s.acceptCreate(w, r, project, id, op, kind)
}

// Route records remain inside their existing Secret. Closing every route is a
// resourceVersion CAS; only a known 409 is retried. Unknown write outcomes are
// observed by a later explicit Operation retry, never blind transport replay.
func (k *kubeClient) setDeletionRoutes(ctx context.Context, p deletionPayload, operationID string, recovering bool) error {
	wanted := map[string]bool{}
	for _, id := range p.EndpointIDs {
		wanted[selector(id)] = true
	}
	if len(wanted) == 0 {
		return nil
	}
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := k.request(ctx, http.MethodGet, k.path("secret", routesSecret), nil)
		if err != nil {
			return err
		}
		raw, err := secretText(secret, "routes.json")
		if err != nil {
			return err
		}
		var routes map[string]map[string]any
		if err = json.Unmarshal([]byte(raw), &routes); err != nil {
			return err
		}
		for key := range wanted {
			route := routes[key]
			if route == nil || route["project_id"] != p.ProjectID {
				return errors.New("deletion route ownership mismatch")
			}
			branch := stringVal(route["branch_id"])
			found := false
			for _, id := range p.BranchIDs {
				found = found || id == branch
			}
			if !found {
				return errors.New("deletion branch route ownership mismatch")
			}
			expected := operationID
			if recovering {
				expected = p.TombstoneID
			}
			prior := stringVal(route["deletion_operation_id"])
			if prior != "" && prior != expected {
				return errors.New("deletion route generation changed")
			}
			if recovering {
				if prior == "" && route["recovery_operation_id"] == operationID {
					continue
				}
				if prior != expected {
					return errors.New("recovery route tombstone missing")
				}
				route["state"] = "suspended"
				delete(route, "deletion_operation_id")
				route["recovery_operation_id"] = operationID
			} else {
				route["state"] = "deleted"
				route["deletion_operation_id"] = operationID
				if roles, ok := route["roles"].(map[string]any); ok {
					delete(roles, dataAPILogin(branch))
					delete(roles, managedAuthLogin(branch))
				}
			}
		}
		b, err := json.Marshal(routes)
		if err != nil {
			return err
		}
		data, ok := secret["data"].(map[string]any)
		if !ok {
			return errors.New("route Secret data missing")
		}
		data["routes.json"] = base64.StdEncoding.EncodeToString(b)
		_, err = k.request(ctx, http.MethodPut, k.path("secret", routesSecret), secret)
		if kubeStatusIs(err, http.StatusConflict) {
			continue
		}
		return err
	}
	return errors.New("route generation contention")
}

func (s *server) retireDeletedCompute(ctx context.Context, p deletionPayload) error {
	for _, id := range p.EndpointIDs {
		name := kubeName(id)
		path := s.kube.path("vm", name)
		vm, err := s.kube.request(ctx, http.MethodGet, path, nil)
		if kubeStatusIs(err, 404) {
			continue
		}
		if err != nil {
			return err
		}
		if !owned(vm, p.ProjectID, id) || stringVal(nested(vm, "metadata", "uid")) == "" || stringVal(nested(vm, "metadata", "resourceVersion")) == "" {
			return errors.New("deletion VM ownership or generation mismatch")
		}
		uid := stringVal(nested(vm, "metadata", "uid"))
		if nested(vm, "metadata", "deletionTimestamp") == nil {
			_, err = s.kube.request(ctx, http.MethodDelete, path, record{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": record{"uid": uid, "resourceVersion": nested(vm, "metadata", "resourceVersion")}})
			if err != nil && !kubeStatusIs(err, 404) {
				return err
			}
		}
		// Deletion has closed wake admission. Any successor is a failure requiring
		// investigation; suspension's permissive successor observer is inappropriate.
		for {
			vm, err = s.kube.request(ctx, http.MethodGet, path, nil)
			if kubeStatusIs(err, 404) {
				break
			}
			if err != nil {
				return err
			}
			if !owned(vm, p.ProjectID, id) || nested(vm, "metadata", "uid") != uid {
				return errors.New("new VM appeared after deletion admission closed")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

func (s *server) reconcileDeletion(ctx context.Context, id, worker, action string, p deletionPayload) error {
	var durable deletionPayload
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1 AND project_id=$2 AND action=$3`, id, p.ProjectID, action).Scan(&raw); err != nil {
		return err
	}
	if json.Unmarshal(raw, &durable) != nil || s.createRequestHash("delete-scope", durable) != s.createRequestHash("delete-scope", p) {
		return errors.New("deletion scope mismatch")
	}
	recovering := action == "recover_project"
	steps := []func(context.Context) error{
		func(ctx context.Context) error { return s.kube.setDeletionRoutes(ctx, p, id, false) },
		func(ctx context.Context) error {
			items, err := s.many(ctx, `SELECT branch_id,endpoint_id FROM data_api_instances WHERE project_id=$1 AND branch_id=ANY($2::text[])`, p.ProjectID, p.BranchIDs)
			if err != nil {
				return err
			}
			for _, v := range items {
				if err = s.retireDeletionDataAPI(ctx, dataAPIPayload{ProjectID: p.ProjectID, BranchID: stringVal(v["branch_id"]), EndpointID: stringVal(v["endpoint_id"])}); err != nil {
					return err
				}
			}
			authItems, err := s.many(ctx, `SELECT branch_id,endpoint_id FROM managed_auth_instances WHERE project_id=$1 AND branch_id=ANY($2::text[])`, p.ProjectID, p.BranchIDs)
			if err != nil {
				return err
			}
			for _, v := range authItems {
				if err = s.retireDeletionManagedAuth(ctx, managedAuthPayload{ProjectID: p.ProjectID, BranchID: stringVal(v["branch_id"]), EndpointID: stringVal(v["endpoint_id"])}); err != nil {
					return err
				}
			}
			return nil
		},
		func(ctx context.Context) error { return s.retireDeletedCompute(ctx, p) },
		func(ctx context.Context) error { return s.commitDeletion(ctx, id, worker, action, p) },
	}
	if recovering {
		steps = []func(context.Context) error{
			func(ctx context.Context) error {
				var tenant string
				if err := s.db.QueryRow(ctx, "SELECT tenant_id FROM projects WHERE id=$1", p.ProjectID).Scan(&tenant); err != nil {
					return err
				}
				for _, bid := range p.BranchIDs {
					var timeline string
					if err := s.db.QueryRow(ctx, `SELECT timeline_id FROM branches WHERE id=$1 AND project_id=$2 AND
					 (deletion_operation_id=$3 OR (deletion_operation_id IS NULL AND deleted_at IS NULL))`, bid, p.ProjectID, p.TombstoneID).Scan(&timeline); err != nil {
						return err
					}
					if _, err := s.kube.serviceRequest(ctx, "pageserver-managed", 9898, "v1/tenant/"+tenant+"/timeline/"+timeline, http.MethodGet, nil); err != nil {
						return err
					}
				}
				return nil
			},
			func(ctx context.Context) error { return s.commitDeletion(ctx, id, worker, action, p) },
			func(ctx context.Context) error { return s.kube.setDeletionRoutes(ctx, p, id, true) },
		}
	}
	for i, fn := range steps {
		if err := s.assertLease(ctx, id, worker); err != nil {
			return err
		}
		if recovering {
			var valid bool
			err := s.db.QueryRow(ctx, `SELECT
			 (p.state='recovering' AND p.deletion_operation_id=$2 AND t.restored_at IS NULL)
			 OR (p.state='ready' AND p.deletion_operation_id IS NULL AND t.restored_by_operation_id=$3)
			 FROM projects p JOIN resource_tombstones t ON t.project_id=p.id
			 WHERE p.id=$1 AND t.operation_id=$2`, p.ProjectID, p.TombstoneID, id).Scan(&valid)
			if err != nil {
				return err
			}
			if !valid {
				return errors.New("recovery intent generation changed")
			}
		}
		if !recovering {
			var count int
			if err := s.db.QueryRow(ctx, `SELECT count(*) FROM branches WHERE id=ANY($1::text[]) AND project_id=$2 AND deletion_operation_id=$3`, p.BranchIDs, p.ProjectID, id).Scan(&count); err != nil {
				return err
			}
			if count != len(p.BranchIDs) {
				return errors.New("deletion intent generation changed")
			}
		}
		if err := s.createStep(ctx, id, worker, i, "retained_lifecycle", fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) commitDeletion(ctx context.Context, id, worker, action string, p deletionPayload) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return err
	}
	if action == "recover_project" {
		var restored bool
		var restoredBy *string
		if err = tx.QueryRow(ctx, `SELECT restored_at IS NOT NULL,restored_by_operation_id FROM resource_tombstones WHERE operation_id=$1 AND project_id=$2 FOR UPDATE`, p.TombstoneID, p.ProjectID).Scan(&restored, &restoredBy); err != nil {
			return err
		}
		if restored {
			if restoredBy == nil || *restoredBy != id {
				return errors.New("recovery operation generation changed")
			}
			return tx.Commit(ctx)
		}
		// Check quota/name conflicts in the same transaction; failure leaves the
		// project closed and its retained data available for an explicit retry.
		var org string
		if err = tx.QueryRow(ctx, "SELECT org_id FROM projects WHERE id=$1", p.ProjectID).Scan(&org); err != nil {
			return err
		}
		if err = projectQuota(ctx, tx, org); err != nil {
			return err
		}
		var count, limit int
		if err = tx.QueryRow(ctx, `SELECT max_endpoints,(SELECT count(*) FROM endpoints e JOIN projects p ON p.id=e.project_id WHERE p.org_id=$1 AND e.deleted_at IS NULL) FROM organization_quotas WHERE org_id=$1`, org).Scan(&limit, &count); err != nil {
			return err
		}
		if count+len(p.EndpointIDs) > limit {
			return errOrganizationQuota
		}
		tag, e := tx.Exec(ctx, `UPDATE projects SET state='ready',deleted_at=NULL,recover_until=NULL,deletion_operation_id=NULL,version=version+1,updated_at=now() WHERE id=$1 AND deletion_operation_id=$2 AND recover_until>now()`, p.ProjectID, p.TombstoneID)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return errors.New("recovery window or intent changed")
		}
		if _, err = tx.Exec(ctx, `UPDATE branches SET state='ready',deleted_at=NULL,deletion_operation_id=NULL,version=version+1 WHERE id=ANY($1::text[]) AND deletion_operation_id=$2`, p.BranchIDs, p.TombstoneID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE endpoints SET state='active',deleted_at=NULL,deletion_operation_id=NULL,version=version+1,updated_at=now() WHERE id=ANY($1::text[]) AND deletion_operation_id=$2`, p.EndpointIDs, p.TombstoneID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE resource_tombstones SET restored_at=now(),restored_by_operation_id=$2 WHERE operation_id=$1 AND restored_at IS NULL`, p.TombstoneID, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE branch_service_instances SET desired_state='active',observed_state='suspended',version=version+1,last_observed_at=now() WHERE branch_id=ANY($1::text[]) AND service_kind='postgres'`, p.BranchIDs); err != nil {
			return err
		}
	} else {
		snapshot, _ := json.Marshal(p)
		if _, err = tx.Exec(ctx, `INSERT INTO resource_tombstones(operation_id,project_id,resource_type,resource_id,snapshot,recover_until)
   SELECT id,project_id,CASE action WHEN 'delete_project' THEN 'project' ELSE 'branch' END,resource_id,$2,CASE action WHEN 'delete_project' THEN now()+interval '7 days' END FROM operations WHERE id=$1 ON CONFLICT(operation_id) DO NOTHING`, id, snapshot); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE branches SET state='deleted',deleted_at=COALESCE(deleted_at,now()),version=version+CASE WHEN state='deleted' THEN 0 ELSE 1 END WHERE id=ANY($1::text[]) AND deletion_operation_id=$2`, p.BranchIDs, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE endpoints SET state='deleted',deleted_at=COALESCE(deleted_at,now()),version=version+CASE WHEN state='deleted' THEN 0 ELSE 1 END,updated_at=now() WHERE id=ANY($1::text[]) AND deletion_operation_id=$2`, p.EndpointIDs, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE data_api_instances SET state='disabled',updated_at=now() WHERE branch_id=ANY($1::text[])`, p.BranchIDs); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE managed_auth_instances SET state='disabled',updated_at=now() WHERE branch_id=ANY($1::text[])`, p.BranchIDs); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE branch_service_instances SET desired_state='disabled',observed_state='disabled',public_endpoint=NULL,version=version+1,last_observed_at=now() WHERE branch_id=ANY($1::text[])`, p.BranchIDs); err != nil {
			return err
		}
		if action == "delete_project" {
			if _, err = tx.Exec(ctx, `UPDATE projects SET state='deleted',deleted_at=COALESCE(deleted_at,now()),recover_until=(SELECT recover_until FROM resource_tombstones WHERE operation_id=$2),version=version+CASE WHEN state='deleted' THEN 0 ELSE 1 END,updated_at=now() WHERE id=$1 AND deletion_operation_id=$2`, p.ProjectID, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// Preserve the existing Pod template and Secret refs. Constructing a fresh
// empty disable template would discard the service identity and fail Kube
// validation, so deletion changes only the owned Deployment replica intent.
func (s *server) retireDeletionDataAPI(ctx context.Context, p dataAPIPayload) error {
	path := s.kube.path("deployment", dataAPIName(p.BranchID))
	item, err := s.kube.request(ctx, http.MethodGet, path, nil)
	if kubeStatusIs(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedDataAPI(item, p) {
		return errors.New("deletion Data API workload ownership mismatch")
	}
	spec, ok := item["spec"].(map[string]any)
	if !ok {
		return errors.New("Data API spec missing")
	}
	if number(spec["replicas"]) != 0 {
		spec["replicas"] = 0
		if _, err = s.kube.request(ctx, http.MethodPut, path, item); err != nil {
			return err
		}
	}
	for {
		item, err = s.kube.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if !ownedDataAPI(item, p) {
			return errors.New("Data API ownership changed during retirement")
		}
		if number(nested(item, "status", "observedGeneration")) >= number(nested(item, "metadata", "generation")) && number(nested(item, "status", "replicas")) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *server) retireDeletionManagedAuth(ctx context.Context, p managedAuthPayload) error {
	path := s.kube.path("deployment", managedAuthName(p.BranchID))
	item, err := s.kube.request(ctx, http.MethodGet, path, nil)
	if kubeStatusIs(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedManagedAuth(item, p) {
		return errors.New("deletion Managed Auth workload ownership mismatch")
	}
	spec, ok := item["spec"].(map[string]any)
	if !ok {
		return errors.New("Managed Auth spec missing")
	}
	if number(spec["replicas"]) != 0 {
		spec["replicas"] = 0
		if _, err = s.kube.request(ctx, http.MethodPut, path, item); err != nil {
			return err
		}
	}
	for {
		item, err = s.kube.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if !ownedManagedAuth(item, p) {
			return errors.New("Managed Auth ownership changed during retirement")
		}
		if number(nested(item, "status", "observedGeneration")) >= number(nested(item, "metadata", "generation")) && number(nested(item, "status", "replicas")) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
