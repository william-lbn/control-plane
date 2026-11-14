package control

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDataAPIConsoleResponseBound(t *testing.T) {
	w := &dataAPIConsoleWriter{header: make(http.Header), code: 200}
	w.WriteHeader(403)
	payload := bytes.Repeat([]byte("x"), 200000)
	for i := 0; i < 3; i++ {
		n, err := w.Write(payload)
		if err != nil || n != len(payload) {
			t.Fatal("proxy response write failed")
		}
	}
	if w.code != 403 || !w.truncated || w.body.Len() != 65536 {
		t.Fatal("upstream status or bounded response contract lost")
	}
}

func dataAPITestPayload(t *testing.T) dataAPIPayload {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": "provider-1", "x": base64.RawURLEncoding.EncodeToString(key)}}})
	return dataAPIPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", Generation: 1, SecretRef: "neon-da-0123456789abcdef-g1",
		Spec: DataAPISpec{Database: "postgres", Schema: "app_data", Issuer: "https://identity.example.test", Audience: "branch-1", JWKS: jwks, AllowedOrigins: []string{}}}
}
func TestDataAPINativeValidation(t *testing.T) {
	p := dataAPITestPayload(t)
	if err := validateDataAPI(p); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"public", "pg_catalog", "information_schema", "neon_auth", "control_guard", "a,b", "a\n", "a;drop table x"} {
		t.Run(schema, func(t *testing.T) {
			copy := p
			copy.Spec.Schema = schema
			if validateDataAPI(copy) == nil {
				t.Fatal("unsafe schema accepted")
			}
		})
	}
	p.Spec.JWKS = json.RawMessage(`{"keys":[{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","kid":"bad","x":"x","d":"private"}]}`)
	if validateDataAPI(p) == nil {
		t.Fatal("private or invalid provider key accepted")
	}
}
func TestDataAPINativeCapabilityFailClosed(t *testing.T) {
	t.Setenv("NEON_DATA_API_ENABLED", "true")
	t.Setenv("NEON_DATA_API_LAB_HTTP", "true")
	t.Setenv("NEON_DATA_API_GATEWAY_IMAGE", "example.test/gateway@sha256:"+strings.Repeat("a", 64))
	t.Setenv("NEON_DATA_API_POSTGREST_IMAGE", "example.test/postgrest@sha256:"+strings.Repeat("b", 64))
	if !dataAPIEnabled() {
		t.Fatal("explicit pinned lab profile rejected")
	}
	t.Setenv("NEON_DATA_API_POSTGREST_IMAGE", "example.test/postgrest:latest")
	if dataAPIEnabled() {
		t.Fatal("floating image accepted")
	}
	t.Setenv("NEON_DATA_API_POSTGREST_IMAGE", "example.test/postgrest@sha256:"+strings.Repeat("b", 64))
	t.Setenv("NEON_DATA_API_LAB_HTTP", "false")
	if dataAPIEnabled() {
		t.Fatal("unaccepted production transport advertised")
	}
}
func TestDataAPINativeWorkloadIsolation(t *testing.T) {
	p := dataAPITestPayload(t)
	d, err := dataAPIDeployment(p, "neon", true)
	if err != nil {
		t.Fatal(err)
	}
	spec := nested(d, "spec", "template", "spec").(map[string]any)
	if spec["automountServiceAccountToken"] != false {
		t.Fatal("runtime has a Kubernetes token")
	}
	containers := spec["containers"].([]any)
	auth := containers[0].(map[string]any)
	sql := containers[1].(map[string]any)
	probe, _ := json.Marshal(nested(sql, "startupProbe", "exec", "command"))
	if string(probe) != `["/opt/neon-control/postgrest","--ready","/run/data-api/postgrest.conf"]` ||
		sql["readinessProbe"] != nil || sql["livenessProbe"] != nil {
		t.Fatal("PostgREST must prove startup without recurring database probes")
	}
	if sql["ports"] != nil {
		t.Fatal("The PostgREST management listener is private to the Pod")
	}
	a, _ := json.Marshal(auth)
	b, _ := json.Marshal(sql)
	if strings.Contains(string(a), "sql-config") || strings.Contains(string(b), "auth-config") {
		t.Fatal("runtime credential domains overlap")
	}
	volumes := spec["volumes"].([]any)
	keys, _ := json.Marshal(volumes[0])
	if strings.Contains(string(keys), "postgrest.conf") || strings.Contains(string(keys), "password") {
		t.Fatal("auth entry has SQL credentials")
	}
	keys, _ = json.Marshal(volumes[1])
	if strings.Contains(string(keys), "delegation-seed") || strings.Contains(string(keys), "config.json") {
		t.Fatal("PostgREST has provider/signing credentials")
	}
	disabled, err := dataAPIDeployment(p, "neon", false)
	if err != nil {
		t.Fatal(err)
	}
	if number(nested(disabled, "spec", "replicas")) != 0 {
		t.Fatal("disable did not scale to zero")
	}
	if !protectedPGRole(dataAPIRole(p.BranchID)) || !protectedPGRole(dataAPILogin(p.BranchID)) {
		t.Fatal("service identity can be mutated by ordinary catalog API")
	}
}

func TestDataAPIRuntimePlacement(t *testing.T) {
	p := dataAPITestPayload(t)
	const env = "NEON_DATA_API_RUNTIME_HOSTNAMES"
	t.Setenv(env, "worker-117,worker-118,worker-119")
	for _, enable := range []bool{true, false} {
		d, err := dataAPIDeployment(p, "neon", enable)
		if err != nil {
			t.Fatal(err)
		}
		terms, _ := json.Marshal(nested(d, "spec", "template", "spec", "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms"))
		const expected = `[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["worker-117","worker-118","worker-119"]}]}]`
		if string(terms) != expected {
			t.Fatalf("unexpected node selector terms: %s", terms)
		}
	}
	t.Setenv(env, "worker-117,worker-117")
	if _, err := dataAPIDeployment(p, "neon", true); err == nil {
		t.Fatal("duplicate runtime hostname was accepted")
	}
}
