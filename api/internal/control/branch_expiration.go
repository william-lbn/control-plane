package control

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func parseBranchExpiration(raw json.RawMessage, now time.Time) (*time.Time, *int, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("expires_at is required; null clears it")
	}
	if string(raw) == "null" {
		return nil, nil, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return nil, nil, errors.New("RFC3339 timestamp required")
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, nil, errors.New("RFC3339 timezone required")
	}
	deadline = deadline.UTC().Truncate(time.Second)
	if !deadline.After(now) || deadline.After(now.Add(30*24*time.Hour)) {
		return nil, nil, errors.New("future expiration within thirty days required")
	}
	interval := int(math.Ceil(deadline.Sub(now).Seconds()))
	return &deadline, &interval, nil
}

func (s *server) setBranchExpiration(w http.ResponseWriter, r *http.Request) {
	if !creationEnabled() || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "lifecycle_disabled", "Managed lifecycle driver disabled")
		return
	}
	var body struct {
		ExpiresAt   json.RawMessage `json:"expires_at"`
		ConfirmName string          `json:"confirm_name"`
	}
	if readJSON(r, &body) != nil || len(body.ExpiresAt) == 0 {
		fail(w, r, 422, "invalid_expiration", "Explicit expires_at timestamp or null required")
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
	project, branch := r.PathValue("project"), r.PathValue("branch")
	hash := s.createRequestHash("branch-expiration:"+project+":"+branch, record{"body": body, "version": version})
	if s.replayCreate(w, r, key, hash, "", "branch") {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var ready bool
	err = tx.QueryRow(r.Context(), `SELECT state='ready' AND source='managed' AND deleted_at IS NULL FROM projects WHERE id=$1 FOR UPDATE`, project).Scan(&ready)
	if err != nil || !ready {
		fail(w, r, 409, "managed_project_required", "Ready managed project required")
		return
	}
	var name, state string
	var current, generation int64
	var root, protected bool
	var previous *time.Time
	err = tx.QueryRow(r.Context(), `SELECT name,state,version,is_default,protected,expires_at,expiration_generation FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, branch, project).Scan(&name, &state, &current, &root, &protected, &previous, &generation)
	if err != nil {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	if state != "ready" || root || protected {
		fail(w, r, 409, "expiration_not_allowed", "Ready unprotected non-default leaf branch required")
		return
	}
	if version != current {
		fail(w, r, 412, "version_mismatch", "Refresh branch before updating expiration")
		return
	}
	if body.ConfirmName != name {
		fail(w, r, 422, "confirmation_required", "Enter the exact branch name")
		return
	}
	var now time.Time
	if err = tx.QueryRow(r.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read expiration clock")
		return
	}
	if previous != nil && !previous.After(now) {
		fail(w, r, 409, "branch_expired", "An elapsed expiration cannot be cancelled or extended")
		return
	}
	deadline, interval, err := parseBranchExpiration(body.ExpiresAt, now)
	if err != nil {
		fail(w, r, 422, "invalid_expiration", "Use a future RFC3339 timestamp within thirty days, or explicit null")
		return
	}
	var held bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait')) OR EXISTS(SELECT 1 FROM branches WHERE parent_branch_id=$2 AND deleted_at IS NULL)`, project, branch).Scan(&held)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read branch dependencies")
		return
	}
	if held {
		fail(w, r, 409, "expiration_dependencies_held", "Wait for active Operations and remove descendants first")
		return
	}
	if deadline != nil {
		reason, err := expirationDependencyReason(r.Context(), tx, project, branch)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read expiration dependencies")
			return
		}
		if reason != "" {
			fail(w, r, 409, reason, "Retire unsupported Backends and finish Compute creation before setting expiration")
			return
		}
	}
	op := newID("op_")
	generation++
	payload, _ := json.Marshal(record{"project_id": project, "branch_id": branch, "expires_at": deadline, "expiration_generation": generation})
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,finished_at) VALUES($1,$2,'branch_lifecycle',$3,'set_branch_expiration','succeeded',$4,$5,$6,now())`, op, project, branch, userFrom(r).ID, requestID(r), payload)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state,attempts) VALUES($1,0,'persist_branch_expiration','succeeded',1)`, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, op)
	}
	status := "none"
	if deadline != nil {
		status = "scheduled"
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE branches SET expires_at=$2,ttl_interval_seconds=$3,expiration_generation=$4,expiration_actor_id=$5,expiration_operation_id=$6,expiration_status=$7,expiration_error_code=NULL,expiration_last_checked_at=NULL,version=version+1 WHERE id=$1`, branch, deadline, interval, generation, userFrom(r).ID, op, status)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.finishCreateError(w, r, tx, key, hash, "", "branch", err)
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "set_branch_expiration", "branch", branch, "succeeded", requestID(r))
	s.acceptCreate(w, r, project, branch, op, "branch")
}

func expirationDependencyReason(ctx context.Context, tx pgx.Tx, project, branch string) (string, error) {
	var functions, dataAPI, unknown, compute bool
	err := tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND state<>'deleted') OR EXISTS(SELECT 1 FROM function_instances WHERE project_id=$1 AND branch_id=$2 AND state<>'retired'),
 EXISTS(SELECT 1 FROM data_api_instances WHERE branch_id=$2 AND state<>'disabled'),
 EXISTS(SELECT 1 FROM branch_service_instances WHERE branch_id=$2 AND service_kind NOT IN ('postgres','auth','object_storage') AND desired_state<>'disabled'),
 EXISTS(SELECT 1 FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND deleted_at IS NULL AND (state<>'active' OR workload_kind<>'neonvm'))`, project, branch).Scan(&functions, &dataAPI, &unknown, &compute)
	if err != nil {
		return "", err
	}
	if functions {
		return "functions_retirement_required", nil
	}
	if dataAPI {
		return "data_api_retirement_required", nil
	}
	if unknown {
		return "service_retirement_driver_required", nil
	}
	if compute {
		return "compute_not_ready", nil
	}
	return "", nil
}

func (s *server) runBranchExpirationController(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := s.enqueueDueBranchExpirations(cycle)
			cancel()
			if err != nil && s.logger != nil {
				s.logger.Warn("branch expiration admission unavailable")
			}
		}
	}
}

func (s *server) enqueueDueBranchExpirations(ctx context.Context) error {
	items, err := s.many(ctx, `SELECT b.id,b.project_id FROM branches b JOIN projects p ON p.id=b.project_id WHERE b.state='ready' AND b.deleted_at IS NULL AND b.expires_at<=clock_timestamp() AND p.state='ready' AND p.source='managed' ORDER BY b.expiration_last_checked_at NULLS FIRST,b.expires_at,b.id LIMIT 16`)
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range items {
		if err := s.enqueueBranchExpiration(ctx, stringVal(item["project_id"]), stringVal(item["id"])); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *server) enqueueBranchExpiration(ctx context.Context, project, branch string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var projectState string
	err = tx.QueryRow(ctx, `SELECT state FROM projects WHERE id=$1 AND source='managed' AND deleted_at IS NULL FOR UPDATE SKIP LOCKED`, project).Scan(&projectState)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if projectState != "ready" {
		return nil
	}
	var generation int64
	var policy string
	var permitted bool
	err = tx.QueryRow(ctx, `SELECT expiration_generation,expiration_operation_id,NOT protected AND NOT is_default FROM branches WHERE id=$1 AND project_id=$2 AND state='ready' AND deleted_at IS NULL AND expires_at<=clock_timestamp() FOR UPDATE SKIP LOCKED`, branch, project).Scan(&generation, &policy, &permitted)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	block := func(reason string) error {
		_, err := tx.Exec(ctx, `UPDATE branches SET expiration_status='blocked',expiration_error_code=$2,expiration_last_checked_at=clock_timestamp() WHERE id=$1`, branch, reason)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if !permitted {
		return block("resource_protected")
	}
	var original bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations o JOIN branches b ON b.id=$3 AND b.project_id=o.project_id WHERE o.id=$1 AND o.project_id=$2 AND o.resource_type='branch_lifecycle' AND o.resource_id=b.id AND o.action='set_branch_expiration' AND o.state='succeeded' AND o.payload->>'expiration_generation'=b.expiration_generation::text AND (o.payload->>'expires_at')::timestamptz=b.expires_at)`, policy, project, branch).Scan(&original)
	if err != nil {
		return err
	}
	if !original {
		return block("expiration_policy_mismatch")
	}
	var held bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait')) OR EXISTS(SELECT 1 FROM branches WHERE parent_branch_id=$2 AND deleted_at IS NULL)`, project, branch).Scan(&held)
	if err != nil {
		return err
	}
	if held {
		return block("operation_or_child_conflict")
	}
	reason, err := expirationDependencyReason(ctx, tx, project, branch)
	if err != nil {
		return err
	}
	if reason != "" {
		return block(reason)
	}
	rows, err := tx.Query(ctx, `SELECT id FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND deleted_at IS NULL ORDER BY id FOR UPDATE`, project, branch)
	if err != nil {
		return err
	}
	endpoints, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	p := deletionPayload{ProjectID: project, BranchIDs: []string{branch}, EndpointIDs: endpoints, ExpirationGeneration: generation, ExpectedVMUIDs: map[string]string{}, ExpectedServiceUIDs: map[string]string{}}
	for _, endpoint := range endpoints {
		name := kubeName(endpoint)
		for _, resource := range []string{"vm", "service"} {
			value, e := s.kube.request(ctx, http.MethodGet, s.kube.path(resource, name), nil)
			uid := ""
			if !kubeStatusIs(e, 404) {
				if e != nil {
					return block("resource_observation_unavailable")
				}
				uid = stringVal(nested(value, "metadata", "uid"))
				if uid == "" || stringVal(nested(value, "metadata", "resourceVersion")) == "" || !owned(value, project, endpoint) {
					return block("resource_ownership_mismatch")
				}
				if resource == "service" && !failedEndpointServiceOwned(value, endpointDeletionPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint}) {
					return block("resource_ownership_mismatch")
				}
			}
			if resource == "vm" {
				p.ExpectedVMUIDs[endpoint] = uid
			} else {
				p.ExpectedServiceUIDs[endpoint] = uid
			}
		}
	}
	op := newID("op_")
	payload, _ := json.Marshal(p)
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'branch_lifecycle',$3,'expire_branch','queued','system:branch-expiration',$4,$5)`, op, project, branch, newID("req_"), payload)
	for i, name := range []string{"close_expired_branch_admission", "retire_expired_branch_backends", "retire_expired_branch_compute", "observe_expired_branch_runners_gone", "retire_expired_endpoint_services", "persist_expired_branch_tombstone"} {
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, op, i, name)
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE branches SET state='deleting',deletion_operation_id=$2,expiration_operation_id=$2,expiration_status='retiring',expiration_error_code=NULL,expiration_last_checked_at=clock_timestamp(),version=version+1 WHERE id=$1`, branch, op)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE endpoints SET state='deleting',deletion_operation_id=$2,scale_to_zero=false,version=version+1,updated_at=now() WHERE id=ANY($1::text[])`, endpoints, op)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE backend_credentials SET revoked_at=now(),generation=generation+1 WHERE branch_id=$1 AND revoked_at IS NULL`, branch)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *server) reconcileBranchExpiration(ctx context.Context, id, worker string, p deletionPayload) error {
	if len(p.BranchIDs) != 1 || p.ExpirationGeneration < 1 || p.FailedCreateOperationID != "" || p.TombstoneID != "" || len(p.ExpectedVMUIDs) != len(p.EndpointIDs) || len(p.ExpectedServiceUIDs) != len(p.EndpointIDs) {
		return errors.New("invalid expiration graph")
	}
	var valid bool
	err := s.db.QueryRow(ctx, `SELECT b.deletion_operation_id=$3 AND b.expiration_operation_id=$3 AND b.expiration_generation=$4 AND b.expires_at<=clock_timestamp() AND b.state IN ('deleting','deleted') AND p.state='ready' AND NOT b.protected AND NOT b.is_default FROM branches b JOIN projects p ON p.id=b.project_id WHERE p.id=$1 AND b.id=$2`, p.ProjectID, p.BranchIDs[0], id, p.ExpirationGeneration).Scan(&valid)
	if err != nil || !valid {
		return errors.New("original expiration intent changed")
	}
	for _, endpoint := range p.EndpointIDs {
		if _, ok := p.ExpectedVMUIDs[endpoint]; !ok {
			return errors.New("original VM witness missing")
		}
		if _, ok := p.ExpectedServiceUIDs[endpoint]; !ok {
			return errors.New("original Service witness missing")
		}
	}
	steps := []func(context.Context) error{
		func(ctx context.Context) error { return s.kube.setDeletionRoutes(ctx, p, id, false) },
		func(ctx context.Context) error {
			items, err := s.many(ctx, `SELECT branch_id,endpoint_id FROM managed_auth_instances WHERE project_id=$1 AND branch_id=ANY($2::text[])`, p.ProjectID, p.BranchIDs)
			if err != nil {
				return err
			}
			for _, item := range items {
				if err = s.retireDeletionManagedAuth(ctx, managedAuthPayload{ProjectID: p.ProjectID, BranchID: stringVal(item["branch_id"]), EndpointID: stringVal(item["endpoint_id"])}); err != nil {
					return err
				}
			}
			return nil
		},
		func(ctx context.Context) error { return s.retireDeletedComputeBound(ctx, p, p.ExpectedVMUIDs) },
		func(ctx context.Context) error {
			for _, endpoint := range p.EndpointIDs {
				if err := s.observeDeletedEndpointRunners(ctx, endpointDeletionPayload{ProjectID: p.ProjectID, BranchID: p.BranchIDs[0], EndpointID: endpoint}); err != nil {
					return err
				}
			}
			return nil
		},
		func(ctx context.Context) error {
			for _, endpoint := range p.EndpointIDs {
				if err := s.retireExpiredEndpointService(ctx, p.ProjectID, endpoint, p.ExpectedServiceUIDs[endpoint]); err != nil {
					return err
				}
			}
			return nil
		},
		func(ctx context.Context) error { return s.commitDeletion(ctx, id, worker, "expire_branch", p) },
	}
	for i, step := range steps {
		if err := s.createStep(ctx, id, worker, i, "branch_expiration", step); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) retireExpiredEndpointService(ctx context.Context, project, endpoint, uid string) error {
	path := s.kube.path("service", kubeName(endpoint))
	scope := endpointDeletionPayload{ProjectID: project, EndpointID: endpoint}
	for {
		value, err := s.kube.request(ctx, http.MethodGet, path, nil)
		if kubeStatusIs(err, 404) {
			return nil
		}
		if err != nil {
			return err
		}
		if !failedEndpointServiceOwned(value, scope) || nested(value, "metadata", "uid") != uid {
			return errors.New("expiration Service identity changed")
		}
		if nested(value, "metadata", "deletionTimestamp") == nil {
			_, err = s.kube.request(ctx, http.MethodDelete, path, record{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": record{"uid": uid, "resourceVersion": nested(value, "metadata", "resourceVersion")}})
			if err != nil && !kubeStatusIs(err, 404) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Expired branch metadata remains readable. Work that could wake or mutate its
// runtime is denied while the background retirement Operation is pending.
func (s *server) branchExpirationRouteAllowed(w http.ResponseWriter, r *http.Request) bool {
	branch, endpoint := r.PathValue("branch"), r.PathValue("endpoint")
	if branch == "" && endpoint == "" {
		return true
	}
	if ((r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.Contains(r.URL.Path, "/roles") && !strings.Contains(r.URL.Path, "/databases")) || r.Method == http.MethodDelete || strings.HasSuffix(r.URL.Path, "/expiration") {
		return true
	}
	var deadline *time.Time
	query := `SELECT expires_at FROM branches WHERE id=$1 AND project_id=$2`
	target := branch
	if endpoint != "" {
		query = `SELECT b.expires_at FROM branches b JOIN endpoints e ON e.branch_id=b.id WHERE e.id=$1 AND e.project_id=$2`
		target = endpoint
	}
	err := s.db.QueryRow(r.Context(), query, target, r.PathValue("project")).Scan(&deadline)
	if isNoRows(err) {
		return true
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read branch expiration")
		return false
	}
	if deadline == nil {
		return true
	}
	if !deadline.After(time.Now()) || strings.Contains(r.URL.Path, "/functions") || strings.HasSuffix(r.URL.Path, "/data-api") {
		fail(w, r, 409, "branch_expiring", "This work is unavailable for an expiring branch")
		return false
	}
	return true
}
