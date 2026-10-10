package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Independent endpoint retirement never includes a branch in its deletion
// scope. BranchID is an ownership assertion, not permission to delete data.
type endpointDeletionPayload struct {
	ProjectID  string `json:"project_id"`
	BranchID   string `json:"branch_id"`
	EndpointID string `json:"endpoint_id"`
}

func (s *server) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	if !creationEnabled() || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "lifecycle_disabled", "Managed lifecycle driver disabled")
		return
	}
	var body struct {
		ConfirmSelector string `json:"confirm_selector"`
	}
	if readJSON(r, &body) != nil {
		fail(w, r, 422, "invalid_request", "Invalid endpoint deletion request")
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
	p := endpointDeletionPayload{ProjectID: r.PathValue("project"), EndpointID: r.PathValue("endpoint")}
	hash := s.createRequestHash("delete_endpoint:"+p.ProjectID+":"+p.EndpointID, record{"body": body, "version": version})
	// Replay is checked before requiring a live endpoint. A lost 202 reply or
	// completed deletion must return the same Operation, never a second intent.
	if s.replayCreate(w, r, key, hash, "", "endpoint") {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var state, source string
	err = tx.QueryRow(r.Context(), `SELECT state,source FROM projects WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, p.ProjectID).Scan(&state, &source)
	if err != nil || state != "ready" || source != "managed" {
		fail(w, r, 409, "managed_project_required", "Ready managed project required")
		return
	}
	// Acquire parent locks before the endpoint, matching service admission and
	// project retirement. No SQL or Kubernetes mutation occurs in this admission.
	err = tx.QueryRow(r.Context(), `SELECT branch_id FROM endpoints WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, p.EndpointID, p.ProjectID).Scan(&p.BranchID)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Endpoint not found")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read endpoint scope")
		return
	}
	err = tx.QueryRow(r.Context(), `SELECT state FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, p.BranchID, p.ProjectID).Scan(&state)
	if err != nil || state != "ready" {
		fail(w, r, 409, "branch_not_ready", "Ready branch required")
		return
	}
	var selector, workload string
	var current int64
	err = tx.QueryRow(r.Context(), `SELECT selector,state,workload_kind,version FROM endpoints WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL FOR UPDATE`, p.EndpointID, p.ProjectID, p.BranchID).Scan(&selector, &state, &workload, &current)
	if err != nil || state != "active" || workload != "neonvm" {
		fail(w, r, 409, "compute_not_ready", "Only a ready managed endpoint can be deleted")
		return
	}
	if current != version {
		fail(w, r, 412, "version_mismatch", "Refresh endpoint before deleting")
		return
	}
	if body.ConfirmSelector != selector {
		fail(w, r, 422, "confirmation_required", "Enter the exact endpoint selector")
		return
	}
	var pending, dependency bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait'))`, p.ProjectID).Scan(&pending)
	if err != nil || pending {
		fail(w, r, 409, "operation_conflict", "Wait for active project Operations before deleting an endpoint")
		return
	}
	err = tx.QueryRow(r.Context(), `SELECT
	 EXISTS(SELECT 1 FROM data_api_instances WHERE endpoint_id=$1 AND state<>'disabled') OR
	 EXISTS(SELECT 1 FROM managed_auth_instances WHERE endpoint_id=$1 AND state<>'disabled') OR
	 EXISTS(SELECT 1 FROM object_storage_instances WHERE endpoint_id=$1 AND state<>'disabled')`, p.EndpointID).Scan(&dependency)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read endpoint dependencies")
		return
	}
	if dependency {
		fail(w, r, 409, "endpoint_has_services", "Disable dependent Data API, Auth and Object Storage services before deleting their endpoint")
		return
	}
	op := newID("op_")
	raw, _ := json.Marshal(p)
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'endpoint',$3,'delete_endpoint','queued',$4,$5,$6)`, op, p.ProjectID, p.EndpointID, userFrom(r).ID, requestID(r), raw)
	for i, name := range []string{"close_endpoint_proxy_admission", "retire_owned_endpoint_compute", "observe_endpoint_runners_gone", "persist_endpoint_tombstone"} {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, op, i, name)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE endpoints SET state='deleting',deletion_operation_id=$2,scale_to_zero=false,version=version+1,updated_at=now() WHERE id=$1`, p.EndpointID, op)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.finishCreateError(w, r, tx, key, hash, "", "endpoint", err)
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "delete_endpoint", "endpoint", p.EndpointID, "accepted", requestID(r))
	s.acceptCreate(w, r, p.ProjectID, p.EndpointID, op, "endpoint")
}

func (s *server) reconcileEndpointDeletion(ctx context.Context, id, worker string, p endpointDeletionPayload) error {
	var raw []byte
	var state, deletionID string
	err := s.db.QueryRow(ctx, `SELECT o.payload,e.state,e.deletion_operation_id FROM operations o JOIN endpoints e ON e.id=o.resource_id AND e.project_id=o.project_id WHERE o.id=$1 AND o.action='delete_endpoint' AND e.id=$2 AND e.project_id=$3 AND e.branch_id=$4`, id, p.EndpointID, p.ProjectID, p.BranchID).Scan(&raw, &state, &deletionID)
	var durable endpointDeletionPayload
	if err != nil || json.Unmarshal(raw, &durable) != nil || durable != p || deletionID != id || (state != "deleting" && state != "deleted") {
		return errors.New("endpoint deletion scope or generation mismatch")
	}
	scope := deletionPayload{ProjectID: p.ProjectID, BranchIDs: []string{p.BranchID}, EndpointIDs: []string{p.EndpointID}}
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"close_endpoint_proxy_admission", func(ctx context.Context) error { return s.kube.setDeletionRoutes(ctx, scope, id, false) }},
		{"retire_owned_endpoint_compute", func(ctx context.Context) error { return s.retireDeletedCompute(ctx, scope) }},
		{"observe_endpoint_runners_gone", func(ctx context.Context) error { return s.observeDeletedEndpointRunners(ctx, p) }},
		{"persist_endpoint_tombstone", func(ctx context.Context) error { return s.commitEndpointDeletion(ctx, id, worker, p) }},
	}
	for i, step := range steps {
		if err := s.createStep(ctx, id, worker, i, step.name, step.run); err != nil {
			return err
		}
	}
	return nil
}

// Observe normal NeonVM garbage collection; never delete or force a Runner.
// Even a terminal Pod must disappear before this Operation claims retirement.
func (s *server) observeDeletedEndpointRunners(ctx context.Context, p endpointDeletionPayload) error {
	path := s.kube.path("pod", "") + "?labelSelector=" + url.QueryEscape("vm.neon.tech/name="+kubeName(p.EndpointID))
	for {
		list, err := s.kube.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		items, ok := list["items"].([]any)
		if !ok {
			return errors.New("endpoint runner observation invalid")
		}
		for _, item := range items {
			pod, ok := item.(map[string]any)
			if !ok || !owned(pod, p.ProjectID, p.EndpointID) || nested(pod, "metadata", "labels", "vm.neon.tech/name") != kubeName(p.EndpointID) {
				return errors.New("endpoint runner ownership mismatch")
			}
		}
		if len(items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *server) commitEndpointDeletion(ctx context.Context, id, worker string, p endpointDeletionPayload) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE endpoints SET state='deleted',deleted_at=COALESCE(deleted_at,now()),version=version+CASE WHEN state='deleted' THEN 0 ELSE 1 END,updated_at=now() WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deletion_operation_id=$4 AND state IN ('deleting','deleted')`, p.EndpointID, p.ProjectID, p.BranchID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("endpoint deletion intent changed")
	}
	raw, _ := json.Marshal(p)
	if _, err = tx.Exec(ctx, `INSERT INTO resource_tombstones(operation_id,project_id,resource_type,resource_id,snapshot) VALUES($1,$2,'endpoint',$3,$4) ON CONFLICT(operation_id) DO NOTHING`, id, p.ProjectID, p.EndpointID, raw); err != nil {
		return err
	}
	// The postgres capability describes the branch, not this retired endpoint.
	// Recompute only its observation; do not disable or mutate other Backends.
	if _, err = tx.Exec(ctx, `UPDATE branch_service_instances SET observed_state=CASE WHEN EXISTS(SELECT 1 FROM endpoints WHERE branch_id=$1 AND deleted_at IS NULL AND state='active') THEN observed_state ELSE 'suspended' END,last_observed_at=now() WHERE branch_id=$1 AND service_kind='postgres'`, p.BranchID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// A replacement writer must keep the branch SQL identities and probe verifier,
// including when a read replica still serves that timeline. Compute creation
// is not a role-password rotation operation. Old credentials remain retained.
func (s *server) reserveEndpointCredentials(ctx context.Context, p createPayload, password string) error {
	var prior string
	err := s.db.QueryRow(ctx, `SELECT id FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND endpoint_type='read_write' AND state='deleted' AND deleted_at IS NOT NULL ORDER BY created_at DESC,id DESC LIMIT 1`, p.ProjectID, p.BranchID).Scan(&prior)
	if isNoRows(err) {
		return s.kube.reserveCredentials(ctx, p.ProjectID, p.EndpointID, password)
	}
	if err != nil {
		return err
	}
	return s.kube.reserveRetainedWriterCredentials(ctx, p, prior, password)
}

func (k *kubeClient) reserveRetainedWriterCredentials(ctx context.Context, p createPayload, prior, password string) error {
	secret, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(prior)+"-credentials"), nil)
	if err != nil || !owned(secret, p.ProjectID, prior) {
		return errors.New("retained branch credential unavailable or unowned")
	}
	verifier, err := secretText(secret, "adminVerifier")
	if err != nil {
		return err
	}
	if !scramMatches(password, verifier) {
		return errBranchPasswordMismatch
	}
	return k.reserveReplicaCredentials(ctx, p.ProjectID, p.EndpointID, prior)
}

var errBranchPasswordMismatch = errors.New("replacement writer requires the existing branch SQL password")
