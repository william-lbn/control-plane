package control

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

func TestAINativeProtocolsBranchAdmission(t *testing.T) {
	f := newProviderFixture(t)
	s := f.s
	s.aiProviders = nil
	var calls atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"native-fixture"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
	}))
	defer peer.Close()
	s.ai.Routes = []aigateway.Route{{ID: "native", Name: "Native", Provider: "openai_compatible", BaseURL: peer.URL + "/v1", UpstreamModel: "actual", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true, MaxOutputTokens: 32, Protocols: []string{aigateway.Responses, aigateway.Messages}}}
	mux := http.NewServeMux()
	registerAINativePublic(mux, "branch", s.aiPublic)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	t.Setenv("NEON_AI_GATEWAY_URL", gateway.URL)
	router := s.routes()
	body := []byte(`{"model":"native","input":"private-native-prompt","max_output_tokens":8}`)
	call := func(path, actor, token, ticket string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", path, bytes.NewReader(body))
		if actor != "" {
			r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
			r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
			r.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if ticket != "" {
			r.Header.Set("X-Neon-Console-Ticket", ticket)
		}
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/api/") {
			router.ServeHTTP(w, r)
		} else {
			mux.ServeHTTP(w, r)
		}
		if w.Code != want {
			t.Fatalf("%s status=%d want=%d: %s", path, w.Code, want, w.Body)
		}
		return w
	}
	console := "/api/v1/projects/prj_provider/branches/br_provider/ai-gateway/request/responses"
	public := "/ai/v1/br_provider/openai/v1/responses"
	call(public, "", "", "", 401)
	w := call(console, "provider_operator", "", "", 200)
	if !strings.Contains(w.Body.String(), "native-fixture") || calls.Load() != 1 {
		t.Fatal("Console did not invoke native gateway")
	}
	var state string
	var total int64
	if s.db.QueryRow(f.ctx, `SELECT state,total_tokens FROM ai_gateway_requests`).Scan(&state, &total) != nil || state != "completed" || total != 5 {
		t.Fatal("native result not durably settled")
	}
	id := newID("bcr_")
	token := "ncb_" + id[4:] + "." + randomToken(32)
	f.exec(`INSERT INTO backend_credentials(id,project_id,org_id,branch_id,issuer_id,name,scopes,branch_scope,allowed_models,token_hash,pepper_version,expires_at) VALUES($1,'prj_provider','org_provider','br_provider','provider_operator','native',ARRAY['ai_gateway:invoke'],'self',ARRAY['native'],$2,'v1',now()+interval '1 hour')`, id, s.backendHash("backend-token", token, "v1"))
	call(public, "", token, "", 200)
	f.exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_native_sibling','prj_provider','sibling','ready',now())`)
	call("/ai/v1/br_native_sibling/v1/responses", "", token, "", 403)
	call(public, "", "", s.signAITicket("provider_operator", "br_provider", "native", body), 401)
	f.exec(`UPDATE backend_credentials SET revoked_at=now() WHERE id=$1`, id)
	call(public, "", token, "", 401)
	f.exec(`UPDATE organization_members SET role='viewer' WHERE user_id='provider_operator'`)
	call(console, "provider_operator", "", "", 403)
	if calls.Load() != 2 {
		t.Fatal("denied native request reached upstream")
	}
	var leaked bool
	if s.db.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM ai_gateway_requests WHERE row_to_json(ai_gateway_requests)::text LIKE '%private-native-prompt%' OR row_to_json(ai_gateway_requests)::text LIKE '%native-fixture%')`).Scan(&leaked) != nil || leaked {
		t.Fatal("content leaked into metadata")
	}
}

func TestAINativeSDKHeadersAndRelayPaths(t *testing.T) {
	for _, tc := range []struct{ path, header, protocol string }{
		{"/ai/v1/br/anthropic/v1/messages", "x-api-key", aigateway.Messages},
		{"/ai/v1/br/gemini/v1beta/models/alias:streamGenerateContent?alt=sse", "x-goog-api-key", aigateway.Gemini},
	} {
		mux := http.NewServeMux()
		registerAINativePublic(mux, "branch", func(w http.ResponseWriter, r *http.Request) {
			p, m, stream := aiNativeRequestRoute(r)
			if p != tc.protocol || aiAuthorization(r) != "Bearer ncb_fixture" {
				t.Error("SDK branch credential not resolved")
			}
			if p == aigateway.Gemini && (m != "alias" || !stream) {
				t.Error("Gemini path lost")
			}
			w.WriteHeader(204)
		})
		r := httptest.NewRequest("POST", tc.path, nil)
		r.Header.Set(tc.header, "ncb_fixture")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatal("native public route missing")
		}
	}
}
