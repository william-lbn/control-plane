package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

func registerAIProviderState(mux *http.ServeMux, root string, handler http.HandlerFunc) {
	for _, suffix := range []string{"/v1", "/openai/v1"} {
		for _, route := range []string{"GET responses/{providerResponse}", "DELETE responses/{providerResponse}", "GET responses/{providerResponse}/input_items", "POST responses/{providerResponse}/reconcile", "POST conversations", "GET conversations/{providerConversation}", "POST conversations/{providerConversation}", "DELETE conversations/{providerConversation}", "POST conversations/{providerConversation}/reconcile", "POST conversations/{providerConversation}/items", "GET conversations/{providerConversation}/items", "GET conversations/{providerConversation}/items/{providerItem}", "DELETE conversations/{providerConversation}/items/{providerItem}"} {
			method, path, _ := strings.Cut(route, " ")
			mux.HandleFunc(method+" "+root+suffix+"/"+path, handler)
		}
	}
}

func aiProviderStateSuffix(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/responses/") {
		suffix := "/responses/" + r.PathValue("providerResponse")
		if strings.HasSuffix(r.URL.Path, "/input_items") {
			suffix += "/input_items"
		}
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			suffix += "/reconcile"
		}
		return suffix
	}
	suffix := "/conversations"
	if id := r.PathValue("providerConversation"); id != "" {
		suffix += "/" + id
	}
	if strings.HasSuffix(r.URL.Path, "/items") || r.PathValue("providerItem") != "" {
		suffix += "/items"
	}
	if id := r.PathValue("providerItem"); id != "" {
		suffix += "/" + id
	}
	if strings.HasSuffix(r.URL.Path, "/reconcile") {
		suffix += "/reconcile"
	}
	return suffix
}
func aiProviderStateProtocol(r *http.Request) string {
	return "provider-state:" + r.Method + ":" + aiProviderStateSuffix(r) + "?" + r.URL.RawQuery
}

// Model-less supplier APIs require an explicit published alias. The credential
// is checked against that model before private routes or handles are opened.
func (s *server) aiPublicProviderState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	model, branch := r.Header.Get("X-Neon-Model"), r.PathValue("branch")
	if model == "" || len(model) > 128 {
		aigateway.Error(w, 422, "provider_state_model_required")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32769))
	if err != nil || len(raw) > 32768 {
		aigateway.Error(w, 422, "invalid_provider_state_request")
		return
	}
	if r.Method != "POST" && len(raw) > 0 {
		aigateway.Error(w, 422, "invalid_provider_state_request")
		return
	}
	actor, project := "", ""
	generation := -1
	if ticket := r.Header.Get("X-Neon-Console-Ticket"); ticket != "" && r.Header.Get("Authorization") == "" {
		t, err := s.verifyAITicket(ticket, raw, branch, model)
		if err != nil || t.Protocol != aiProviderStateProtocol(r) {
			aigateway.Error(w, 401, "invalid_console_ticket")
			return
		}
		actor, generation = t.Actor, t.Generation
		if s.db.QueryRow(r.Context(), `SELECT b.project_id FROM branches b JOIN projects p ON p.id=b.project_id JOIN users u ON u.id=$2 AND NOT u.disabled WHERE b.id=$1 AND b.state='ready' AND b.deleted_at IS NULL AND p.state='ready' AND p.deleted_at IS NULL`, branch, actor).Scan(&project) != nil {
			aigateway.Error(w, 403, "credential_scope_denied")
			return
		}
		access, e := s.projectAccess(r.Context(), project, actor)
		if e != nil || access.Level < 2 {
			aigateway.Error(w, 403, "credential_scope_denied")
			return
		}
	} else {
		header := aiAuthorization(r)
		if !strings.HasPrefix(header, "Bearer ") {
			aigateway.Error(w, 401, "invalid_backend_credential")
			return
		}
		verified, e := s.verifyBackendCredential(r.Context(), strings.TrimPrefix(header, "Bearer "), branch, "ai_gateway:invoke", model)
		if e != nil {
			status := 503
			if errors.Is(e, errBackendCredentialInvalid) {
				status = 401
			}
			if errors.Is(e, errBackendCredentialForbidden) {
				status = 403
			}
			aigateway.Error(w, status, "credential_scope_denied")
			return
		}
		if s.db.QueryRow(r.Context(), `SELECT issuer_id,project_id FROM backend_credentials WHERE id=$1`, verified["credential_id"]).Scan(&actor, &project) != nil {
			aigateway.Error(w, 503, "credential_unavailable")
			return
		}
	}
	snapshot, err := s.aiBranchCatalog(r.Context(), true, branch)
	if err != nil || snapshot.Config == nil {
		aigateway.Error(w, 503, "provider_catalog_unavailable")
		return
	}
	if generation >= 0 && generation != snapshot.Config.Generation {
		aigateway.Error(w, 409, "provider_catalog_changed")
		return
	}
	route, ok := snapshot.Config.Find(model)
	if !ok || !route.AllowProviderState {
		aigateway.Error(w, 403, "provider_state_disabled")
		return
	}
	if snapshot.Unavailable[model] {
		aigateway.Error(w, 503, "provider_credential_unavailable")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/reconcile") {
		if r.Header.Get("X-Neon-Console-Ticket") == "" {
			aigateway.Error(w, 403, "console_administrator_required")
			return
		}
		access, e := s.projectAccess(r.Context(), project, actor)
		if e != nil || access.Level < 3 {
			aigateway.Error(w, 403, "console_administrator_required")
			return
		}
		s.reconcileAIProviderState(w, r, actor, project, branch, raw, route, snapshot.Config)
		return
	}
	s.serveAIProviderState(w, r, actor, project, branch, raw, route, snapshot.Config)
}

func decodeAIProviderConversation(raw []byte, itemsOnly bool, route aigateway.Route) (map[string]any, error) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value map[string]any
	if d.Decode(&value) != nil || value == nil || d.Decode(new(any)) != io.EOF {
		return nil, providerProblem(422, "invalid_provider_state_request", "A JSON object is required")
	}
	for k := range value {
		if k != "metadata" && k != "items" || itemsOnly && k != "items" {
			return nil, providerProblem(422, "invalid_provider_state_request", "Unsupported conversation field")
		}
	}
	if metadata, ok := value["metadata"]; ok {
		clean, err := aigateway.ProviderStateMetadata(metadata)
		if err != nil {
			return nil, providerProblem(422, "invalid_metadata", "At most 15 metadata entries are allowed; _neon_ is reserved")
		}
		value["metadata"] = clean
	}
	if items, ok := value["items"]; ok {
		list, valid := items.([]any)
		if !valid || len(list) > 20 || itemsOnly && len(list) == 0 {
			return nil, providerProblem(422, "invalid_items", "Use 1-20 inline conversation items")
		}
		// Conversations accept the same bounded inline native inputs. Raw
		// supplier item/file/vector identifiers remain forbidden recursively.
		if err := aigateway.ValidateProviderStateItems(list, route); err != nil {
			return nil, providerProblem(422, "invalid_items", "Use bounded inline messages or function tool items")
		}
	} else if itemsOnly {
		return nil, providerProblem(422, "invalid_items", "Conversation items are required")
	}
	return value, nil
}

func (s *server) serveAIProviderState(w http.ResponseWriter, r *http.Request, actor, project, branch string, raw []byte, route aigateway.Route, cfg *aigateway.Config) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	suffix := aiProviderStateSuffix(r)
	id := r.PathValue("providerResponse")
	kind := "response"
	if strings.HasPrefix(suffix, "/conversations") {
		id = r.PathValue("providerConversation")
		kind = "conversation"
	}
	creating := kind == "conversation" && id == "" && r.Method == "POST"
	mutating := r.Method != "GET"
	action := ""
	if mutating {
		action = "update"
		if creating {
			action = "create"
		} else if r.Method == "DELETE" {
			action = "delete"
		}
		if strings.Contains(suffix, "/items") {
			action = "create_items"
			if r.Method == "DELETE" {
				action = "delete_item"
			}
		}
	}
	if !creating && !(kind == "response" && aigateway.ResponseHandle.MatchString(id) || kind == "conversation" && aigateway.ConversationHandle.MatchString(id)) {
		aigateway.Error(w, 404, "not_found")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if mutating && (len(key) < 8 || len(key) > 128) {
		aigateway.Error(w, 422, "invalid_idempotency_key")
		return
	}
	input := map[string]any{}
	var err error
	if r.Method == "POST" {
		input, err = decodeAIProviderConversation(raw, action == "create_items", route)
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
		if action == "update" && input["items"] != nil {
			aigateway.Error(w, 422, "invalid_provider_state_request")
			return
		}
	}
	requestRaw, _ := json.Marshal([]any{r.Method, suffix, r.URL.RawQuery, project, branch, route.ID, input})
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
	var h aiProviderHandle
	var data aiProviderHandleData
	mutationID, version, requestHash, mutationState, mutationHandle := "", "", "", "", ""
	var replay []byte
	var replayStatus int
	if mutating {
		err = tx.QueryRow(ctx, `SELECT id,key_version,request_hash,state,handle_id,response_ciphertext,COALESCE(http_status,503) FROM ai_provider_state_mutations WHERE actor_id=$1 AND key_hash=$2 FOR UPDATE`, actor, keyHash(key)).Scan(&mutationID, &version, &requestHash, &mutationState, &mutationHandle, &replay, &replayStatus)
		if err == nil {
			if requestHash != s.backendHash("neon-ai-provider-state-request-v1", string(requestRaw), version) {
				aigateway.Error(w, 409, "idempotency_conflict")
				return
			}
			if mutationState != "succeeded" {
				aigateway.Error(w, 409, "provider_state_outcome_unavailable")
				return
			}
			// Replays recheck current handle ownership and the current published
			// supplier account. A receipt is never a capability on its own.
			h, _, _, err = scanAIProviderHandle(tx.QueryRow(ctx, `SELECT `+aiProviderHandleColumns+` FROM ai_provider_handles WHERE id=$1 AND project_id=$2 AND branch_id=$3`, mutationHandle, project, branch))
			if err != nil || h.ProviderIdentity != route.StateIdentity() {
				aigateway.Error(w, 409, "provider_state_route_changed")
				return
			}
			opened, e := s.openAIProviderState(version, "mutation-receipt", aiProviderMutationAAD(mutationID, actor, project, branch, h.ID, action, requestHash), replay)
			if e != nil {
				aiInputFailure(w, r, e)
				return
			}
			defer clear(opened)
			w.Header().Set("Idempotency-Replayed", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(replayStatus)
			_, _ = w.Write(opened)
			return
		}
		if !isNoRows(err) {
			aiInputFailure(w, r, err)
			return
		}
		var recent int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM ai_provider_state_mutations WHERE branch_id=$1 AND created_at>clock_timestamp()-interval '1 minute'`, branch).Scan(&recent); err != nil {
			aiInputFailure(w, r, err)
			return
		}
		if recent >= 60 {
			aigateway.Error(w, 429, "provider_state_mutation_limit")
			return
		}
	}
	if creating {
		h = s.newAIProviderHandle(newID("ncv_"), "conversation", newID("aps_"), project, branch, actor, route)
		if err = s.insertAIProviderHandle(ctx, tx, &h); err != nil {
			aiInputFailure(w, r, err)
			return
		}
	} else {
		h, data, err = s.loadAIProviderHandle(ctx, tx, id, project, branch, route, action == "delete")
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
		var busy bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_provider_state_mutations WHERE handle_id=$1 AND state IN ('pending','uncertain')) OR EXISTS(SELECT 1 FROM ai_provider_state_uses u JOIN ai_gateway_requests g ON g.id=u.request_id WHERE u.handle_id=$1 AND g.state='running')`, h.ID).Scan(&busy)
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
		if busy && mutating {
			aigateway.Error(w, 409, "provider_state_in_use")
			return
		}
	}
	proof, err := s.aiProviderHandleProof(h)
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	path := "/responses/" + data.RemoteID
	if kind == "conversation" {
		path = "/conversations"
		if !creating {
			path += "/" + data.RemoteID
		}
	}
	if strings.HasSuffix(suffix, "/input_items") {
		path += "/input_items"
	}
	if strings.Contains(suffix, "/items") {
		path += "/items"
	}
	if token := r.PathValue("providerItem"); token != "" {
		remote, e := s.aiProviderItemOpen(h, token)
		if e != nil {
			aiInputFailure(w, r, e)
			return
		}
		path += "/" + url.PathEscape(remote)
	}
	query, e := s.aiProviderStateQuery(h, r.URL.Query())
	if e != nil {
		aiInputFailure(w, r, e)
		return
	}
	if r.URL.RawQuery != "" && !strings.HasSuffix(suffix, "/items") && !strings.HasSuffix(suffix, "/input_items") {
		aigateway.Error(w, 422, "invalid_provider_state_query")
		return
	}
	if r.Method == "POST" && action != "create_items" {
		metadata, _ := input["metadata"].(map[string]string)
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata["_neon_binding"] = proof
		input["metadata"] = metadata
	}
	body := []byte(nil)
	if r.Method == "POST" {
		body, _ = json.Marshal(input)
	}
	if mutating {
		mutationID, version = newID("aps_"), s.backendKeys.Active
		requestHash = s.backendHash("neon-ai-provider-state-request-v1", string(requestRaw), version)
		_, err = tx.Exec(ctx, `INSERT INTO ai_provider_state_mutations(id,actor_id,project_id,org_id,branch_id,handle_id,action,key_hash,key_version,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, mutationID, actor, project, org, branch, h.ID, action, keyHash(key), version, requestHash)
		if err == nil && action == "delete" {
			_, err = tx.Exec(ctx, `UPDATE ai_provider_handles SET state='deleting',version=version+1,updated_at=clock_timestamp() WHERE id=$1`, h.ID)
		}
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		aiInputFailure(w, r, err)
		return
	}
	value, status, dispatched, callErr := aigateway.ProviderStateCall(ctx, route, r.Method, path, query, body, time.Duration(cfg.TimeoutSeconds)*time.Second)
	clear(body)
	if callErr == nil {
		callErr = s.rewriteAIProviderStateResult(h, data, proof, action, suffix, r.PathValue("providerItem"), value)
	}
	finish, close := context.WithTimeout(context.Background(), 5*time.Second)
	defer close()
	if callErr == nil && creating {
		remote, _ := value["_neon_remote_id"].(string)
		delete(value, "_neon_remote_id")
		callErr = s.commitAIProviderHandle(finish, h, remote, "ready", nil)
	}
	if mutating {
		if status != 200 && status != 201 {
			if callErr == nil {
				callErr = errors.New("unexpected supplier state status")
			}
		}
		if err = s.finishAIProviderMutation(finish, h, mutationID, actor, action, requestHash, version, value, status, callErr == nil, dispatched); err != nil {
			callErr = err
		}
	}
	if callErr != nil {
		code := 502
		if status == 429 {
			code = 429
		}
		aigateway.Error(w, code, "provider_state_outcome_unavailable")
		return
	}
	jsonResponse(w, status, value)
}

func (s *server) aiProviderStateQuery(h aiProviderHandle, q url.Values) (string, error) {
	out := url.Values{}
	for key, values := range q {
		if len(values) != 1 {
			return "", providerProblem(422, "invalid_provider_state_query", "Duplicate query field")
		}
		v := values[0]
		switch key {
		case "limit":
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 100 {
				return "", providerProblem(422, "invalid_provider_state_query", "Page limit must be 1-100")
			}
			out.Set(key, strconv.Itoa(n))
		case "order":
			if v != "asc" && v != "desc" {
				return "", providerProblem(422, "invalid_provider_state_query", "Invalid page order")
			}
			out.Set(key, v)
		case "after", "before":
			remote, e := s.aiProviderItemOpen(h, v)
			if e != nil {
				return "", e
			}
			out.Set(key, remote)
		default:
			return "", providerProblem(422, "invalid_provider_state_query", "Unsupported query field")
		}
	}
	if out.Get("after") != "" && out.Get("before") != "" {
		return "", providerProblem(422, "invalid_provider_state_query", "Use one pagination cursor")
	}
	return out.Encode(), nil
}

func (s *server) rewriteAIProviderStateResult(h aiProviderHandle, data aiProviderHandleData, proof, action, suffix, itemToken string, value map[string]any) error {
	if action == "delete" {
		if value["id"] != data.RemoteID || value["deleted"] != true {
			return errAIProviderHandleEnvelope
		}
		value["id"] = h.ID
		return nil
	}
	if action == "delete_item" {
		// OpenAI returns the parent Conversation, not an item.deleted object.
		// Authenticate that parent receipt before acknowledging the deletion.
		metadata, _ := value["metadata"].(map[string]any)
		echo, _ := metadata["_neon_binding"].(string)
		if value["id"] != data.RemoteID || value["object"] != "conversation" || !hmac.Equal([]byte(echo), []byte(proof)) {
			return errAIProviderHandleEnvelope
		}
		delete(metadata, "_neon_binding")
		value["id"] = h.ID
		return nil
	}
	if strings.Contains(suffix, "/items") || strings.HasSuffix(suffix, "/input_items") {
		if strings.HasSuffix(suffix, "/items") || strings.HasSuffix(suffix, "/input_items") {
			items, ok := value["data"].([]any)
			if !ok || len(items) > 100 || value["object"] != "list" {
				return errAIProviderHandleEnvelope
			}
		} else {
			remote, err := s.aiProviderItemOpen(h, itemToken)
			if err != nil || value["id"] != remote {
				return errAIProviderHandleEnvelope
			}
		}
		return s.rewriteAIProviderItems(h, value)
	}
	if h.Kind == "response" {
		return aigateway.RewriteProviderResponse(value, h.Model, aigateway.ProviderStateBinding{Intent: aigateway.ProviderStateIntent{Store: true, Previous: h.Previous, Conversation: h.Parent}, ResponseID: h.ID, PreviousRemote: "", ConversationRemote: "", Proof: proof, CommitResponse: func(remote, status string) error {
			if remote != data.RemoteID {
				return errAIProviderHandleEnvelope
			}
			return nil
		}, RewriteItems: func(items any) error { return s.rewriteAIProviderItems(h, items) }})
	}
	remote, ok := value["id"].(string)
	metadata, valid := value["metadata"].(map[string]any)
	echo, _ := metadata["_neon_binding"].(string)
	if !ok || !valid || !strings.HasPrefix(remote, "conv_") || !aiProviderRemoteID.MatchString(remote) || !hmac.Equal([]byte(echo), []byte(proof)) || action != "create" && remote != data.RemoteID {
		return errAIProviderHandleEnvelope
	}
	delete(metadata, "_neon_binding")
	value["id"] = h.ID
	if action == "create" {
		value["_neon_remote_id"] = remote
	}
	return nil
}

func (s *server) finishAIProviderMutation(ctx context.Context, h aiProviderHandle, id, actor, action, hash, version string, value map[string]any, status int, success, dispatched bool) error {
	var ciphertext []byte
	var err error
	if success {
		raw, e := json.Marshal(value)
		if e != nil || len(raw) > 65536 {
			return errors.New("supplier receipt exceeds persistence bound")
		}
		defer clear(raw)
		ciphertext, err = s.sealAIProviderState(version, "mutation-receipt", aiProviderMutationAAD(id, actor, h.Project, h.Branch, h.ID, action, hash), raw)
		if err != nil {
			return err
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	state := "uncertain"
	if success {
		state = "succeeded"
	} else if !dispatched {
		state = "failed"
	}
	_, err = tx.Exec(ctx, `UPDATE ai_provider_state_mutations SET state=$2,response_ciphertext=$3,http_status=$4,finished_at=CASE WHEN $2 IN ('succeeded','failed') THEN clock_timestamp() ELSE NULL END WHERE id=$1 AND state='pending'`, id, state, ciphertext, statusOrUnavailable(status))
	if err != nil {
		return err
	}
	if action == "delete" && success {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_handles SET state='deleted',provider_status='deleted',ciphertext=NULL,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, h.ID)
	} else if !success && (action == "create" || action == "delete") {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_handles SET state='uncertain',version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND state IN ('pending','deleting')`, h.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func statusOrUnavailable(status int) int {
	if status < 200 || status > 599 {
		return 503
	}
	return status
}

func (s *server) aiProviderStateList(w http.ResponseWriter, r *http.Request) {
	if accessFrom(r).Level < 2 {
		fail(w, r, 403, "forbidden", "Editing access is required")
		return
	}
	items, err := s.many(r.Context(), `SELECT id,kind,model_id,state,provider_status,created_at,expires_at,version,COALESCE(parent_handle_id,'') AS conversation,COALESCE(previous_handle_id,'') AS previous_response_id FROM ai_provider_handles WHERE project_id=$1 AND branch_id=$2 ORDER BY created_at DESC,id DESC LIMIT 100`, r.PathValue("project"), r.PathValue("branch"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Provider references are unavailable")
		return
	}
	jsonResponse(w, 200, record{"items": items})
}

func (s *server) aiConsoleProviderState(w http.ResponseWriter, r *http.Request) {
	if userFrom(r).APIKey || accessFrom(r).Level < 2 {
		fail(w, r, 403, "console_session_required", "A Console editing session is required")
		return
	}
	if s.ai == nil || s.backendKeys == nil || os.Getenv("NEON_AI_GATEWAY_URL") == "" {
		aigateway.Error(w, 503, "ai_gateway_disabled")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32769))
	if err != nil || len(raw) > 32768 {
		aigateway.Error(w, 422, "invalid_request")
		return
	}
	branch, model := r.PathValue("branch"), r.Header.Get("X-Neon-Model")
	snapshot, err := s.aiBranchCatalog(r.Context(), false, branch)
	if err != nil || snapshot.Config == nil {
		aigateway.Error(w, 503, "provider_catalog_unavailable")
		return
	}
	path := "/ai/v1/" + url.PathEscape(branch) + "/v1" + aiProviderStateSuffix(r)
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, strings.TrimRight(os.Getenv("NEON_AI_GATEWAY_URL"), "/")+path, bytes.NewReader(raw))
	if err != nil {
		aigateway.Error(w, 503, "gateway_unavailable")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Neon-Model", model)
	req.Header.Set("Idempotency-Key", r.Header.Get("Idempotency-Key"))
	req.Header.Set("X-Neon-Console-Ticket", s.signAIProtocolTicket(userFrom(r).ID, branch, model, raw, snapshot.Config.Generation, aiProviderStateProtocol(r)))
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Duration(s.ai.TimeoutSeconds+5) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		aigateway.Error(w, 502, "provider_state_outcome_unavailable")
		return
	}
	defer response.Body.Close()
	for _, key := range []string{"Content-Type", "Cache-Control", "Idempotency-Replayed"} {
		w.Header().Set(key, response.Header.Get(key))
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, aigateway.MaxResponseBytes+1))
}

func (s *server) aiProviderStateRelay(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil || os.Getenv("NEON_AI_GATEWAY_URL") == "" {
		aigateway.Error(w, 503, "ai_gateway_disabled")
		return
	}
	path := "/ai/v1/" + url.PathEscape(r.PathValue("aiBranch")) + "/v1" + aiProviderStateSuffix(r)
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, strings.TrimRight(os.Getenv("NEON_AI_GATEWAY_URL"), "/")+path, io.LimitReader(r.Body, 32769))
	if err != nil {
		aigateway.Error(w, 503, "gateway_unavailable")
		return
	}
	for _, key := range []string{"Authorization", "Content-Type", "Idempotency-Key", "X-Neon-Model"} {
		req.Header.Set(key, r.Header.Get(key))
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Duration(s.ai.TimeoutSeconds+5) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		aigateway.Error(w, 502, "provider_state_outcome_unavailable")
		return
	}
	defer response.Body.Close()
	for _, key := range []string{"Content-Type", "Cache-Control", "Idempotency-Replayed"} {
		w.Header().Set(key, response.Header.Get(key))
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, aigateway.MaxResponseBytes+1))
}
