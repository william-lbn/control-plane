package control

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

// An administrator supplies external evidence and, for a lost creation reply,
// a candidate ID from the supplier dashboard. Only a GET is performed. The
// supplier must echo this handle's original HMAC; candidate IDs are never
// adopted merely because they exist. Financial reconciliation remains separate.
func (s *server) reconcileAIProviderState(w http.ResponseWriter, r *http.Request, actor, project, branch string, raw []byte, route aigateway.Route, cfg *aigateway.Config) {
	var input struct {
		RemoteID   string `json:"remote_id"`
		Resolution string `json:"resolution"`
		Evidence   string `json:"evidence_sha256"`
	}
	r.Body = http.NoBody
	if json.Unmarshal(raw, &input) != nil || !aiProviderIdentity.MatchString(input.Evidence) || (input.Resolution != "adopted" && input.Resolution != "confirmed_applied" && input.Resolution != "confirmed_not_accepted" && input.Resolution != "deleted") {
		aigateway.Error(w, 422, "invalid_provider_state_reconciliation")
		return
	}
	// Reject misspelled or additional fields rather than dropping an operator's
	// intent. This action never accepts replacement ownership or route fields.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for key := range fields {
		if key != "remote_id" && key != "resolution" && key != "evidence_sha256" {
			aigateway.Error(w, 422, "invalid_provider_state_reconciliation")
			return
		}
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		aigateway.Error(w, 422, "invalid_idempotency_key")
		return
	}
	id := r.PathValue("providerResponse")
	if id == "" {
		id = r.PathValue("providerConversation")
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	if s.aiProviders != nil {
		var revision int
		_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(794210047)`)
		if err == nil {
			err = tx.QueryRow(ctx, `SELECT revision FROM ai_provider_catalog_state WHERE singleton`).Scan(&revision)
		}
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
		if cfg.Generation != s.ai.Generation+revision {
			aigateway.Error(w, 409, "provider_catalog_changed")
			return
		}
	}
	if err = aiBranchLock(ctx, tx, branch); err != nil {
		aiInputFailure(w, r, err)
		return
	}
	var org string
	if err = tx.QueryRow(ctx, `SELECT org_id FROM ai_input_file_scopes WHERE branch_id=$1 AND project_id=$2`, branch, project).Scan(&org); err != nil {
		aiInputReadFailure(w, r, err)
		return
	}
	h, state, _, err := scanAIProviderHandle(tx.QueryRow(ctx, `SELECT `+aiProviderHandleColumns+` FROM ai_provider_handles WHERE id=$1 AND project_id=$2 AND branch_id=$3 FOR UPDATE`, id, project, branch))
	if err != nil {
		aiInputReadFailure(w, r, err)
		return
	}
	if h.ProviderIdentity != route.StateIdentity() {
		aigateway.Error(w, 409, "provider_state_route_changed")
		return
	}
	canonical, _ := json.Marshal([]any{id, project, branch, input})
	version := s.backendKeys.Active
	requestHash := s.backendHash("neon-ai-provider-state-reconcile-v1", string(canonical), version)
	var existing, existingHash, existingVersion, resolution, evidence string
	err = tx.QueryRow(ctx, `SELECT id,request_hash,key_version,resolution,evidence_sha256 FROM ai_provider_state_reconciliations WHERE actor_id=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&existing, &existingHash, &existingVersion, &resolution, &evidence)
	if err == nil {
		if existingHash != s.backendHash("neon-ai-provider-state-reconcile-v1", string(canonical), existingVersion) {
			aigateway.Error(w, 409, "idempotency_conflict")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		jsonResponse(w, 200, record{"id": existing, "object": "provider.state.reconciliation", "handle_id": h.ID, "resolution": resolution, "evidence_sha256": evidence})
		return
	}
	if !isNoRows(err) {
		aiInputFailure(w, r, err)
		return
	}
	if state == "deleted" || state == "revoked" {
		aigateway.Error(w, 409, "provider_state_reconciliation_unavailable")
		return
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_gateway_requests WHERE id=$1 AND state='running' AND expires_at>clock_timestamp()) OR EXISTS(SELECT 1 FROM ai_provider_state_mutations WHERE handle_id=$2 AND state='pending' AND created_at>clock_timestamp()-make_interval(secs=>$3))`, h.RequestID, h.ID, cfg.TimeoutSeconds+30).Scan(&busy); err != nil {
		aiInputFailure(w, r, err)
		return
	}
	if busy {
		aigateway.Error(w, 409, "provider_state_in_use")
		return
	}
	var mutation, action string
	err = tx.QueryRow(ctx, `SELECT id,action FROM ai_provider_state_mutations WHERE handle_id=$1 AND state IN ('pending','uncertain') ORDER BY created_at LIMIT 1 FOR UPDATE`, h.ID).Scan(&mutation, &action)
	if err != nil && !isNoRows(err) {
		aiInputFailure(w, r, err)
		return
	}
	if mutation == "" && state != "uncertain" && state != "pending" {
		aigateway.Error(w, 409, "provider_state_reconciliation_unavailable")
		return
	}
	if input.Resolution == "adopted" && (mutation != "" && action != "create") || input.Resolution == "deleted" && action != "delete" || input.Resolution != "adopted" && input.Resolution != "deleted" && (mutation == "" || action == "create" || action == "delete") {
		aigateway.Error(w, 422, "provider_state_resolution_mismatch")
		return
	}
	data := aiProviderHandleData{}
	if len(h.Ciphertext) > 0 {
		data, err = s.aiProviderHandleOpen(h)
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
	}
	remote := data.RemoteID
	if input.RemoteID != "" {
		if remote != "" && remote != input.RemoteID {
			aigateway.Error(w, 409, "provider_state_identity_conflict")
			return
		}
		remote = input.RemoteID
	}
	if !aiProviderRemoteID.MatchString(remote) || h.Kind == "response" && !strings.HasPrefix(remote, "resp_") || h.Kind == "conversation" && !strings.HasPrefix(remote, "conv_") {
		aigateway.Error(w, 422, "provider_state_candidate_required")
		return
	}
	proof, err := s.aiProviderHandleProof(h)
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	path := "/responses/" + remote
	if h.Kind == "conversation" {
		path = "/conversations/" + remote
	}
	// Keep the branch/receipt locks until the bounded observation is committed.
	// No supplier write or inference is issued under this transaction.
	value, status, _, callErr := aigateway.ProviderStateCall(ctx, route, "GET", path, "", nil, time.Duration(cfg.TimeoutSeconds)*time.Second)
	providerStatus := "ready"
	if input.Resolution == "deleted" {
		if callErr == nil || status != 404 || len(h.Ciphertext) == 0 {
			aigateway.Error(w, 409, "provider_state_deletion_not_proven")
			return
		}
	} else {
		metadata, _ := value["metadata"].(map[string]any)
		echo, _ := metadata["_neon_binding"].(string)
		if callErr != nil || value["id"] != remote || !hmac.Equal([]byte(proof), []byte(echo)) {
			aigateway.Error(w, 409, "provider_state_ownership_not_proven")
			return
		}
		if h.Kind == "response" {
			providerStatus, _ = value["status"].(string)
			if providerStatus != "completed" && providerStatus != "incomplete" && providerStatus != "failed" && providerStatus != "cancelled" {
				aigateway.Error(w, 409, "provider_response_not_terminal")
				return
			}
		}
		if len(h.Ciphertext) == 0 {
			if err = s.aiProviderHandleSeal(&h, aiProviderHandleData{RemoteID: remote}); err != nil {
				aiInputFailure(w, r, err)
				return
			}
		}
	}
	if state == "pending" {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_handles SET state='uncertain',version=version+1,updated_at=clock_timestamp() WHERE id=$1`, h.ID)
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
	}
	if input.Resolution == "deleted" {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_handles SET state='deleted',provider_status='deleted',ciphertext=NULL,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, h.ID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_handles SET state='ready',provider_status=$2,ciphertext=COALESCE(ciphertext,$3),remote_identity_sha256=COALESCE(remote_identity_sha256,$4),version=version+1,updated_at=clock_timestamp() WHERE id=$1`, h.ID, providerStatus, h.Ciphertext, aiProviderRemoteIdentity(h, remote))
	}
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	if mutation != "" {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_state_mutations SET state='reconciled',finished_at=clock_timestamp() WHERE id=$1`, mutation)
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
	}
	receipt := newID("apr_")
	_, err = tx.Exec(ctx, `INSERT INTO ai_provider_state_reconciliations(id,handle_id,mutation_id,actor_id,project_id,branch_id,key_hash,key_version,request_hash,resolution,evidence_sha256) VALUES($1,$2,NULLIF($3,''),$4,$5,$6,$7,$8,$9,$10,$11)`, receipt, h.ID, mutation, actor, project, branch, keyHash(key), version, requestHash, input.Resolution, input.Evidence)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, record{"id": receipt, "object": "provider.state.reconciliation", "handle_id": h.ID, "resolution": input.Resolution, "evidence_sha256": input.Evidence})
}
