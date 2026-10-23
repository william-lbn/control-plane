package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConsoleOIDCNetworkPinsProxyBoundaryAndSecretRotation(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("Unexpected secret at JWKS")
		}
		if r.URL.Path == "/jwks" {
			w.Header().Set("Location", "http://127.0.0.2/forbidden")
			w.WriteHeader(302)
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()
	config := consoleOIDCSettings{Version: 1, PublicOrigin: "http://console.example.test", LabHTTP: true, EgressProxy: "http://127.0.0.1:1", Providers: []*consoleOIDCProvider{{ID: "fixture", Name: "Fixture", Organization: "org_ci", Issuer: upstream.URL, AuthorizationURL: upstream.URL + "/authorize", TokenURL: upstream.URL + "/token", JWKSURL: upstream.URL + "/jwks", ClientID: "fixture", ClientSecret: "original-confidential-client-secret", AuthMethod: "client_secret_basic", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true}}}
	build := func() *consoleOIDCSettings {
		t.Helper()
		raw, _ := json.Marshal(config)
		p, e := newConsoleOIDCSettings(raw, false)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	original := build()
	client := original.Providers[0].client
	response, err := client.Get(upstream.URL + "/jwks")
	if err != nil || response.StatusCode != 302 {
		t.Fatal("LAN must bypass even an unavailable egress proxy", err)
	}
	response.Body.Close()
	if hits.Load() != 1 {
		t.Fatal("Redirect followed")
	}
	if _, err = client.Get(upstream.URL + "/unapproved"); err == nil || hits.Load() != 1 {
		t.Fatal("Unapproved path reached provider")
	}
	config.Providers[0].ClientSecret = "rotated-confidential-client-secret"
	rotated := build()
	if rotated.Providers[0].revision != original.Providers[0].revision {
		t.Fatal("Secret rotation changed identity binding")
	}
	config.Providers[0].ClientID = "another-client"
	if build().Providers[0].revision == original.Providers[0].revision {
		t.Fatal("Changed client adopted an existing identity")
	}
	config.Providers[0].AllowedIPs = []string{"127.0.0.2"}
	wrong := build()
	if _, err = wrong.Providers[0].client.Get(upstream.URL + "/jwks"); err == nil || hits.Load() != 1 {
		t.Fatal("DNS escaped exact address pin")
	}
	var target string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target = r.Host
		if r.Method != "CONNECT" {
			t.Error("Proxy request was not CONNECT")
		}
		h := w.(http.Hijacker)
		conn, buf, e := h.Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buf.Flush()
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := consoleOIDCConnect(ctx, proxyURL, "8.8.8.8:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if target != "8.8.8.8:443" {
		t.Fatal("WAN CONNECT did not use checked literal target")
	}
}

func TestConsoleSSOSessionGuardsDiscoveryExpiryAndAvailability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_sso_guards_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('guard','guard','unused','viewer')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_guard','org_guard','Guard CI')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('org_guard')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_guard','guard','admin')`)
	exec(`INSERT INTO console_sso_policies(org_id,provider_id,provider_revision,mode) VALUES('org_guard','fixture','revision-one','required')`)
	exec(`INSERT INTO console_sso_bindings(id,org_id,provider_id,provider_revision,subject,user_id) VALUES('binding','org_guard','fixture','revision-one','subject','guard')`)
	exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at,sso_binding_id,sso_policy_version) VALUES($1,'guard',$2,now()+interval '1 hour','binding',1)`, digest("sso-token"), digest("csrf"))
	exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,'guard',$2,now()+interval '1 hour')`, digest("local-token"), digest("csrf"))
	exec(`INSERT INTO api_keys(id,user_id,org_id,name,key_hash,max_role,expires_at) VALUES('machine','guard','org_guard','CI',$1,'admin',now()+interval '1 hour')`, digest("ncp_machine"))
	p := &consoleOIDCProvider{ID: "fixture", Organization: "org_guard", revision: "revision-one"}
	s := &server{db: db, consoleOIDC: &consoleOIDCSettings{PublicOrigin: "http://console.example.test", Providers: []*consoleOIDCProvider{p}, byID: map[string]*consoleOIDCProvider{"fixture": p}}}
	call := func(method, path, token string, cookies []*http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewBufferString(`{"code":"123456"}`))
		r.Header.Set("Origin", "http://console.example.test")
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: token})
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	call("GET", "/api/v1/session", "sso-token", nil, 200)
	w := call("GET", "/api/v1/organizations", "local-token", nil, 200)
	if !strings.Contains(w.Body.String(), `"console_sso_required":true`) {
		t.Fatal("Required SSO is not discoverable to a local member")
	}
	call("GET", "/api/v1/organizations/org_guard", "local-token", nil, 403)
	// Machine credentials remain scoped and authorized independently.
	r := httptest.NewRequest("GET", "/api/v1/organizations/org_guard", nil)
	r.Header.Set("Authorization", "Bearer ncp_machine")
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("Console policy changed machine authentication", w.Code, w.Body.String())
	}
	for _, change := range []string{"binding", "membership", "policy", "configuration", "disabled_user", "disabled_org"} {
		switch change {
		case "binding":
			exec(`UPDATE console_sso_bindings SET revoked_at=now()`)
		case "membership":
			exec(`DELETE FROM organization_members WHERE org_id='org_guard'`)
		case "policy":
			exec(`UPDATE console_sso_policies SET version=2`)
		case "configuration":
			p.revision = "different"
		case "disabled_user":
			exec(`UPDATE users SET disabled=true`)
		case "disabled_org":
			exec(`UPDATE organizations SET state='suspended' WHERE id='org_guard'`)
		}
		call("GET", "/api/v1/session", "sso-token", nil, 401)
		switch change {
		case "binding":
			exec(`UPDATE console_sso_bindings SET revoked_at=NULL`)
		case "membership":
			exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_guard','guard','admin')`)
		case "policy":
			exec(`UPDATE console_sso_policies SET version=1`)
		case "configuration":
			p.revision = "revision-one"
		case "disabled_user":
			exec(`UPDATE users SET disabled=false`)
		case "disabled_org":
			exec(`UPDATE organizations SET state='active' WHERE id='org_guard'`)
		}
	}
	state, browser := randomToken(32), randomToken(32)
	exec(`INSERT INTO console_sso_flows(state_hash,browser_hash,nonce_hash,key_version,sealed_verifier,org_id,provider_id,provider_revision,policy_version,purpose,expires_at) VALUES($1,$2,'nonce','v1','sealed','org_guard','fixture','revision-one',1,'login',now()-interval '1 second')`, digest(state), digest(browser))
	call("GET", "/auth/sso/callback?state="+state+"&code=expired", "", []*http.Cookie{{Name: "neon_v2_oidc", Value: browser}}, 400)
	challenge := randomToken(32)
	exec(`INSERT INTO console_sso_challenges(challenge_hash,binding_id,policy_version,expires_at,token_expires_at) VALUES($1,'binding',1,now()-interval '1 second',now()+interval '1 hour')`, digest(challenge))
	call("POST", "/auth/sso/mfa", "", []*http.Cookie{{Name: "neon_v2_sso_mfa", Value: challenge}}, 401)
	exec(`ALTER TABLE console_sso_challenges RENAME TO hidden_sso_challenges`)
	w = call("POST", "/auth/sso/mfa", "", []*http.Cookie{{Name: "neon_v2_sso_mfa", Value: challenge}}, 503)
	if strings.Contains(w.Body.String(), "hidden_sso") || strings.Contains(w.Body.String(), "SQLSTATE") {
		t.Fatal("Storage error leaked")
	}
	exec(`ALTER TABLE hidden_sso_challenges RENAME TO console_sso_challenges`)
	exec(`ALTER TABLE console_sso_bindings RENAME TO hidden_sso_bindings`)
	call("GET", "/api/v1/session", "sso-token", nil, 503)
	exec(`ALTER TABLE hidden_sso_bindings RENAME TO console_sso_bindings`)
	call("GET", "/api/v1/session", "sso-token", nil, 200)
}
