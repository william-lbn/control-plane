package dataapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testBranch = "br_0123456789abcdef"
const otherBranch = "br_fedcba9876543210"

type fixture struct {
	g        *Gateway
	provider ed25519.PrivateKey
	config   Config
}

func makeFixture(t *testing.T, upstream string) fixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": "provider-test",
		"x": base64.RawURLEncoding.EncodeToString(public)}}})
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	c := Config{Version: 1, DelegationIssuer: "urn:neon:selfhost:data-api", AllowPlaintextUpstream: true,
		Routes: []RouteConfig{{BranchID: testBranch, Upstream: upstream, Issuer: "https://identity.example.test/",
			Audience: "urn:neon:branch:" + testBranch, DefaultRole: "authenticated", AllowedRoles: []string{"authenticated", "anonymous"},
			AllowedOrigins: []string{"https://app.example.test"}, JWKS: jwks}}}
	g, err := New(c, seed, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return time.Unix(1791072000, 0) }
	return fixture{g, private, c}
}

func (f fixture) claims() jwt.MapClaims {
	return jwt.MapClaims{"iss": f.config.Routes[0].Issuer, "aud": f.config.Routes[0].Audience,
		"sub": "user-alice", "exp": f.g.now().Add(2 * time.Minute).Unix(), "iat": f.g.now().Unix()}
}
func (f fixture) token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = "provider-test"
	signed, err := token.SignedString(f.provider)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
func call(t *testing.T, g *Gateway, method, path, token string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, body)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, request)
	return w
}

func TestProviderTokenAndTrustedDelegation(t *testing.T) {
	var gotClaims jwt.MapClaims
	var gotPath, gotQuery, gotCookie, gotForwarded, gotSpoof string
	var key ed25519.PublicKey
	var clock func() time.Time
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(t *jwt.Token) (any, error) { return key, nil },
			jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("urn:neon:selfhost:data-api"), jwt.WithAudience("urn:neon:branch:"+testBranch), jwt.WithTimeFunc(clock))
		if err != nil {
			t.Errorf("trusted token rejected: %v", err)
			w.WriteHeader(401)
			return
		}
		gotClaims = token.Claims.(jwt.MapClaims)
		gotPath, gotQuery, gotCookie, gotForwarded, gotSpoof = r.URL.Path, r.URL.RawQuery, r.Header.Get("Cookie"), r.Header.Get("X-Forwarded-User"), r.Header.Get("X-Neon-Role")
		w.Header().Set("Set-Cookie", "upstream-private=hidden")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Range", "0-0/1")
		_, _ = w.Write([]byte(`[{"owner":"user-alice"}]`))
	}))
	defer upstream.Close()
	f := makeFixture(t, upstream.URL)
	key = f.g.signer.Public().(ed25519.PublicKey)
	clock = f.g.now
	claims := f.claims()
	claims["custom_tenant"] = "tenant-a"
	w := call(t, f.g, "GET", "/data/v1/"+testBranch+"/notes?select=owner", f.token(t, claims), nil, map[string]string{
		"Origin": "https://app.example.test", "Cookie": "neon_v2_session=console-session", "X-Neon-Role": "postgres", "X-Forwarded-User": "root",
		"Connection": "Authorization, X-Untrusted", "X-Untrusted": "hidden"})
	if w.Code != 200 || gotClaims == nil {
		t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
	}
	if gotClaims["sub"] != "user-alice" || gotClaims["role"] != "authenticated" || gotClaims["branch_id"] != testBranch || gotClaims["custom_tenant"] != "tenant-a" {
		t.Fatal("verified identity not preserved")
	}
	if gotClaims["provider_issuer"] != f.config.Routes[0].Issuer || gotClaims["exp"].(float64) != float64(f.g.now().Add(30*time.Second).Unix()) {
		t.Fatal("delegation not bounded")
	}
	if gotPath != "/notes" || gotQuery != "select=owner" || gotCookie != "" || gotForwarded != "" || gotSpoof != "" {
		t.Fatal("unsafe forwarding")
	}
	if w.Header().Get("Set-Cookie") != "" || w.Header().Get("Access-Control-Allow-Origin") != "https://app.example.test" || w.Header().Get("Content-Range") != "0-0/1" {
		t.Fatal("unsafe response headers")
	}
	if bytes.Contains(f.g.DelegationJWKS(), f.provider.Seed()) || bytes.Contains(f.g.DelegationJWKS(), []byte(`"d"`)) {
		t.Fatal("private key exported")
	}
}

func TestJWTRejectionMatrix(t *testing.T) {
	f := makeFixture(t, "http://127.0.0.1:1")
	cases := map[string]func(jwt.MapClaims){
		"issuer":             func(c jwt.MapClaims) { c["iss"] = "https://other.example.test/" },
		"audience":           func(c jwt.MapClaims) { c["aud"] = "urn:neon:branch:" + otherBranch },
		"missing audience":   func(c jwt.MapClaims) { delete(c, "aud") },
		"ambiguous audience": func(c jwt.MapClaims) { c["aud"] = []string{f.config.Routes[0].Audience, "other"} },
		"expired":            func(c jwt.MapClaims) { c["exp"] = f.g.now().Add(-time.Second).Unix() },
		"missing expiry":     func(c jwt.MapClaims) { delete(c, "exp") },
		"future issued":      func(c jwt.MapClaims) { c["iat"] = f.g.now().Add(time.Second).Unix() },
		"not before":         func(c jwt.MapClaims) { c["nbf"] = f.g.now().Add(time.Second).Unix() },
		"missing subject":    func(c jwt.MapClaims) { delete(c, "sub") },
		"wrong subject type": func(c jwt.MapClaims) { c["sub"] = 42 },
		"role escalation":    func(c jwt.MapClaims) { c["role"] = "postgres" },
		"unknown role":       func(c jwt.MapClaims) { c["role"] = "writer" },
		"wrong role type":    func(c jwt.MapClaims) { c["role"] = []string{"authenticated"} },
		"branch mismatch":    func(c jwt.MapClaims) { c["branch_id"] = otherBranch },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := f.claims()
			change(c)
			w := call(t, f.g, "GET", "/data/v1/"+testBranch+"/notes", f.token(t, c), nil, nil)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" || strings.Contains(w.Body.String(), "user-alice") {
				t.Fatalf("rejection failed: %d", w.Code)
			}
		})
	}
	for _, algorithm := range []*jwt.SigningMethodHMAC{jwt.SigningMethodHS256, jwt.SigningMethodHS512} {
		t.Run(algorithm.Alg(), func(t *testing.T) {
			token := jwt.NewWithClaims(algorithm, f.claims())
			token.Header["kid"] = "provider-test"
			value, err := token.SignedString([]byte(f.provider.Public().(ed25519.PublicKey)))
			if err != nil {
				t.Fatal(err)
			}
			if w := call(t, f.g, "GET", "/data/v1/"+testBranch+"/notes", value, nil, nil); w.Code != 401 {
				t.Fatal("algorithm confusion accepted")
			}
		})
	}
	t.Run("untrusted signing key", func(t *testing.T) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, f.claims())
		token.Header["kid"] = "provider-test"
		value, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		if w := call(t, f.g, "GET", "/data/v1/"+testBranch+"/notes", value, nil, nil); w.Code != 401 {
			t.Fatal("untrusted signature accepted")
		}
	})
	t.Run("critical extension", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, f.claims())
		token.Header["kid"] = "provider-test"
		token.Header["crit"] = []string{"unsafe"}
		value, err := token.SignedString(f.provider)
		if err != nil {
			t.Fatal(err)
		}
		if w := call(t, f.g, "GET", "/data/v1/"+testBranch+"/notes", value, nil, nil); w.Code != 401 {
			t.Fatal("critical header accepted")
		}
	})
	t.Run("multiple authorization", func(t *testing.T) {
		request := httptest.NewRequest("GET", "/data/v1/"+testBranch+"/notes", nil)
		request.Header.Add("Authorization", "Bearer "+f.token(t, f.claims()))
		request.Header.Add("Authorization", "Bearer second")
		w := httptest.NewRecorder()
		f.g.ServeHTTP(w, request)
		if w.Code != 401 {
			t.Fatal("multiple bearer accepted")
		}
	})
}

func TestRouteBodyAndCORSLimits(t *testing.T) {
	f := makeFixture(t, "http://127.0.0.1:1")
	for _, path := range []string{"/data/v1/" + otherBranch + "/notes", "/api/v1/projects", "/data/v1/not-a-branch/notes"} {
		if w := call(t, f.g, "GET", path, "", nil, nil); w.Code != 404 {
			t.Fatal("unknown route accepted")
		}
	}
	for _, path := range []string{"/data/v1/" + testBranch + "/../notes", "/data/v1/" + testBranch + "/%2fnotes"} {
		if w := call(t, f.g, "GET", path, f.token(t, f.claims()), nil, nil); w.Code != 400 {
			t.Fatal("ambiguous path accepted")
		}
	}
	if w := call(t, f.g, "POST", "/data/v1/"+testBranch+"/notes", f.token(t, f.claims()), strings.NewReader(strings.Repeat("x", maxBodyBytes+1)), nil); w.Code != 413 {
		t.Fatal("body budget not enforced")
	}
	if w := call(t, f.g, "GET", "/data/v1/"+testBranch+"/notes", f.token(t, f.claims()), nil, map[string]string{"Origin": "https://evil.example.test"}); w.Code != 403 {
		t.Fatal("untrusted origin accepted")
	}
	w := call(t, f.g, "OPTIONS", "/data/v1/"+testBranch+"/notes", "", nil, map[string]string{"Origin": "https://app.example.test", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization, content-profile"})
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("CORS preflight failed")
	}
	w = call(t, f.g, "OPTIONS", "/data/v1/"+testBranch+"/notes", "", nil, map[string]string{"Origin": "https://app.example.test", "Access-Control-Request-Method": "TRACE"})
	if w.Code != 403 {
		t.Fatal("unsafe CORS method accepted")
	}
}

func TestConfigurationFailClosed(t *testing.T) {
	f := makeFixture(t, "http://127.0.0.1:1")
	for name, change := range map[string]func(*Config){
		"plaintext":             func(c *Config) { c.AllowPlaintextUpstream = false },
		"URL credentials":       func(c *Config) { c.Routes[0].Upstream = "http://user:private@127.0.0.1:1" },
		"URL query":             func(c *Config) { c.Routes[0].Upstream = "http://127.0.0.1:1/?target=evil" },
		"protected role":        func(c *Config) { c.Routes[0].AllowedRoles = append(c.Routes[0].AllowedRoles, "neon_superuser") },
		"missing default grant": func(c *Config) { c.Routes[0].DefaultRole = "writer" },
		"wildcard origin":       func(c *Config) { c.Routes[0].AllowedOrigins = []string{"*"} },
		"duplicate audience":    func(c *Config) { r := c.Routes[0]; r.BranchID = otherBranch; c.Routes = append(c.Routes, r) },
		"private JWK": func(c *Config) {
			c.Routes[0].JWKS = json.RawMessage(`{"keys":[{"kty":"OKP","kid":"a","use":"sig","alg":"EdDSA","crv":"Ed25519","x":"bad","d":"private"}]}`)
		},
		"unsupported version": func(c *Config) { c.Version = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(f.config)
			var config Config
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			change(&config)
			if _, err := New(config, f.g.signer.Seed(), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	data, _ := json.Marshal(f.config)
	if _, err := ParseConfig(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfig(bytes.NewReader(append(data, []byte(` {}`)...))); err == nil {
		t.Fatal("trailing config accepted")
	}
	if _, err := ParseConfig(strings.NewReader(`{"version":1,"password":"private"}`)); err == nil {
		t.Fatal("unknown config fields accepted")
	}
}
