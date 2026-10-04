package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestComputeManagementJWTIsolation(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1790985000, 0)
	token := computeJWT(key, "ep_native", now)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("invalid signed JWT")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
		t.Fatal("JWT signature invalid")
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil || claims["compute_id"] != "cp-native" || claims["scope"] != nil || int64(claims["exp"].(float64)) != now.Add(time.Minute).Unix() {
		t.Fatal("JWT must have exact endpoint scope, no global admin scope and bounded expiry")
	}
	config := map[string]any{}
	installComputeJWK(config, key)
	jwks := nested(config, "compute_ctl_config", "jwks", "keys").([]any)
	jwk := jwks[0].(map[string]any)
	decoded, err := base64.RawURLEncoding.DecodeString(jwk["x"].(string))
	if err != nil || len(decoded) != ed25519.PublicKeySize || !ed25519.PublicKey(decoded).Equal(public) {
		t.Fatal("public JWK does not match signer")
	}
	encoded, _ := json.Marshal(config)
	if strings.Contains(string(encoded), base64.StdEncoding.EncodeToString(key.Seed())) {
		t.Fatal("compute config leaked private signing material")
	}
}

func TestCatalogIdentifiersAreQuoted(t *testing.T) {
	for _, name := range []string{"app", "租户数据库", `name"; DROP ROLE postgres; --`} {
		if !validPGIdentifier(name) {
			t.Fatalf("valid quoted identifier rejected: %q", name)
		}
		if !strings.HasPrefix(pgQuote(name), `"`) || !strings.HasSuffix(pgQuote(name), `"`) {
			t.Fatal("identifier was not quoted")
		}
	}
	for _, name := range []string{"", "leading ", strings.Repeat("a", 64), "nul\x00", string([]byte{0xff})} {
		if validPGIdentifier(name) {
			t.Fatal("invalid PostgreSQL identifier accepted")
		}
	}
	for _, name := range []string{"PG_admin", "cloud_admin", "control_probe", "neon_superuser"} {
		if !protectedPGRole(name) {
			t.Fatal("protected role exposed")
		}
	}
}
