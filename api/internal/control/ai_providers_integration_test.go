package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

const providerTestKey = "disposable-provider-original-key-0123456789"

type providerFixture struct {
	t              *testing.T
	s              *server
	ctx            context.Context
	peer           *httptest.Server
	gets, posts    atomic.Int64
	metadataStatus atomic.Int64
	key            atomic.Value
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "v2_ai_provider_" + randomToken(5)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 10
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := &providerFixture{t: t, ctx: ctx}
	f.key.Store(providerTestKey)
	f.peer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.key.Load().(string) {
			w.WriteHeader(401)
			fmt.Fprint(w, "never expose provider response or key")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/v1/models" {
			f.gets.Add(1)
			if code := f.metadataStatus.Load(); code != 0 {
				w.WriteHeader(int(code))
				return
			}
			fmt.Fprint(w, `{"object":"list","data":[{"id":"model:small"},{"id":"model:other"}]}`)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/chat/completions" {
			f.posts.Add(1)
			fmt.Fprint(w, `{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"disposable-provider-reply"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(f.peer.Close)
	f.s = &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), idempotencyKey: bytes.Repeat([]byte{0x51}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{0x31}, 32)}}, aiProviders: providerSettingsFixture(t, f.peer.URL+"/v1", true), ai: &aigateway.Config{Version: 1, Generation: 3, TimeoutSeconds: 5, MaxConcurrentPerBranch: 2, RequestsPerMinute: 100, Routes: []aigateway.Route{{ID: "static-route", Name: "Preserved static route", Provider: "openai_compatible", BaseURL: f.peer.URL + "/v1", UpstreamModel: "model:small", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true, MaxOutputTokens: 32}}}}
	f.exec(`INSERT INTO users(id,username,password_hash,role) VALUES('provider_operator','provider_operator','unused','viewer'),('provider_org_admin','provider_org_admin','unused','owner')`)
	f.exec(`INSERT INTO organizations(id,slug,name) VALUES('org_provider','provider','Provider')`)
	f.exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_provider','provider_operator','admin'),('org_provider','provider_org_admin','admin')`)
	f.exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_provider','org_provider','Provider','lab',16,'ready','managed',now(),now())`)
	f.exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_provider','prj_provider','main','ready',now())`)
	for _, actor := range []string{"provider_operator", "provider_org_admin"} {
		f.exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	return f
}
func (f *providerFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.s.db.Exec(f.ctx, q, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *providerFixture) request(actor, method, path, key string, input any) *httptest.ResponseRecorder {
	var raw []byte
	if input != nil {
		raw, _ = json.Marshal(input)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if actor != "" {
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	return w
}
func (f *providerFixture) call(actor, method, path, key string, input any, want int) record {
	f.t.Helper()
	w := f.request(actor, method, path, key, input)
	if w.Code != want {
		f.t.Fatalf("%s %s: status %d expected %d, public body %s", method, path, w.Code, want, w.Body.String())
	}
	if strings.Contains(w.Body.String(), providerTestKey) || strings.Contains(w.Body.String(), `"ciphertext"`) {
		f.t.Fatal("provider key or sealed box escaped public response")
	}
	var value record
	if json.Unmarshal(w.Body.Bytes(), &value) != nil {
		f.t.Fatal("invalid public JSON")
	}
	return value
}
func (f *providerFixture) draft(version int64, mode, secret string) aiProviderDraftInput {
	return aiProviderDraftInput{Version: version, Name: "CI provider", BaseURL: f.peer.URL + "/v1", Credential: aiProviderCredentialInput{Mode: mode, Secret: secret, LabAcknowledged: true}}
}
func (f *providerFixture) work() {
	f.t.Helper()
	worked, err := f.s.aiProviderWorkOnce(f.ctx)
	if err != nil || !worked {
		f.t.Fatal("provider worker failed", err)
	}
}
func providerTestModels(alias string) []aiProviderModel {
	return []aiProviderModel{{ID: alias, Name: "CI small model", UpstreamModel: "model:small", MaxOutputTokens: 32}}
}
func (f *providerFixture) invoke(ticket string, want int) {
	f.t.Helper()
	body := []byte(`{"model":"provider-small","messages":[{"role":"user","content":"test prompt"}],"max_tokens":16}`)
	if ticket == "" {
		snapshot, err := f.s.aiCatalog(f.ctx, false)
		if err != nil {
			f.t.Fatal(err)
		}
		ticket = f.s.signAITicketFor("provider_operator", "br_provider", "provider-small", body, snapshot.Config.Generation)
	}
	r := httptest.NewRequest("POST", "/ai/v1/br_provider/v1/chat/completions", bytes.NewReader(body))
	r.SetPathValue("branch", "br_provider")
	r.Header.Set("X-Neon-Console-Ticket", ticket)
	w := httptest.NewRecorder()
	f.s.aiPublic(w, r)
	if w.Code != want {
		f.t.Fatalf("inference status %d expected %d: %s", w.Code, want, w.Body.String())
	}
}

func TestAIProviderCatalogLifecycleAndAdmission(t *testing.T) {
	f := newProviderFixture(t)
	base := "/api/v1/platform/ai-providers"
	actor := "provider_operator"
	f.call("", "GET", base, "", nil, 401)
	f.call("provider_org_admin", "GET", base, "", nil, 403)
	cap := f.call("provider_org_admin", "GET", base+"/capabilities", "", nil, 200)
	if cap["can_manage"] != false {
		t.Fatal("organization admin gained operator rights")
	}
	input := f.draft(0, "sealed", providerTestKey)
	created := f.call(actor, "POST", base, "create-provider-once", input, 202)
	id, revision, op := stringVal(created["provider_id"]), stringVal(created["revision_id"]), stringVal(created["operation_id"])
	replay := f.request(actor, "POST", base, "create-provider-once", input)
	if replay.Code != 202 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("lost create response was not replayed")
	}
	changed := input
	changed.Name = "Different"
	f.call(actor, "POST", base, "create-provider-once", changed, 409)
	f.call(actor, "POST", base+"/"+id+"/publish", "publish-before-test", aiProviderPublishInput{Version: 1, RevisionID: revision, Models: providerTestModels("provider-small")}, 409)
	if f.posts.Load() != 0 || f.gets.Load() != 0 {
		t.Fatal("save or failed publish invoked provider")
	}
	f.work()
	operation := f.call(actor, "GET", "/api/v1/platform/ai-provider-operations/"+op, "", nil, 200)
	if operation["state"] != "succeeded" || f.posts.Load() != 0 || f.gets.Load() != 1 {
		t.Fatal("metadata test did not complete without inference")
	}
	f.call(actor, "POST", base+"/"+id+"/publish", "static-alias-rejected", aiProviderPublishInput{Version: 1, RevisionID: revision, Models: providerTestModels("static-route")}, 409)
	f.call(actor, "POST", base+"/"+id+"/publish", "publish-original", aiProviderPublishInput{Version: 1, RevisionID: revision, Models: providerTestModels("provider-small")}, 200)
	f.invoke("", 200)
	if f.posts.Load() != 1 {
		t.Fatal("published route did not reach actual disposable upstream")
	}
	snapshot, err := f.s.aiCatalog(f.ctx, true)
	if err != nil || snapshot.Config.Generation != 4 || len(snapshot.Config.Routes) != 2 {
		t.Fatal("dynamic route or static route missing", err)
	}
	originalRoute, _ := snapshot.Config.Find("provider-small")
	wire, _ := json.Marshal(originalRoute)
	if bytes.Contains(wire, []byte(providerTestKey)) {
		t.Fatal("opened route serialized key")
	}
	// A failed key rotation preserves the old publication and original credential.
	bad := f.draft(2, "sealed", "disposable-wrong-key-0123456789012345")
	failed := f.call(actor, "POST", base+"/"+id+"/revisions", "failed-key-rotation", bad, 202)
	f.work()
	failOp := f.call(actor, "GET", "/api/v1/platform/ai-provider-operations/"+stringVal(failed["operation_id"]), "", nil, 200)
	if failOp["error_code"] != "provider_authentication_rejected" || failOp["retryable"] != false {
		t.Fatal("typed authentication failure missing")
	}
	f.call(actor, "POST", base+"/"+id+"/publish", "reject-failed-rotation", aiProviderPublishInput{Version: 3, RevisionID: stringVal(failed["revision_id"]), Models: providerTestModels("provider-small")}, 409)
	f.invoke("", 200)
	// Stale browser version fails; re-discovery explicitly rewraps the tested key.
	reused := f.draft(3, "reuse", "")
	reused.Credential.ReuseRevisionID = revision
	good := f.call(actor, "POST", base+"/"+id+"/revisions", "rediscover-original-key", reused, 202)
	f.work()
	f.call(actor, "POST", base+"/"+id+"/revisions", "stale-provider-version", reused, 409)
	f.call(actor, "POST", base+"/"+id+"/publish", "publish-rediscovery", aiProviderPublishInput{Version: 4, RevisionID: stringVal(good["revision_id"]), Models: providerTestModels("provider-small")}, 200)
	if _, err = f.s.db.Exec(f.ctx, `UPDATE ai_provider_revisions SET models='[]' WHERE id=$1`, revision); err == nil {
		t.Fatal("tested receipt changed")
	}
	if _, err = f.s.db.Exec(f.ctx, `UPDATE ai_provider_publications SET models='[]'`); err == nil {
		t.Fatal("publication history changed")
	}
	// Generation recheck prevents an already prepared old catalog from admission.
	if _, err = f.s.reserveAIRequestFor(f.ctx, newID("air_"), "prj_provider", "br_provider", actor, "", aigateway.ChatRequest{Model: "provider-small"}, snapshot.Config); err != errAIProviderCatalogChanged {
		t.Fatal("stale catalog admitted inference", err)
	}
	body := []byte(`{"model":"provider-small","messages":[{"role":"user","content":"test prompt"}],"max_tokens":16}`)
	oldTicket := f.s.signAITicketFor(actor, "br_provider", "provider-small", body, snapshot.Config.Generation)
	before := f.posts.Load()
	f.invoke(oldTicket, 409)
	if f.posts.Load() != before {
		t.Fatal("stale Console ticket dispatched")
	}
	f.call(actor, "GET", base+"/"+id, "", nil, 200)
	f.call(actor, "POST", base+"/"+id+"/disable", "disable-original", aiProviderVersionInput{Version: 5}, 200)
	disabled, err := f.s.aiCatalog(f.ctx, true)
	if err != nil || len(disabled.Config.Routes) != 1 || disabled.Config.Routes[0].ID != "static-route" {
		t.Fatal("disable altered static catalog", err)
	}
	f.invoke("", 400)
	if f.posts.Load() != before {
		t.Fatal("disabled provider dispatched")
	}
	f.call(actor, "DELETE", base+"/"+id, "delete-definition", aiProviderVersionInput{Version: 6}, 200)
	f.call(actor, "GET", base+"/"+id, "", nil, 404)
	var retained int
	f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_provider_publications WHERE provider_id=$1`, id).Scan(&retained)
	if retained != 2 {
		t.Fatal("deleted definition lost publication history")
	}
	var plaintext bool
	if err = f.s.db.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM ai_provider_revisions WHERE credential_box::text LIKE '%'||$1||'%') OR EXISTS(SELECT 1 FROM ai_provider_requests WHERE request_hash LIKE '%'||$1||'%' OR response::text LIKE '%'||$1||'%')`, providerTestKey).Scan(&plaintext); err != nil || plaintext {
		t.Fatal("raw key persisted", err)
	}
}

func TestAIProviderDiscoveryRetrySupersessionAndConcurrentAliases(t *testing.T) {
	f := newProviderFixture(t)
	base := "/api/v1/platform/ai-providers"
	actor := "provider_operator"
	f.metadataStatus.Store(503)
	first := f.call(actor, "POST", base, "create-transient-provider", f.draft(0, "sealed", providerTestKey), 202)
	f.work()
	id, revision, op := stringVal(first["provider_id"]), stringVal(first["revision_id"]), stringVal(first["operation_id"])
	failed := f.call(actor, "GET", "/api/v1/platform/ai-provider-operations/"+op, "", nil, 200)
	if failed["retryable"] != true {
		t.Fatal("transient failure did not retain explicit retry")
	}
	f.metadataStatus.Store(0)
	f.call(actor, "POST", "/api/v1/platform/ai-provider-operations/"+op+"/retry", "retry-original-operation", record{}, 202)
	f.work()
	complete := f.call(actor, "GET", "/api/v1/platform/ai-provider-operations/"+op, "", nil, 200)
	if complete["state"] != "succeeded" || complete["attempts"] != float64(2) {
		t.Fatal("retry replaced original operation or attempt history")
	}
	second := f.call(actor, "POST", base, "second-provider", f.draft(0, "sealed", providerTestKey), 202)
	f.work()
	ids := []string{id, stringVal(second["provider_id"])}
	revisions := []string{revision, stringVal(second["revision_id"])}
	codes := make(chan int, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(at int) {
			defer group.Done()
			codes <- f.request(actor, "POST", base+"/"+ids[at]+"/publish", fmt.Sprintf("parallel-alias-%d", at), aiProviderPublishInput{Version: 1, RevisionID: revisions[at], Models: providerTestModels("one-global-alias")}).Code
		}(i)
	}
	group.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal("concurrent alias collision was not serialized", counts)
	}
	// Queued operation from a disabled actor performs no metadata access.
	last := f.call(actor, "POST", base, "actor-revoked-provider", f.draft(0, "sealed", providerTestKey), 202)
	f.exec(`UPDATE users SET disabled=true WHERE id=$1`, actor)
	before := f.gets.Load()
	f.work()
	var state, code string
	if err := f.s.db.QueryRow(f.ctx, `SELECT state,error_code FROM ai_provider_operations WHERE id=$1`, last["operation_id"]).Scan(&state, &code); err != nil || state != "failed" || code != "provider_operation_superseded" || f.gets.Load() != before {
		t.Fatal("revoked operator invoked metadata", err)
	}
}

func TestAIProviderCredentialSourceUIDAndEndpointScope(t *testing.T) {
	f := newProviderFixture(t)
	var replaced atomic.Bool
	var secretReads atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secretReads.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/neon/secrets/approved-key" {
			t.Error("source made unexpected Kubernetes request")
			w.WriteHeader(403)
			return
		}
		uid := "11111111-2222-4333-8444-555555555555"
		if replaced.Load() {
			uid = "22222222-2222-4333-8444-555555555555"
		}
		json.NewEncoder(w).Encode(record{"type": "Opaque", "immutable": true, "metadata": record{"name": "approved-key", "namespace": "neon", "uid": uid}, "data": record{"provider-key": base64.StdEncoding.EncodeToString([]byte(providerTestKey))}})
	}))
	defer peer.Close()
	f.s.kube = &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	source := aiProviderCredentialSource{ID: "approved", Name: "CI approved credential", Namespace: "neon", SecretName: "approved-key", SecretUID: "11111111-2222-4333-8444-555555555555", DataKey: "provider-key", AllowedBaseURLs: []string{f.peer.URL + "/v1"}}
	f.s.aiProviders.sources[source.ID] = source
	f.s.aiProviders.policy.CredentialSources = []aiProviderCredentialSource{source}
	input := f.draft(0, "operator_reference", "")
	input.Credential.SourceID = source.ID
	base := "/api/v1/platform/ai-providers"
	actor := "provider_operator"
	first := f.call(actor, "POST", base, "fixed-source-original", input, 202)
	f.work()
	var uid string
	if err := f.s.db.QueryRow(f.ctx, `SELECT source_secret_uid FROM ai_provider_revisions WHERE id=$1 AND state='tested'`, first["revision_id"]).Scan(&uid); err != nil || uid != source.SecretUID || secretReads.Load() != 1 {
		t.Fatal("original credential source receipt missing", err)
	}
	replaced.Store(true)
	second := f.call(actor, "POST", base, "fixed-source-replaced", input, 202)
	before := f.gets.Load()
	f.work()
	operation := f.call(actor, "GET", "/api/v1/platform/ai-provider-operations/"+stringVal(second["operation_id"]), "", nil, 200)
	if operation["state"] != "failed" || operation["error_code"] != "provider_credential_unavailable" || f.gets.Load() != before {
		t.Fatal("replacement credential source was adopted")
	}
	input.BaseURL = "http://127.0.0.1:1/v1"
	f.call(actor, "POST", base, "source-cross-endpoint", input, 422)
	reuse := f.draft(1, "reuse", "")
	reuse.Credential.ReuseRevisionID = stringVal(first["revision_id"])
	reuse.BaseURL = "http://127.0.0.1:2/v1"
	f.call(actor, "POST", base+"/"+stringVal(first["provider_id"])+"/revisions", "reuse-cross-endpoint", reuse, 422)
}

func TestAIProviderCatalogRestrictedRoleAndPublicationFence(t *testing.T) {
	f := newProviderFixture(t)
	actor := "provider_operator"
	base := "/api/v1/platform/ai-providers"
	created := f.call(actor, "POST", base, "restricted-reader-provider", f.draft(0, "sealed", providerTestKey), 202)
	f.work()
	id, revision := stringVal(created["provider_id"]), stringVal(created["revision_id"])
	f.call(actor, "POST", base+"/"+id+"/publish", "restricted-reader-publish", aiProviderPublishInput{Version: 1, RevisionID: revision, Models: providerTestModels("provider-small")}, 200)
	var schema string
	if err := f.s.db.QueryRow(f.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := newID("apread_")
	f.exec("CREATE ROLE " + role + " LOGIN PASSWORD 'provider-ci-disposable'")
	f.exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	f.exec("GRANT SELECT ON ai_provider_catalog_state,ai_provider_runtime,ai_gateway_requests TO " + role)
	f.exec("GRANT INSERT ON ai_gateway_requests TO " + role)
	f.exec("GRANT SELECT ON ai_budget_policies TO " + role)
	f.exec("GRANT UPDATE(reserved_micros) ON ai_budget_policies TO " + role)
	cfg, err := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "provider-ci-disposable"
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	reader, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	limited := &server{db: reader, ai: f.s.ai, aiProviders: f.s.aiProviders, backendKeys: f.s.backendKeys}
	snapshot, err := limited.aiCatalog(f.ctx, true)
	if err != nil || len(snapshot.Config.Routes) != 2 {
		t.Fatal("restricted Gateway could not read active catalog", err)
	}
	if _, err = reader.Exec(f.ctx, `UPDATE ai_provider_catalog_state SET revision=revision+1`); err == nil {
		t.Fatal("Gateway could mutate catalog generation")
	}
	if _, err = reader.Exec(f.ctx, `SELECT credential_box FROM ai_provider_revisions`); err == nil {
		t.Fatal("Gateway could read unpublished credential boxes")
	}
	ok, err := limited.reserveAIRequestFor(f.ctx, newID("air_"), "prj_provider", "br_provider", actor, "", aigateway.ChatRequest{Model: "provider-small"}, snapshot.Config)
	if err != nil || !ok {
		t.Fatal("catalog admission required forbidden UPDATE privilege", err)
	}
	// A publisher waits for an already admitted transaction, then invalidates
	// old snapshots. It never waits for the entire paid inference to finish.
	tx, err := reader.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock_shared(794210047)`); err != nil {
		t.Fatal(err)
	}
	codes := make(chan int, 1)
	go func() {
		codes <- f.request(actor, "POST", base+"/"+id+"/disable", "restricted-reader-disable", aiProviderVersionInput{Version: 2}).Code
	}()
	select {
	case code := <-codes:
		t.Fatal("publication bypassed in-progress admission", code)
	case <-time.After(50 * time.Millisecond):
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-codes:
		if code != 200 {
			t.Fatal("disable failed after admission committed", code)
		}
	case <-f.ctx.Done():
		t.Fatal("publisher did not resume")
	}
	if _, err = limited.reserveAIRequestFor(f.ctx, newID("air_"), "prj_provider", "br_provider", actor, "", aigateway.ChatRequest{Model: "provider-small"}, snapshot.Config); err != errAIProviderCatalogChanged {
		t.Fatal("restricted Gateway admitted disabled snapshot", err)
	}
}
