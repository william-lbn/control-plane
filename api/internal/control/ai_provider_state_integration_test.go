package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

type providerStateFixture struct {
	*providerFixture
	mux     *http.ServeMux
	calls   atomic.Int64
	fail    atomic.Bool
	mu      sync.Mutex
	objects map[string]record
	items   map[string][]any
}

func newProviderStateFixture(t *testing.T) *providerStateFixture {
	f := &providerStateFixture{providerFixture: newProviderFixture(t), objects: map[string]record{}, items: map[string][]any{}}
	f.s.aiProviders = nil
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		var input record
		if r.Method == "POST" && json.NewDecoder(r.Body).Decode(&input) != nil {
			w.WriteHeader(400)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		parts := strings.Split(path, "/")
		if r.Method == "POST" && path == "responses" {
			for _, key := range []string{"conversation", "previous_response_id"} {
				if id, ok := input[key].(string); ok && f.objects[id] == nil {
					t.Error("unresolved supplier context", key)
					w.WriteHeader(400)
					return
				}
			}
			id := fmt.Sprintf("resp_fixture%d", n)
			value := record{"id": id, "object": "response", "model": "actual", "status": "completed", "metadata": input["metadata"], "previous_response_id": input["previous_response_id"], "conversation": input["conversation"], "output": []any{record{"id": "msg_fixture", "type": "message", "role": "assistant", "content": []any{record{"type": "output_text", "text": "state fixture reply"}}}}, "usage": record{"input_tokens": 9, "output_tokens": 2, "total_tokens": 11}}
			f.objects[id] = value
			if f.fail.Load() {
				w.WriteHeader(500)
				return
			}
			if input["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				created := record{}
				for k, v := range value {
					created[k] = v
				}
				created["status"] = "in_progress"
				created["output"] = []any{}
				delete(created, "usage")
				for _, event := range []record{{"type": "response.created", "response": created}, {"type": "response.output_item.added", "item": record{"id": "msg_fixture", "type": "message"}, "response_id": id}, {"type": "response.output_text.delta", "item_id": "msg_fixture", "response_id": id, "delta": "state fixture reply"}, {"type": "response.completed", "response": value}} {
					raw, _ := json.Marshal(event)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
				}
				return
			}
			_ = json.NewEncoder(w).Encode(value)
			return
		}
		if r.Method == "POST" && path == "conversations" {
			id := fmt.Sprintf("conv_fixture%d", n)
			value := record{"id": id, "object": "conversation", "created_at": 1900000000, "metadata": input["metadata"]}
			f.objects[id] = value
			if initial, ok := input["items"].([]any); ok {
				for i, item := range initial {
					v := item.(map[string]any)
					v["id"] = fmt.Sprintf("msg_initial%d", i)
					f.items[id] = append(f.items[id], v)
				}
			}
			if f.fail.Load() {
				w.WriteHeader(500)
				return
			}
			_ = json.NewEncoder(w).Encode(value)
			return
		}
		if len(parts) < 2 || f.objects[parts[1]] == nil {
			w.WriteHeader(404)
			return
		}
		id := parts[1]
		if len(parts) >= 3 {
			list := f.items[id]
			if parts[2] == "input_items" {
				list = []any{record{"id": "msg_input", "type": "message", "role": "user", "content": "private fixture input"}}
			}
			if len(parts) == 4 {
				for i, item := range list {
					v := item.(map[string]any)
					if v["id"] == parts[3] {
						if r.Method == "DELETE" {
							f.items[id] = append(list[:i], list[i+1:]...)
							_ = json.NewEncoder(w).Encode(f.objects[id])
							return
						}
						_ = json.NewEncoder(w).Encode(v)
						return
					}
				}
				w.WriteHeader(404)
				return
			}
			if r.Method == "POST" {
				list = nil
				for i, item := range input["items"].([]any) {
					v := item.(map[string]any)
					v["id"] = fmt.Sprintf("msg_added%d_%d", n, i)
					list = append(list, v)
				}
				f.items[id] = append(f.items[id], list...)
			}
			if list == nil {
				list = []any{}
			}
			value := record{"object": "list", "data": list, "has_more": false, "first_id": nil, "last_id": nil}
			if len(list) > 0 {
				value["first_id"] = list[0].(map[string]any)["id"]
				value["last_id"] = list[len(list)-1].(map[string]any)["id"]
			}
			_ = json.NewEncoder(w).Encode(value)
			return
		}
		if r.Method == "DELETE" {
			delete(f.objects, id)
			_ = json.NewEncoder(w).Encode(record{"id": id, "deleted": true})
			return
		}
		if r.Method == "POST" {
			f.objects[id]["metadata"] = input["metadata"]
		}
		_ = json.NewEncoder(w).Encode(f.objects[id])
	}))
	t.Cleanup(peer.Close)
	f.s.ai.Routes = []aigateway.Route{{ID: "state", Name: "State", Provider: "openai_compatible", BaseURL: peer.URL + "/v1", UpstreamModel: "actual", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true, MaxOutputTokens: 32, Protocols: []string{aigateway.Responses}, AllowProviderState: true}}
	f.mux = http.NewServeMux()
	registerAINativePublic(f.mux, "branch", f.s.aiPublic)
	registerAIProviderState(f.mux, "/ai/v1/{branch}", f.s.aiPublicProviderState)
	gateway := httptest.NewServer(f.mux)
	t.Cleanup(gateway.Close)
	t.Setenv("NEON_AI_GATEWAY_URL", gateway.URL)
	id := newID("bcr_")
	token := "ncb_" + id[4:] + "." + randomToken(32)
	f.exec(`INSERT INTO backend_credentials(id,project_id,org_id,branch_id,issuer_id,name,scopes,branch_scope,allowed_models,token_hash,pepper_version,expires_at) VALUES($1,'prj_provider','org_provider','br_provider','provider_org_admin','state',ARRAY['ai_gateway:invoke'],'self',ARRAY['state'],$2,'v1',now()+interval '1 hour')`, id, f.s.backendHash("backend-token", token, "v1"))
	t.Setenv("NEON_TEST_STATE_TOKEN", token)
	return f
}
func (f *providerStateFixture) callState(t *testing.T, method, path, key string, input any, want int) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if input != nil {
		body, _ = json.Marshal(input)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("X-Neon-Model", "state")
	r.Header.Set("Idempotency-Key", key)
	if strings.HasPrefix(path, "/api/") {
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "provider_org_admin-session"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "provider_org_admin-csrf"})
		r.Header.Set("X-CSRF-Token", "provider_org_admin-csrf")
	} else {
		r.Header.Set("Authorization", "Bearer "+os.Getenv("NEON_TEST_STATE_TOKEN"))
	}
	w := httptest.NewRecorder()
	if strings.HasPrefix(path, "/api/") {
		f.s.routes().ServeHTTP(w, r)
	} else {
		f.mux.ServeHTTP(w, r)
	}
	if w.Code != want {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w
}
func stateValue(t *testing.T, w *httptest.ResponseRecorder) record {
	t.Helper()
	var value record
	if json.Unmarshal(w.Body.Bytes(), &value) != nil {
		t.Fatal("invalid public JSON")
	}
	return value
}

const stateRoot = "/ai/v1/br_provider/v1/"
const stateConsole = "/api/v1/projects/prj_provider/branches/br_provider/ai-gateway/state/v1/"

func TestAIProviderStateResponsesJSONStreamAndDeletion(t *testing.T) {
	f := newProviderStateFixture(t)
	first := stateValue(t, f.callState(t, "POST", stateRoot+"responses", "", record{"model": "state", "input": "private fixture prompt", "max_output_tokens": 8, "store": true}, 200))
	id := stringVal(first["id"])
	if !aigateway.ResponseHandle.MatchString(id) || strings.Contains(fmt.Sprint(first), "resp_fixture") {
		t.Fatal("supplier ID escaped")
	}
	f.callState(t, "GET", stateConsole+"responses/"+id, "", nil, 200)
	stream := f.callState(t, "POST", stateRoot+"responses", "", record{"model": "state", "input": "continue", "max_output_tokens": 8, "store": true, "stream": true, "previous_response_id": id}, 200).Body.String()
	if strings.Contains(stream, "resp_fixture") || strings.Contains(stream, "msg_fixture") || !strings.Contains(stream, "response.completed") || !strings.Contains(stream, "nit_") {
		t.Fatal("stream identity/receipt normalization failed", stream)
	}
	list := stateValue(t, f.callState(t, "GET", stateRoot+"responses/"+id+"/input_items?limit=10", "", nil, 200))
	if !strings.HasPrefix(list["last_id"].(string), "nit_") {
		t.Fatal("raw pagination identity")
	}
	f.callState(t, "GET", stateRoot+"responses/"+id+"/input_items?after="+list["last_id"].(string), "", nil, 200)
	f.callState(t, "DELETE", stateRoot+"responses/"+id, "delete-original-response", nil, 200)
	w := f.callState(t, "DELETE", stateRoot+"responses/"+id, "delete-original-response", nil, 200)
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("delete receipt not replayed")
	}
	f.callState(t, "GET", stateRoot+"responses/"+id, "", nil, 410)
	var refs, settled int
	if e := f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM ai_provider_state_uses),(SELECT count(*) FROM ai_gateway_requests WHERE state='completed' AND total_tokens=11)`).Scan(&refs, &settled); e != nil || refs != 1 || settled != 2 {
		t.Fatal("state admission/settlement", refs, settled, e)
	}
	var leaked bool
	if e := f.s.db.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM ai_provider_handles WHERE row_to_json(ai_provider_handles)::text LIKE '%private fixture prompt%' OR row_to_json(ai_provider_handles)::text LIKE '%state fixture reply%' OR row_to_json(ai_provider_handles)::text LIKE '%resp_fixture%')`).Scan(&leaked); e != nil || leaked {
		t.Fatal("plaintext content or remote IDs in ledger", e)
	}
}

func TestAIProviderStateConversationsItemsAndReplay(t *testing.T) {
	f := newProviderStateFixture(t)
	input := record{"metadata": record{"topic": "fixture"}, "items": []any{record{"type": "message", "role": "user", "content": "initial"}}}
	created := stateValue(t, f.callState(t, "POST", stateConsole+"conversations", "create-original-conversation", input, 200))
	id := created["id"].(string)
	if !aigateway.ConversationHandle.MatchString(id) || strings.Contains(fmt.Sprint(created), "_neon_binding") {
		t.Fatal("conversation ownership proof escaped")
	}
	before := f.calls.Load()
	f.callState(t, "POST", stateConsole+"conversations", "create-original-conversation", input, 200)
	if f.calls.Load() != before {
		t.Fatal("replay created a second supplier conversation")
	}
	f.callState(t, "POST", stateConsole+"conversations", "create-original-conversation", record{}, 409)
	f.callState(t, "GET", stateRoot+"conversations/"+id, "", nil, 200)
	f.callState(t, "POST", stateRoot+"conversations/"+id, "update-conversation-meta", record{"metadata": record{"topic": "updated"}}, 200)
	added := stateValue(t, f.callState(t, "POST", stateRoot+"conversations/"+id+"/items", "append-original-items", record{"items": []any{record{"role": "user", "content": "new item"}}}, 200))
	item := added["last_id"].(string)
	if added["data"].([]any)[0].(map[string]any)["id"] != item {
		t.Fatal("item and pagination token differ")
	}
	f.callState(t, "GET", stateRoot+"conversations/"+id+"/items/"+item, "", nil, 200)
	f.callState(t, "GET", stateRoot+"conversations/"+id+"/items?after="+item, "", nil, 200)
	f.callState(t, "POST", stateRoot+"responses", "", record{"model": "state", "input": "in conversation", "max_output_tokens": 8, "conversation": id}, 200)
	deletedItem := stateValue(t, f.callState(t, "DELETE", stateRoot+"conversations/"+id+"/items/"+item, "delete-original-item", nil, 200))
	if deletedItem["id"] != id || deletedItem["object"] != "conversation" || strings.Contains(fmt.Sprint(deletedItem), "_neon_binding") {
		t.Fatal("item deletion did not return a verified parent conversation")
	}
	f.callState(t, "DELETE", stateRoot+"conversations/"+id, "delete-original-conversation", nil, 200)
	f.callState(t, "GET", stateRoot+"conversations/"+id, "", nil, 410)
}

func TestAIProviderStateScopeWithdrawalAndUnknownNoReplay(t *testing.T) {
	f := newProviderStateFixture(t)
	id := stateValue(t, f.callState(t, "POST", stateRoot+"conversations", "scope-original-conversation", record{}, 200))["id"].(string)
	f.exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_state_other','prj_provider','other','ready',now())`)
	before := f.calls.Load()
	f.callState(t, "GET", "/ai/v1/br_state_other/v1/conversations/"+id, "", nil, 403)
	f.s.ai.Routes[0].RevisionID = "withdraw-old-revision"
	f.callState(t, "GET", stateRoot+"conversations/"+id, "", nil, 409)
	f.s.ai.Routes[0].RevisionID = ""
	f.s.ai.Routes[0].AllowProviderState = false
	f.callState(t, "GET", stateRoot+"conversations/"+id, "", nil, 403)
	if before != f.calls.Load() {
		t.Fatal("denied ownership reached supplier")
	}
	f.s.ai.Routes[0].AllowProviderState = true
	f.fail.Store(true)
	f.callState(t, "POST", stateRoot+"conversations", "unknown-original-create", record{}, 502)
	before = f.calls.Load()
	f.callState(t, "POST", stateRoot+"conversations", "unknown-original-create", record{}, 409)
	if before != f.calls.Load() {
		t.Fatal("unknown supplier mutation was replayed")
	}
	var count int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_provider_state_mutations WHERE state='uncertain'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("unknown receipt not retained", err)
	}
	f.callState(t, "POST", stateRoot+"responses", "", record{"model": "state", "input": "unsafe raw id", "max_output_tokens": 8, "previous_response_id": "resp_external"}, 400)
}

func TestAIProviderStateAdmissionRollbackAndHistoryGuards(t *testing.T) {
	f := newProviderStateFixture(t)
	route := f.s.ai.Routes[0]
	request := newID("air_")
	intent := aigateway.ProviderStateIntent{Store: true, Previous: "nrp_aaaaaaaaaaaaaaaaaaaaaaaa"}
	_, admit := f.s.prepareAIProviderResponse(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", route, intent)
	if ok, err := f.s.reserveAIRequestFor(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "state", MaxTokens: 8, Protocol: aigateway.Responses}, f.s.ai, admit); err == nil || ok {
		t.Fatal("unowned context admitted")
	}
	var count int
	if err := f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM ai_provider_handles)+(SELECT count(*) FROM ai_gateway_requests)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial admission survived rollback", err)
	}
	id := stateValue(t, f.callState(t, "POST", stateRoot+"conversations", "guard-original-conversation", record{}, 200))["id"].(string)
	for _, sql := range []string{`DELETE FROM ai_provider_handles WHERE id=$1`, `UPDATE ai_provider_handles SET branch_id='other' WHERE id=$1`, `UPDATE ai_provider_handles SET remote_identity_sha256=repeat('a',64) WHERE id=$1`, `UPDATE ai_provider_handles SET expires_at=expires_at+interval '1 day' WHERE id=$1`, `UPDATE ai_provider_state_mutations SET state='pending' WHERE handle_id=$1`} {
		if _, err := f.s.db.Exec(f.ctx, sql, id); err == nil {
			t.Fatal("immutable provider history changed", sql)
		}
	}
	// An in-flight response pins a conversation against deletion.
	request = newID("air_")
	_, admit = f.s.prepareAIProviderResponse(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", route, aigateway.ProviderStateIntent{Conversation: id})
	if ok, err := f.s.reserveAIRequestFor(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "state", MaxTokens: 8}, f.s.ai, admit); err != nil || !ok {
		t.Fatal(err)
	}
	f.callState(t, "DELETE", stateRoot+"conversations/"+id, "delete-pinned-conversation", nil, 409)
}

func TestAIProviderStateReadOnlyReconciliationAndLifecycleRevocation(t *testing.T) {
	f := newProviderStateFixture(t)
	f.fail.Store(true)
	f.callState(t, "POST", stateRoot+"conversations", "lost-original-creation", record{}, 502)
	var id string
	if err := f.s.db.QueryRow(f.ctx, `SELECT id FROM ai_provider_handles WHERE state='uncertain'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	remote := ""
	f.mu.Lock()
	for candidate := range f.objects {
		remote = candidate
	}
	f.mu.Unlock()
	f.fail.Store(false)
	body := record{"remote_id": remote, "resolution": "adopted", "evidence_sha256": strings.Repeat("a", 64)}
	f.callState(t, "POST", stateRoot+"conversations/"+id+"/reconcile", "adopt-original-resource", body, 403)
	w := f.callState(t, "POST", stateConsole+"conversations/"+id+"/reconcile", "adopt-original-resource", body, 200)
	before := f.calls.Load()
	f.callState(t, "POST", stateConsole+"conversations/"+id+"/reconcile", "adopt-original-resource", body, 200)
	if before != f.calls.Load() || !strings.Contains(w.Body.String(), "adopted") {
		t.Fatal("reconciliation replay contacted supplier")
	}
	f.callState(t, "POST", stateRoot+"conversations", "lost-original-creation", record{}, 409)
	f.callState(t, "GET", stateRoot+"conversations/"+id, "", nil, 200)
	var terminal int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_provider_state_mutations WHERE state='reconciled' AND finished_at IS NOT NULL`).Scan(&terminal); err != nil || terminal != 1 {
		t.Fatal("reconciliation history missing", err)
	}
	f.exec(`UPDATE branches SET state='deleting' WHERE id='br_provider'`)
	var state string
	if err := f.s.db.QueryRow(f.ctx, `SELECT state FROM ai_provider_handles WHERE id=$1`, id).Scan(&state); err != nil || state != "revoked" {
		t.Fatal("branch deletion did not revoke reference", err)
	}
	f.exec(`UPDATE branches SET state='ready' WHERE id='br_provider'`)
	f.callState(t, "GET", stateRoot+"conversations/"+id, "", nil, 410)
	if _, err := f.s.db.Exec(f.ctx, `DELETE FROM ai_provider_state_reconciliations`); err == nil {
		t.Fatal("administrator evidence history was removable")
	}
}

func TestAIProviderStateTokensExpiryAndRestrictedLogin(t *testing.T) {
	f := newProviderStateFixture(t)
	var schema string
	if err := f.s.db.QueryRow(f.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := newID("state_login_")
	f.exec("CREATE ROLE " + role + " LOGIN PASSWORD 'state-ci-disposable'")
	f.exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	f.exec("GRANT SELECT(id,project_id) ON branches TO " + role)
	f.exec("GRANT SELECT(id,org_id) ON projects TO " + role)
	f.exec("GRANT SELECT ON ai_input_file_scopes,ai_gateway_requests TO " + role)
	f.exec("GRANT SELECT,INSERT ON ai_provider_handles,ai_provider_state_uses,ai_provider_state_mutations,ai_provider_state_reconciliations TO " + role)
	f.exec("GRANT UPDATE(state,provider_status,ciphertext,remote_identity_sha256,updated_at,version) ON ai_provider_handles TO " + role)
	f.exec("GRANT UPDATE(state,response_ciphertext,http_status,finished_at) ON ai_provider_state_mutations TO " + role)
	cfg, err := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "state-ci-disposable"
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	limited := &server{db: db, ai: f.s.ai, backendKeys: f.s.backendKeys}
	r := httptest.NewRequest("POST", stateRoot+"conversations", strings.NewReader(`{}`))
	r.Header.Set("Idempotency-Key", "limited-original-conversation")
	w := httptest.NewRecorder()
	limited.serveAIProviderState(w, r, "provider_org_admin", "prj_provider", "br_provider", []byte(`{}`), f.s.ai.Routes[0], f.s.ai)
	if w.Code != 200 {
		t.Fatalf("restricted state role needed excess grants: %d %s", w.Code, w.Body.String())
	}
	id := stateValue(t, w)["id"].(string)
	for _, sql := range []string{`UPDATE ai_provider_handles SET org_id='moved'`, `DELETE FROM ai_provider_handles`, `UPDATE ai_credit_accounts SET credited_micros=credited_micros+1`} {
		if _, err := db.Exec(f.ctx, sql); err == nil {
			t.Fatal("state role had excess privilege", sql)
		}
	}
	h, _, _, err := scanAIProviderHandle(f.s.db.QueryRow(f.ctx, `SELECT `+aiProviderHandleColumns+` FROM ai_provider_handles WHERE id=$1`, id))
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.s.aiProviderItemToken(h, "msg_owned")
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := f.s.aiProviderItemOpen(h, token); err != nil || opened != "msg_owned" {
		t.Fatal("owned token failed", err)
	}
	other := h
	other.Branch = "other"
	if _, err := f.s.aiProviderItemOpen(other, token); err == nil {
		t.Fatal("cross branch cursor accepted")
	}
	other = h
	other.ID = newID("ncv_")
	if _, err := f.s.aiProviderItemOpen(other, token); err == nil {
		t.Fatal("cross conversation cursor accepted")
	}
	other = h
	other.ExpiresAt = other.ExpiresAt.Add(time.Second)
	if _, err := f.s.aiProviderItemOpen(other, token); err == nil {
		t.Fatal("token lifetime was retagged")
	}
}

func TestAIProviderStateHistoryBudgetAndConcurrentContextFence(t *testing.T) {
	f := newProviderStateFixture(t)
	f.s.ai.Routes[0].Price = &aigateway.Price{Version: "state-tariff-v1", Currency: "USD", MaxInputTokens: 64, InputRate: 1000000, OutputRate: 2000000}
	f.call("provider_org_admin", "PUT", budgetPath, "state-budget-original", record{"version": 0, "enabled": true, "limit_micros": 80}, 200)
	id := stateValue(t, f.callState(t, "POST", stateRoot+"conversations", "budget-original-conversation", record{}, 200))["id"].(string)
	request := newID("air_")
	_, admit := f.s.prepareAIProviderResponse(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", f.s.ai.Routes[0], aigateway.ProviderStateIntent{Conversation: id})
	if ok, err := f.s.reserveAIRequestFor(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "state", MaxTokens: 8}, f.s.ai, admit); err != nil || !ok {
		t.Fatal(err)
	}
	budgetCounters(t, f.providerFixture, 0, 80)
	before := f.calls.Load()
	f.callState(t, "POST", stateRoot+"responses", "", record{"model": "state", "input": "short", "max_output_tokens": 8, "store": true, "conversation": id}, 429)
	if f.calls.Load() != before {
		t.Fatal("full historical input reservation was bypassed")
	}
	if err := f.s.settleAIRequest(f.ctx, request, "br_provider", aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11}}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	budgetCounters(t, f.providerFixture, 13, 0)
	f.call("provider_org_admin", "PUT", budgetPath, "state-budget-expanded", record{"version": 1, "enabled": true, "limit_micros": 1000}, 200)
	request = newID("air_")
	_, admit = f.s.prepareAIProviderResponse(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", f.s.ai.Routes[0], aigateway.ProviderStateIntent{Conversation: id})
	if ok, err := f.s.reserveAIRequestFor(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "state", MaxTokens: 8}, f.s.ai, admit); err != nil || !ok {
		t.Fatal(err)
	}
	f.callState(t, "POST", stateRoot+"responses", "", record{"model": "state", "input": "another", "max_output_tokens": 8, "conversation": id}, 409)
	if f.calls.Load() != before {
		t.Fatal("concurrent conversation inference reached supplier")
	}
	var handles int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_provider_handles WHERE kind='response'`).Scan(&handles); err != nil || handles != 0 {
		t.Fatal("denied admission left a handle", err)
	}
}

func TestAIProviderStatePublicRelayAndModelPublication(t *testing.T) {
	model := aiProviderModel{ID: "published-state", Name: "Published state", UpstreamModel: "actual", MaxOutputTokens: 32, Protocols: []string{aigateway.Responses}, AllowProviderState: true}
	if err := validateProviderPublication([]aiProviderModel{model}, []string{"actual"}, map[string]bool{}); err != nil {
		t.Fatal("state publication rejected", err)
	}
	model.Protocols = []string{"chat_completions"}
	if err := validateProviderPublication([]aiProviderModel{model}, []string{"actual"}, map[string]bool{}); err == nil {
		t.Fatal("retention was enabled without Responses")
	}
	f := newProviderStateFixture(t)
	// Keep the original private Gateway mux in its running HTTP listener;
	// route these calls through the separate control API's public LAN relay.
	f.mux = f.s.routes()
	id := stateValue(t, f.callState(t, "POST", stateRoot+"conversations", "relay-original-conversation", record{}, 200))["id"].(string)
	f.callState(t, "GET", strings.Replace(stateRoot, "/v1/", "/v1/", 1)+"conversations/"+id, "", nil, 200)
	f.callState(t, "GET", "/ai/v1/br_provider/openai/v1/conversations/"+id+"/items?limit=1&order=asc", "", nil, 200)
	f.callState(t, "DELETE", stateRoot+"conversations/"+id, "relay-delete-original", nil, 200)
	f.callState(t, "DELETE", stateRoot+"conversations/"+id, "relay-delete-original", nil, 200)
	if f.calls.Load() != 4 {
		t.Fatal("public relay changed paths, query, scope or idempotency", f.calls.Load())
	}
}
