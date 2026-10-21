package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func (s *server) attachFunctionTriggerAttempt(ctx context.Context, run functionTriggerRun, event string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE function_trigger_invocations SET request_event_id=COALESCE(request_event_id,$3) WHERE id=$1 AND state='dispatching' AND lease_owner=$2 AND lease_expires_at>clock_timestamp()`, run.ID, run.Owner, event)
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("trigger dispatch lease changed")
	}
	if run.ObjectEvent != "" {
		_, err = tx.Exec(ctx, `INSERT INTO function_trigger_delivery_attempts(invocation_id,project_id,branch_id,attempt,request_event_id) VALUES($1,$2,$3,$4,$5)`, run.ID, run.Project, run.Branch, run.Attempts, event)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// A manual redrive appends a delivery receipt. The original terminal failure,
// its attempts, source object and logical invocation ID are never overwritten.
func (s *server) retryFunctionObjectEvent(w http.ResponseWriter, r *http.Request) {
	project, branch, trigger, target, actor := r.PathValue("project"), r.PathValue("branch"), r.PathValue("trigger"), r.PathValue("invocation"), userFrom(r).ID
	if !functionTriggerID.MatchString(trigger) || !functions.ValidTriggerInvocationID(target) {
		fail(w, r, 404, "not_found", "Object invocation not found")
		return
	}
	if r.Header.Get("If-Match") != "\""+target+"\"" {
		fail(w, r, 428, "version_required", "Exact immutable invocation ETag required")
		return
	}
	var input struct {
		Acknowledge bool `json:"acknowledge_duplicate_side_effects"`
	}
	if readJSON(r, &input) != nil || !input.Acknowledge {
		fail(w, r, 422, "delivery_acknowledgment_required", "Acknowledge possible duplicate side effects; use invocation_id deduplication")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "idempotency_unavailable", "Durable admission unavailable")
		return
	}
	hash := s.createRequestHash("object-event-redrive", record{"project": project, "branch": branch, "trigger": trigger, "target": target, "acknowledge": true})
	keyHash := s.createRequestHash("object-event-redrive-key", key)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Redrive transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var ready bool
	err = tx.QueryRow(r.Context(), `SELECT true FROM branches b JOIN projects p ON p.id=b.project_id WHERE b.id=$1 AND p.id=$2 AND b.state='ready' AND b.deleted_at IS NULL AND p.state='ready' AND p.deleted_at IS NULL FOR UPDATE OF p,b`, branch, project).Scan(&ready)
	if err != nil || !ready {
		fail(w, r, 409, "branch_not_ready", "Ready project and branch required")
		return
	}
	_, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "function-object-redrive:"+actor+":"+keyHash)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Redrive lock unavailable")
		return
	}
	var previous string
	var receipt []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM function_trigger_retry_requests WHERE actor_id=$1 AND project_id=$2 AND branch_id=$3 AND key_hash=$4`, actor, project, branch, keyHash).Scan(&previous, &receipt)
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key belongs to a different redrive")
			return
		}
		var result record
		if json.Unmarshal(receipt, &result) != nil {
			fail(w, r, 503, "metadata_unavailable", "Invalid retained receipt")
			return
		}
		jsonResponse(w, 202, result)
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Redrive receipt unavailable")
		return
	}
	if !s.functionObjectEventsEnabled() {
		fail(w, r, 503, "function_object_events_disabled", "Operator must enable event dispatch")
		return
	}
	var root, state string
	err = tx.QueryRow(r.Context(), `SELECT COALESCE(parent_invocation_id,id),state FROM function_trigger_invocations WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND trigger_id=$4 AND object_event_id IS NOT NULL`, target, project, branch, trigger).Scan(&root, &state)
	if err != nil {
		fail(w, r, 404, "not_found", "Object invocation not found")
		return
	}
	if state != "failed" && state != "outcome_unknown" {
		fail(w, r, 409, "invocation_not_retryable", "Only a terminal failed or unknown delivery may be redriven")
		return
	}
	run := functionTriggerRun{ID: root, Project: project, Branch: branch, Trigger: trigger}
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT function_id,actor_id,trigger_generation,scheduled_at,snapshot,object_event_id,state FROM function_trigger_invocations WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND trigger_id=$4 AND parent_invocation_id IS NULL FOR UPDATE`, root, project, branch, trigger).Scan(&run.Function, &run.Actor, &run.Generation, &run.Scheduled, &raw, &run.ObjectEvent, &state)
	if err != nil || (state != "failed" && state != "outcome_unknown") || json.Unmarshal(raw, &run.Snapshot) != nil || run.Snapshot.Type != "storage_object_created" || run.Snapshot.LogicalID != root {
		fail(w, r, 409, "original_invocation_unavailable", "Original object occurrence cannot be redriven")
		return
	}
	var redrives, active, delivered int
	if tx.QueryRow(r.Context(), `SELECT count(*),count(*) FILTER(WHERE state IN ('queued','dispatching')),count(*) FILTER(WHERE state='succeeded') FROM function_trigger_invocations WHERE parent_invocation_id=$1`, root).Scan(&redrives, &active, &delivered) != nil {
		fail(w, r, 503, "metadata_unavailable", "Redrive history unavailable")
		return
	}
	if active > 0 || delivered > 0 || redrives >= 3 {
		fail(w, r, 409, "redrive_budget_or_state", "An active/successful redrive or the three-redrive limit prevents another delivery")
		return
	}
	identity, e := readFunctionHTTPIdentity(r.Context(), tx, branch, run.Snapshot.Slug)
	if e != nil || identity.ID != run.Function || (identity.State != "active" && !(identity.State == "suspended" && identity.Idle && identity.IdleSuspended)) {
		fail(w, r, 409, "function_not_exposed", "Original Function must be active or eligible for automatic wake")
		return
	}
	var originalAuthorized bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users u JOIN organization_members m ON m.user_id=u.id JOIN projects p ON p.org_id=m.org_id LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id WHERE u.id=$1 AND p.id=$2 AND NOT u.disabled AND (m.role IN ('admin','owner','editor','developer') OR g.role IN ('admin','editor')))`, run.Actor, project).Scan(&originalAuthorized)
	if err != nil || !originalAuthorized {
		fail(w, r, 409, "original_actor_unavailable", "Original trigger actor must remain authorized")
		return
	}
	var recent int
	if tx.QueryRow(r.Context(), `SELECT count(*) FROM function_trigger_retry_requests WHERE actor_id=$1 AND created_at>now()-interval '1 hour'`, actor).Scan(&recent) != nil || recent >= 100 {
		fail(w, r, 429, "redrive_rate_limited", "Retry later")
		return
	}
	id := "fti_" + strings.TrimPrefix(s.createRequestHash("object-redrive-delivery", record{"actor": actor, "key": keyHash, "request": hash}), "h1:")
	// A domain-separated HMAC gives the exact same delivery on a lost reply.
	if !functions.ValidTriggerInvocationID(id) {
		fail(w, r, 503, "redrive_identity_unavailable", "Durable delivery identity unavailable")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO function_trigger_invocations(id,trigger_id,project_id,branch_id,function_id,actor_id,trigger_generation,scheduled_at,snapshot,state,object_event_id,parent_invocation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',$10,$11)`, id, trigger, project, branch, run.Function, run.Actor, run.Generation, run.Scheduled, raw, run.ObjectEvent, root)
	result := record{"invocation_id": id, "logical_invocation_id": root, "parent_invocation_id": root, "state": "queued", "original_receipt_retained": true, "delivery": "at_least_once_explicit_redrive"}
	receipt, _ = json.Marshal(result)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO function_trigger_retry_requests(actor_id,project_id,branch_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4,$5,$6)`, actor, project, branch, keyHash, hash, receipt)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'redrive_object_trigger','function_trigger_invocation',$2,'succeeded',$3)`, actor, id, requestID(r))
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		fail(w, r, 503, "redrive_outcome_unconfirmed", "Inspect current state or replay the same key/input")
		return
	}
	jsonResponse(w, 202, result)
}
