package dataapi

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestRS256PublicKeyVerification(t *testing.T) {
	f := makeFixture(t, "http://127.0.0.1:1")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Routes[0].JWKS, _ = json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "rsa-provider", "key_ops": []string{"verify"},
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	g, err := New(f.config, f.g.signer.Seed(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	g.now = f.g.now
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, f.claims())
	token.Header["kid"] = "rsa-provider"
	value, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.authenticate(g.routes[testBranch], []string{"Bearer " + value}); err != nil {
		t.Fatal("valid RSA token rejected")
	}
	token.Header["kid"] = "unconfigured"
	value, err = token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.authenticate(g.routes[testBranch], []string{"Bearer " + value}); err == nil {
		t.Fatal("unknown RSA key accepted")
	}
	var keys struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(f.config.Routes[0].JWKS, &keys); err != nil {
		t.Fatal(err)
	}
	keys.Keys = append(keys.Keys, keys.Keys[0])
	duplicate, _ := json.Marshal(keys)
	if _, err := parseJWKS(duplicate); err == nil {
		t.Fatal("duplicate kid accepted")
	}
	keys.Keys = keys.Keys[:1]
	keys.Keys[0]["e"] = base64.RawURLEncoding.EncodeToString([]byte{2})
	badExponent, _ := json.Marshal(keys)
	if _, err := parseJWKS(badExponent); err == nil {
		t.Fatal("unsafe RSA exponent accepted")
	}
	keys.Keys[0]["e"] = base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
	keys.Keys[0]["n"] = base64.RawURLEncoding.EncodeToString([]byte{127})
	weak, _ := json.Marshal(keys)
	if _, err := parseJWKS(weak); err == nil {
		t.Fatal("weak RSA modulus accepted")
	}
}

func TestHTTPSUpstreamRequiresTrustedCertificate(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()
	f := makeFixture(t, upstream.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	untrusted, err := New(f.config, f.g.signer.Seed(), logger)
	if err != nil {
		t.Fatal(err)
	}
	untrusted.now = f.g.now
	if w := call(t, untrusted, "GET", "/data/v1/"+testBranch+"/notes", f.token(t, f.claims()), nil, nil); w.Code != 503 {
		t.Fatal("untrusted upstream accepted")
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	trusted, err := NewWithTrust(f.config, f.g.signer.Seed(), logger, ca)
	if err != nil {
		t.Fatal(err)
	}
	trusted.now = f.g.now
	if w := call(t, trusted, "GET", "/data/v1/"+testBranch+"/notes", f.token(t, f.claims()), nil, nil); w.Code != 200 {
		t.Fatal("trusted upstream rejected")
	}
	if _, err := NewWithTrust(f.config, f.g.signer.Seed(), logger, []byte("not a CA")); err == nil {
		t.Fatal("invalid root CA accepted")
	}
}
