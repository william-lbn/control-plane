package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"
)

type consoleTestOIDC struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	mu        sync.Mutex
	codes     map[string]url.Values
	exchanges int
}

func newConsoleTestOIDC(t *testing.T) *consoleTestOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &consoleTestOIDC{key: key, codes: map[string]url.Values{}}
	i.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jwks":
			jsonResponse(w, 200, record{"keys": []any{record{"kty": "RSA", "kid": "fixture-1", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		case "/token":
			i.mu.Lock()
			defer i.mu.Unlock()
			i.exchanges++
			if r.ParseForm() != nil {
				w.WriteHeader(400)
				return
			}
			client, secret, ok := r.BasicAuth()
			q, exists := i.codes[r.Form.Get("code")]
			delete(i.codes, r.Form.Get("code"))
			h := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !exists || !ok || client != "console-ci" || secret != "fixture-oidc-client-secret" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != "http://console.example.test/auth/sso/callback" || base64.RawURLEncoding.EncodeToString(h[:]) != q.Get("code_challenge") {
				w.WriteHeader(400)
				return
			}
			now := time.Now()
			claims := jwt.MapClaims{"iss": i.server.URL, "aud": "console-ci", "azp": "console-ci", "sub": q.Get("fixture_subject"), "nonce": q.Get("nonce"), "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "email": "same-address@example.test", "email_verified": true}
			switch q.Get("fixture_fault") {
			case "nonce":
				claims["nonce"] = "wrong"
			case "issuer":
				claims["iss"] = "https://other.example.test"
			case "audience":
				claims["aud"] = "other-client"
			case "azp":
				claims["azp"] = "other-client"
			case "expiry":
				claims["exp"] = now.Add(-time.Minute).Unix()
			case "future_iat":
				claims["iat"] = now.Add(time.Hour).Unix()
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "fixture-1"
			signed, e := token.SignedString(key)
			if e != nil {
				w.WriteHeader(500)
				return
			}
			jsonResponse(w, 200, record{"access_token": "fixture-access-token-never-used", "token_type": "Bearer", "expires_in": 600, "id_token": signed})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(i.server.Close)
	return i
}
func (i *consoleTestOIDC) code(t *testing.T, authorization, subject, fault string) string {
	t.Helper()
	u, err := url.Parse(authorization)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.String() == "" || q.Get("response_type") != "code" || q.Get("scope") != "openid" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || len(q.Get("nonce")) != 64 || q.Get("redirect_uri") != "http://console.example.test/auth/sso/callback" {
		t.Fatal("Invalid authorization/PKCE/nonce request")
	}
	q.Set("fixture_subject", subject)
	q.Set("fixture_fault", fault)
	code := randomToken(12)
	i.mu.Lock()
	i.codes[code] = q
	i.mu.Unlock()
	return "/auth/sso/callback?" + url.Values{"state": {q.Get("state")}, "code": {code}}.Encode()
}

func TestConsoleSSOCodeBindingPolicyRevocationAndMFA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
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
	schema := "v2_sso_" + randomToken(6)
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	pwd := "Console-OIDC-test-password-unique-31"
	for _, name := range []string{"alice", "bob"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,$2,'viewer')`, name, hashPassword(pwd))
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(name+"-local"), name, digest(name+"-csrf"))
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_sso','org_sso','OIDC CI'),('org_other','org_other','Other CI')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('org_sso'),('org_other')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_sso','alice','admin'),('org_sso','bob','viewer'),('org_other','alice','admin')`)
	idp := newConsoleTestOIDC(t)
	input := consoleOIDCSettings{Version: 1, PublicOrigin: "http://console.example.test", LabHTTP: true, Providers: []*consoleOIDCProvider{{ID: "ci-oidc", Name: "Local CI IdP", Organization: "org_sso", Issuer: idp.server.URL, AuthorizationURL: idp.server.URL + "/authorize", TokenURL: idp.server.URL + "/token", JWKSURL: idp.server.URL + "/jwks", ClientID: "console-ci", ClientSecret: "fixture-oidc-client-secret", AuthMethod: "client_secret_basic", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true}}}
	raw, _ := json.Marshal(input)
	settings, err := newConsoleOIDCSettings(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{db: db, consoleOIDC: settings, idempotencyKey: bytes.Repeat([]byte{9}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{7}, 32)}}}
	local := func(name string) []*http.Cookie {
		return []*http.Cookie{{Name: "neon_v2_session", Value: name + "-local"}, {Name: "neon_v2_csrf", Value: name + "-csrf"}}
	}
	call := func(method, path string, body any, cookies []*http.Cookie, extra map[string]string, want int) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", input.PublicOrigin)
		for _, c := range cookies {
			r.AddCookie(c)
			if c.Name == "neon_v2_csrf" {
				r.Header.Set("X-CSRF-Token", c.Value)
			}
		}
		for k, v := range extra {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s got %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	base := "/api/v1/organizations/org_sso/sso"
	policy := func(mode, version, key string, cookies []*http.Cookie, want int) {
		t.Helper()
		call("PUT", base, record{"provider_id": "ci-oidc", "mode": mode, "password": pwd}, cookies, map[string]string{"If-Match": `"` + version + `"`, "Idempotency-Key": key}, want)
	}
	policy("optional", "0", "sso-policy-initial", local("bob"), 403)
	policy("optional", "0", "sso-policy-initial", local("alice"), 200)
	policy("optional", "0", "sso-policy-initial", local("alice"), 200) // Durable receipt before stale If-Match.
	policy("required", "1", "sso-policy-unsafe", local("alice"), 409)
	flow := func(link bool, actor string) (string, []*http.Cookie) {
		t.Helper()
		path := "/auth/sso/start"
		var cookies []*http.Cookie
		if link {
			path = base + "/link"
			cookies = local(actor)
		}
		w := call("POST", path, record{"organization_id": "org_sso", "password": pwd}, cookies, nil, 200)
		var result record
		if json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal("Invalid start response")
		}
		return result["authorization_url"].(string), w.Result().Cookies()
	}
	authorization, browser := flow(false, "")
	path := idp.code(t, authorization, "unbound", "")
	before := idp.exchanges
	call("GET", path, nil, nil, nil, 400)
	if idp.exchanges != before {
		t.Fatal("Unbound browser exchanged a code")
	}
	call("GET", path, nil, browser, nil, 401)
	for _, fault := range []string{"nonce", "issuer", "audience", "azp", "expiry", "future_iat"} {
		authorization, browser = flow(true, "alice")
		path = idp.code(t, authorization, "subject-alice", fault)
		call("GET", path, nil, browser, nil, 401)
		call("GET", path, nil, browser, nil, 400)
	}
	authorization, browser = flow(true, "alice")
	path = idp.code(t, authorization, "subject-alice", "")
	call("GET", path, nil, browser, nil, 303)
	call("GET", path, nil, browser, nil, 400)
	var n int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM console_sso_bindings WHERE user_id='alice' AND revoked_at IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatal("Identity not explicitly bound", err, n)
	}
	// Same email does not merge accounts or grant membership/roles.
	authorization, browser = flow(true, "bob")
	call("GET", idp.code(t, authorization, "subject-alice", ""), nil, browser, nil, 409)
	authorization, browser = flow(false, "")
	call("GET", idp.code(t, authorization, "same-email-other-subject", ""), nil, browser, nil, 401)
	loginSSO := func() []*http.Cookie {
		t.Helper()
		auth, bc := flow(false, "")
		w := call("GET", idp.code(t, auth, "subject-alice", ""), nil, bc, nil, 303)
		return w.Result().Cookies()
	}
	ssocookies := loginSSO()
	for _, c := range ssocookies {
		if c.Name == "neon_v2_session" && (!c.HttpOnly || c.SameSite != http.SameSiteStrictMode) {
			t.Fatal("SSO weakened session cookie")
		}
	}
	call("GET", "/api/v1/session", nil, ssocookies, nil, 200)
	policy("required", "1", "sso-policy-required", ssocookies, 200)
	call("GET", "/api/v1/organizations/org_sso", nil, local("alice"), nil, 403)
	call("GET", "/api/v1/organizations/org_sso", nil, ssocookies, nil, 200)
	call("GET", "/api/v1/organizations/org_other", nil, local("alice"), nil, 200)
	policy("optional", "2", "sso-policy-local-bypass", local("alice"), 403)
	policy("optional", "2", "sso-policy-optional", ssocookies, 200)
	// Original authenticated session revocation invalidates an in-flight link.
	authorization, browser = flow(true, "bob")
	exec(`UPDATE sessions SET revoked_at=now() WHERE token_hash=$1`, digest("bob-local"))
	call("GET", idp.code(t, authorization, "subject-bob", ""), nil, browser, nil, 401)
	// Enabled local MFA requires a new local proof after the OIDC assertion.
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "CI", AccountName: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	secret := key.Secret()
	sealed, err := s.sealSavedSQL("v1", secret, consoleMFAAAD("alice"))
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO console_mfa(user_id,key_version,sealed_secret,enabled_at,last_step,recovery_hashes) VALUES('alice','v1',$1,now(),-1,'[]')`, sealed)
	exec(`UPDATE sessions SET mfa_verified_at=now() WHERE user_id='alice'`)
	authorization, browser = flow(false, "")
	mfaRedirect := call("GET", idp.code(t, authorization, "subject-alice", ""), nil, browser, nil, 303)
	if !strings.HasSuffix(mfaRedirect.Header().Get("Location"), "/#/sso-mfa") {
		t.Fatal("MFA was bypassed")
	}
	challenge := mfaRedirect.Result().Cookies()
	call("POST", "/auth/sso/mfa", record{"code": "000000"}, challenge, nil, 401)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w := call("POST", "/auth/sso/mfa", record{"code": code}, challenge, nil, 200)
	mfaCookies := w.Result().Cookies()
	call("GET", "/api/v1/session", nil, mfaCookies, nil, 200)
	call("POST", "/auth/sso/mfa", record{"code": code}, challenge, nil, 401)
	var binding string
	if err = db.QueryRow(ctx, `SELECT id FROM console_sso_bindings WHERE user_id='alice'`).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	call("DELETE", base+"/bindings/"+binding, record{"password": pwd}, local("alice"), nil, 200)
	call("GET", "/api/v1/session", nil, mfaCookies, nil, 401)
	call("GET", "/api/v1/session", nil, ssocookies, nil, 401)
	var audits int
	err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('console_sso_link','console_sso_login','console_sso_policy','console_sso_unlink')`).Scan(&audits)
	if err != nil || audits < 7 {
		t.Fatal("Transactional SSO audit missing", audits, err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 2 {
		t.Fatal("OIDC provisioned an account", n, err)
	}
	if _, err = fmt.Fprintf(os.Stdout, "Console OIDC code/PKCE, six invalid token classes, explicit binding, policy CAS/replay, required SSO, local MFA and unlink passed\n"); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleOIDCConfigRejectsUnapprovedOriginsAndSecrets(t *testing.T) {
	base := consoleOIDCSettings{Version: 1, PublicOrigin: "http://console.example.test", LabHTTP: true, Providers: []*consoleOIDCProvider{{ID: "ci", Name: "CI", Organization: "org_ci", Issuer: "http://127.0.0.1:1234", AuthorizationURL: "http://127.0.0.1:1234/auth", TokenURL: "http://127.0.0.1:1234/token", JWKSURL: "http://127.0.0.1:1234/jwks", ClientID: "ci", ClientSecret: "fixture-secret-long-enough", AuthMethod: "client_secret_basic", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true}}}
	for _, name := range []string{"baseline", "external_token", "query_jwks", "missing_lab", "public_http", "mixed_pins", "short_secret", "duplicate_provider", "unknown_auth", "cookie_mismatch"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var c consoleOIDCSettings
			_ = json.Unmarshal(raw, &c)
			p := c.Providers[0]
			switch name {
			case "external_token":
				p.TokenURL = "https://other.example.test/token"
			case "query_jwks":
				p.JWKSURL += "?redirect=1"
			case "missing_lab":
				c.LabHTTP = false
			case "public_http":
				p.AllowedIPs = []string{"8.8.8.8"}
			case "mixed_pins":
				p.AllowedIPs = append(p.AllowedIPs, "8.8.8.8")
			case "short_secret":
				p.ClientSecret = "short"
			case "duplicate_provider":
				c.Providers = append(c.Providers, p)
			case "unknown_auth":
				p.AuthMethod = "none"
			}
			raw, _ = json.Marshal(c)
			settings, err := newConsoleOIDCSettings(raw, name == "cookie_mismatch")
			if name == "baseline" {
				if err != nil || settings == nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("Invalid OIDC configuration accepted")
			}
		})
	}
}
