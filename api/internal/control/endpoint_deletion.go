package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// Independent endpoint retirement never includes a branch in its deletion
// scope. BranchID is an ownership assertion, not permission to delete data.
type endpointDeletionPayload struct {
	ProjectID               string `json:"project_id"`
	BranchID                string `json:"branch_id"`
	EndpointID              string `json:"endpoint_id"`
	FailedCreateOperationID string `json:"failed_create_operation_id,omitempty"`
}

type failedEndpointCreationReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func verifyFailedEndpointCreation(ctx context.Context, reader failedEndpointCreationReader, p endpointDeletionPayload) error {
	var raw []byte
	var terminal bool
	err := reader.QueryRow(ctx, `SELECT payload,state='failed' AND finished_at IS NOT NULL AND lease_owner IS NULL AND lease_expires_at IS NULL
 AND (SELECT count(*) FROM operations WHERE project_id=$2 AND resource_type='endpoint' AND resource_id=$3 AND action='create_endpoint')=1
 FROM operations WHERE id=$1 AND project_id=$2 AND resource_type='endpoint' AND resource_id=$3 AND action='create_endpoint'`, p.FailedCreateOperationID, p.ProjectID, p.EndpointID).Scan(&raw, &terminal)
	var original createPayload
	if err != nil || !terminal || json.Unmarshal(raw, &original) != nil || original.ProjectID != p.ProjectID || original.BranchID != p.BranchID || original.EndpointID != p.EndpointID {
		return errors.New("original failed endpoint creation identity or terminal receipt mismatch")
	}
	return nil
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
	if err = checkDefaultOwnerReservation(r.Context(), tx, p.ProjectID, p.BranchID); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	var selector, workload string
	var current int64
	err = tx.QueryRow(r.Context(), `SELECT selector,state,workload_kind,version FROM endpoints WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL FOR UPDATE`, p.EndpointID, p.ProjectID, p.BranchID).Scan(&selector, &state, &workload, &current)
	if err != nil || (state != "active" && state != "error") || workload != "neonvm" {
		fail(w, r, 409, "compute_not_ready", "Ready Compute or verified failed independent creation required")
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
	if state == "error" {
		// Parent/endpoint locks serialize this read with retry admission. Do not
		// lock the original Operation here: retry already owns its row before
		// acquiring these parent locks, which would invert the lock order.
		rows, e := tx.Query(r.Context(), `SELECT id,payload FROM operations WHERE project_id=$1 AND resource_type='endpoint' AND resource_id=$2 AND action='create_endpoint'`, p.ProjectID, p.EndpointID)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not verify original creation")
			return
		}
		origins, e := pgx.CollectRows(rows, pgx.RowToMap)
		if e != nil || len(origins) != 1 {
			fail(w, r, 409, "failed_creation_required", "Unique original failed independent creation required")
			return
		}
		p.FailedCreateOperationID = stringVal(origins[0]["id"])
		if e = verifyFailedEndpointCreation(r.Context(), tx, p); e != nil {
			fail(w, r, 409, "failed_creation_required", "Original failed creation does not match this Compute")
			return
		}
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
	 EXISTS(SELECT 1 FROM object_storage_instances WHERE endpoint_id=$1 AND state<>'disabled') OR
	 EXISTS(SELECT 1 FROM function_definitions WHERE endpoint_id=$1 AND state<>'deleted') OR
	 EXISTS(SELECT 1 FROM function_instances i JOIN function_definitions f ON f.id=i.function_id
	  WHERE f.endpoint_id=$1 AND i.state<>'retired')`, p.EndpointID).Scan(&dependency)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read endpoint dependencies")
		return
	}
	if dependency {
		fail(w, r, 409, "endpoint_has_services", "Retire dependent Data API, Auth, Object Storage and Functions before deleting their endpoint")
		return
	}
	op := newID("op_")
	action := "delete_endpoint"
	if p.FailedCreateOperationID != "" {
		action = "cleanup_failed_endpoint"
	}
	raw, _ := json.Marshal(p)
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'endpoint',$3,$4,'queued',$5,$6,$7)`, op, p.ProjectID, p.EndpointID, action, userFrom(r).ID, requestID(r), raw)
	names := []string{"close_endpoint_proxy_admission", "retire_owned_endpoint_compute", "observe_endpoint_runners_gone"}
	if p.FailedCreateOperationID != "" {
		names = append(names, "retire_owned_endpoint_service")
	}
	names = append(names, "persist_endpoint_tombstone")
	for i, name := range names {
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
	s.audit(r.Context(), userFrom(r).ID, action, "endpoint", p.EndpointID, "accepted", requestID(r))
	s.acceptCreate(w, r, p.ProjectID, p.EndpointID, op, "endpoint")
}

func (s *server) reconcileEndpointDeletion(ctx context.Context, id, worker string, p endpointDeletionPayload) error {
	var raw []byte
	var state, deletionID string
	action := "delete_endpoint"
	if p.FailedCreateOperationID != "" {
		action = "cleanup_failed_endpoint"
	}
	err := s.db.QueryRow(ctx, `SELECT o.payload,e.state,e.deletion_operation_id FROM operations o JOIN endpoints e ON e.id=o.resource_id AND e.project_id=o.project_id WHERE o.id=$1 AND o.action=$5 AND e.id=$2 AND e.project_id=$3 AND e.branch_id=$4`, id, p.EndpointID, p.ProjectID, p.BranchID, action).Scan(&raw, &state, &deletionID)
	var durable endpointDeletionPayload
	if err != nil || json.Unmarshal(raw, &durable) != nil || durable != p || deletionID != id || (state != "deleting" && state != "deleted") {
		return errors.New("endpoint deletion scope or generation mismatch")
	}
	if p.FailedCreateOperationID != "" {
		if err := verifyFailedEndpointCreation(ctx, s.db, p); err != nil {
			return err
		}
	}
	scope := deletionPayload{ProjectID: p.ProjectID, BranchIDs: []string{p.BranchID}, EndpointIDs: []string{p.EndpointID}}
	type retirementStep struct {
		name string
		run  func(context.Context) error
	}
	steps := []retirementStep{
		{"close_endpoint_proxy_admission", func(ctx context.Context) error {
			if p.FailedCreateOperationID != "" {
				return s.kube.setDeletionRoutesAllowUnpublished(ctx, scope, id, false, map[string]bool{selector(p.EndpointID): true})
			}
			return s.kube.setDeletionRoutes(ctx, scope, id, false)
		}},
		{"retire_owned_endpoint_compute", func(ctx context.Context) error {
			if p.FailedCreateOperationID != "" {
				return s.retireFailedEndpointCompute(ctx, id, worker, p, scope)
			}
			return s.retireDeletedCompute(ctx, scope)
		}},
		{"observe_endpoint_runners_gone", func(ctx context.Context) error { return s.observeDeletedEndpointRunners(ctx, p) }},
	}
	if p.FailedCreateOperationID != "" {
		steps = append(steps, retirementStep{"retire_owned_endpoint_service", func(ctx context.Context) error {
			return s.retireFailedEndpointService(ctx, id, worker, p)
		}})
	}
	steps = append(steps, retirementStep{"persist_endpoint_tombstone", func(ctx context.Context) error { return s.commitEndpointDeletion(ctx, id, worker, p) }})
	for i, step := range steps {
		if err := s.createStep(ctx, id, worker, i, step.name, step.run); err != nil {
			return err
		}
	}
	return nil
}

// Pin the first owned VM identity (or verified absence) before issuing DELETE.
// A later explicit retry must never adopt a replacement under the same name.
func (s *server) retireFailedEndpointCompute(ctx context.Context, operation, worker string, p endpointDeletionPayload, scope deletionPayload) error {
	name := kubeName(p.EndpointID)
	vm, err := s.kube.request(ctx, http.MethodGet, s.kube.path("vm", name), nil)
	uid := ""
	if !kubeStatusIs(err, http.StatusNotFound) {
		if err != nil {
			return err
		}
		uid = stringVal(nested(vm, "metadata", "uid"))
		if !owned(vm, p.ProjectID, p.EndpointID) || uid == "" || stringVal(nested(vm, "metadata", "resourceVersion")) == "" {
			return errors.New("failed creation VM identity or ownership mismatch")
		}
	}
	expected, err := s.bindFailedEndpointResource(ctx, operation, worker, p, "vm", uid)
	if err != nil {
		return err
	}
	// Capture every ephemeral identity before the first destructive request.
	// A Service replacement during a failed VM delete is not adoptable later.
	if _, err = s.observeFailedEndpointService(ctx, operation, worker, p); err != nil {
		return err
	}
	return s.retireDeletedComputeBound(ctx, scope, map[string]string{p.EndpointID: expected})
}

func (s *server) bindFailedEndpointResource(ctx context.Context, operation, worker string, p endpointDeletionPayload, kind, uid string) (string, error) {
	name := kubeName(p.EndpointID)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO failed_endpoint_resource_bindings(operation_id,resource_kind,project_id,endpoint_id,namespace,resource_name,observed_uid)
 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(operation_id,resource_kind) DO NOTHING`, operation, kind, p.ProjectID, p.EndpointID, s.kube.namespace, name, uid)
	if err != nil {
		return "", err
	}
	var expected string
	err = tx.QueryRow(ctx, `SELECT observed_uid FROM failed_endpoint_resource_bindings WHERE operation_id=$1 AND resource_kind=$2 AND project_id=$3 AND endpoint_id=$4 AND namespace=$5 AND resource_name=$6`, operation, kind, p.ProjectID, p.EndpointID, s.kube.namespace, name).Scan(&expected)
	if err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err // Unknown commit: no Kubernetes delete; explicit retry observes it.
	}
	return expected, nil
}

func failedEndpointServiceOwned(item map[string]any, p endpointDeletionPayload) bool {
	name := kubeName(p.EndpointID)
	return owned(item, p.ProjectID, p.EndpointID) && stringVal(nested(item, "metadata", "uid")) != "" && stringVal(nested(item, "metadata", "resourceVersion")) != "" && nested(item, "spec", "selector", "vm.neon.tech/name") == name && (nested(item, "spec", "type") == nil || nested(item, "spec", "type") == "ClusterIP")
}

func (s *server) observeFailedEndpointService(ctx context.Context, operation, worker string, p endpointDeletionPayload) (string, error) {
	service, err := s.kube.request(ctx, http.MethodGet, s.kube.path("service", kubeName(p.EndpointID)), nil)
	uid := ""
	if !kubeStatusIs(err, http.StatusNotFound) {
		if err != nil {
			return "", err
		}
		if !failedEndpointServiceOwned(service, p) {
			return "", errors.New("failed creation Service ownership or selector mismatch")
		}
		uid = stringVal(nested(service, "metadata", "uid"))
	}
	expected, err := s.bindFailedEndpointResource(ctx, operation, worker, p, "service", uid)
	if err != nil {
		return "", err
	}
	if uid != "" && uid != expected {
		return "", errors.New("Service appeared or was replaced after original retirement observation")
	}
	return expected, nil
}

func (s *server) retireFailedEndpointService(ctx context.Context, operation, worker string, p endpointDeletionPayload) error {
	path := s.kube.path("service", kubeName(p.EndpointID))
	expected, err := s.observeFailedEndpointService(ctx, operation, worker, p)
	if err != nil {
		return err
	}
	for {
		service, err := s.kube.request(ctx, http.MethodGet, path, nil)
		if kubeStatusIs(err, http.StatusNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !failedEndpointServiceOwned(service, p) || nested(service, "metadata", "uid") != expected {
			return errors.New("Service appeared or was replaced after original retirement observation")
		}
		if nested(service, "metadata", "deletionTimestamp") == nil {
			_, err = s.kube.request(ctx, http.MethodDelete, path, record{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": record{"uid": expected, "resourceVersion": nested(service, "metadata", "resourceVersion")}})
			if err != nil && !kubeStatusIs(err, http.StatusNotFound) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
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
