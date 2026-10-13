package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAIGatewayIntegration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, e := pgxpool.ParseConfig(dsn)
	if dsn == "" || e != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "v2_ai_" + randomToken(6)
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('alice','alice','unused','owner'),('viewer','viewer','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_ai','ai','AI')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_ai','alice','admin'),('org_ai','viewer','viewer')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_ai','org_ai','AI','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_ai','prj_ai','main','ready',now()),('br_sibling','prj_ai','sibling','ready',now())`)
	for _, actor := range []string{"alice", "viewer"} {
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	calls := atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"object":"chat.completion","choices":[{"message":{"content":"actual upstream"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()
	s := &server{db: db, backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{0x31}, 32)}}, ai: &aigateway.Config{Version: 1, Generation: 1, TimeoutSeconds: 5, MaxConcurrentPerBranch: 1, RequestsPerMinute: 12, Routes: []aigateway.Route{{ID: "small", Name: "Small", Provider: "openai_compatible", BaseURL: upstream.URL + "/v1", UpstreamModel: "qwen2.5:0.5b", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true, MaxOutputTokens: 128}}}}
	gatewayMux := http.NewServeMux()
	gatewayMux.HandleFunc("POST /ai/v1/{branch}/v1/chat/completions", s.aiPublic)
	gatewayMux.HandleFunc("GET /ai/v1/{branch}/v1/models", s.aiPublic)
	gateway := httptest.NewServer(gatewayMux)
	defer gateway.Close()
	t.Setenv("NEON_AI_GATEWAY_URL", gateway.URL)
	body := []byte(`{"model":"small","messages":[{"role":"user","content":"private prompt"}],"max_tokens":32}`)
	call := func(actor, method, path, token string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if actor != "" {
			r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
			r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
			r.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/api/") {
			s.routes().ServeHTTP(w, r)
		} else {
			gatewayMux.ServeHTTP(w, r)
		}
		if w.Code != want {
			t.Fatalf("status %d expected %d", w.Code, want)
		}
		return w
	}
	call("viewer", "POST", "/api/v1/projects/prj_ai/branches/br_ai/ai-gateway/request", "", 403)
	call("", "POST", "/ai/v1/br_ai/v1/chat/completions", "", 401)
	if calls.Load() != 0 {
		t.Fatal("unauthorized inference reached provider")
	}
	reply := call("alice", "POST", "/api/v1/projects/prj_ai/branches/br_ai/ai-gateway/request", "", 200)
	services := call("alice", "GET", "/api/v1/projects/prj_ai/branches/br_ai/services", "", 200)
	if !strings.Contains(services.Body.String(), `"observed_state":"configured"`) || !strings.Contains(services.Body.String(), `"driver_version":"go-ai-text-chat-v1"`) {
		t.Fatal("installed inference still advertised as an unimplemented branch service")
	}
	if !strings.Contains(reply.Body.String(), "actual upstream") || calls.Load() != 1 {
		t.Fatal("Console did not invoke real independent gateway")
	}
	var state string
	var tokens int64
	if db.QueryRow(ctx, `SELECT state,total_tokens FROM ai_gateway_requests`).Scan(&state, &tokens) != nil || state != "completed" || tokens != 5 {
		t.Fatal("durable settlement missing")
	}
	usage := call("viewer", "GET", "/api/v1/projects/prj_ai/branches/br_ai/ai-gateway/usage", "", 200)
	if strings.Contains(usage.Body.String(), "private prompt") || strings.Contains(usage.Body.String(), "actual upstream") {
		t.Fatal("content leaked to usage")
	}
	id := "bcr_" + randomToken(12)
	token := "ncb_" + id[4:] + "." + randomToken(32)
	exec(`INSERT INTO backend_credentials(id,project_id,org_id,branch_id,issuer_id,name,scopes,branch_scope,allowed_models,token_hash,pepper_version,expires_at) VALUES($1,'prj_ai','org_ai','br_ai','alice','server',ARRAY['ai_gateway:invoke'],'self',ARRAY['small'],$2,'v1',now()+interval '1 hour')`, id, s.backendHash("backend-token", token, "v1"))
	call("", "POST", "/ai/v1/br_sibling/v1/chat/completions", token, 403)
	call("", "POST", "/ai/v1/br_ai/v1/chat/completions", token, 200)
	ticket := s.signAITicket("alice", "br_ai", "small", body)
	if _, e = s.verifyAITicket(ticket, append(body, ' '), "br_ai", "small"); e == nil {
		t.Fatal("ticket payload substitution accepted")
	}
	if _, e = s.verifyAITicket(ticket, body, "br_sibling", "small"); e == nil {
		t.Fatal("ticket branch substitution accepted")
	}
	exec(`UPDATE backend_credentials SET revoked_at=now() WHERE id=$1`, id)
	call("", "POST", "/ai/v1/br_ai/v1/chat/completions", token, 401)
	if calls.Load() != 2 {
		t.Fatal("revoked or cross-branch request reached provider")
	}
	exec(`UPDATE organization_members SET role='viewer' WHERE user_id='alice'`)
	r := httptest.NewRequest("POST", "/ai/v1/br_ai/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("X-Neon-Console-Ticket", ticket)
	w := httptest.NewRecorder()
	gatewayMux.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("demoted Console ticket accepted")
	}
	exec(`UPDATE organization_members SET role='admin' WHERE user_id='alice'`)
	var group sync.WaitGroup
	admitted := atomic.Int64{}
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			ok, e := s.reserveAIRequest(ctx, newID("air_"), "prj_ai", "br_ai", "alice", "", aigateway.ChatRequest{Model: "small"})
			if e != nil {
				t.Error(e)
			}
			if ok {
				admitted.Add(1)
			}
		}()
	}
	group.Wait()
	if admitted.Load() != 1 {
		t.Fatal("cross-instance branch admission oversubscribed")
	}
	call("alice", "POST", "/api/v1/projects/prj_ai/branches/br_ai/ai-gateway/request", "", 429)
	if calls.Load() != 2 {
		t.Fatal("limited request reached provider")
	}
	var data map[string]any
	if json.Unmarshal(usage.Body.Bytes(), &data) != nil {
		t.Fatal("invalid usage contract")
	}
}
