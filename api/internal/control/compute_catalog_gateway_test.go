package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func catalogGatewayFixture(t *testing.T) (map[string]any, map[string]any, ed25519.PrivateKey, catalogPayload) {
	t.Helper()
	p := catalogPayload{ProjectID: "prj_0123456789abcdef", EndpointID: "ep_0123456789abcdef", Name: "gateway_new_customer"}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := computeConfig(p.ProjectID, strings.Repeat("a", 32), strings.Repeat("b", 32), "admin-intent", "probe-intent", "read_write")
	if err != nil {
		t.Fatal(err)
	}
	var source map[string]any
	_ = json.Unmarshal([]byte(raw), &source)
	installComputeJWK(source, key)
	source["spec"].(map[string]any)["skip_pg_catalog_updates"] = true
	// The real gateway reads JSON from Kubernetes; normalize typed JWK slices
	// into the same wire representation before comparing complete identities.
	normalized, _ := json.Marshal(source)
	_ = json.Unmarshal(normalized, &source)
	verifier, err := scramVerifier("protected-gateway-test-password")
	if err != nil {
		t.Fatal(err)
	}
	apply, _, err := computeCatalogBootstrapNativeRoleConfigs(source, p, verifier)
	if err != nil {
		t.Fatal(err)
	}
	return source, apply, key, p
}
func TestComputeCatalogBootstrapGatewayTargetAdmission(t *testing.T) {
	source, apply, _, _ := catalogGatewayFixture(t)
	before, _ := json.Marshal(source)
	body, _ := json.Marshal(apply)
	if sameComputeIdentity(source, apply) {
		t.Fatal("fixture did not exercise the frozen skip flag")
	}
	if !validComputeCatalogRequest(source, apply) {
		t.Fatal("exact native role projection denied")
	}
	for _, scenario := range []string{"two_roles", "no_admin", "other_admin", "databases", "delete_delta", "tenant", "topology_override", "unknown_startup", "duplicate_setting", "jwks", "protected_role", "empty_verifier", "bad_verifier", "extra_role_option", "uninitialized", "replica", "null_mode"} {
		t.Run(scenario, func(t *testing.T) {
			var current, request map[string]any
			_ = json.Unmarshal(before, &current)
			_ = json.Unmarshal(body, &request)
			spec := request["spec"].(map[string]any)
			cluster := spec["cluster"].(map[string]any)
			role := cluster["roles"].([]any)[0].(map[string]any)
			switch scenario {
			case "two_roles":
				cluster["roles"] = append(cluster["roles"].([]any), map[string]any{"name": "unrelated"})
			case "no_admin":
				role["options"] = nil
			case "other_admin":
				role["options"].([]any)[0].(map[string]any)["value"] = "cloud_admin"
			case "databases":
				cluster["databases"] = []any{map[string]any{"name": "unrelated"}}
			case "delete_delta":
				spec["delta_operations"] = []any{map[string]any{"action": "delete_db", "name": "unrelated"}}
			case "tenant":
				for _, v := range cluster["settings"].([]any) {
					item := v.(map[string]any)
					if item["name"] == "neon.tenant_id" {
						item["value"] = "foreign"
					}
				}
			case "topology_override":
				spec["tenant_id"] = strings.Repeat("c", 32)
			case "unknown_startup":
				spec["future_startup_override"] = true
			case "duplicate_setting":
				cluster["settings"] = append(cluster["settings"].([]any), cluster["settings"].([]any)[0])
			case "jwks":
				request["compute_ctl_config"] = map[string]any{"foreign": true}
			case "protected_role":
				role["name"] = "cloud_admin"
			case "empty_verifier":
				role["encrypted_password"] = ""
			case "bad_verifier":
				role["encrypted_password"] = "SCRAM-SHA-256$invalid"
			case "extra_role_option":
				role["options"] = append(role["options"].([]any), map[string]any{"name": "SUPERUSER"})
			case "uninitialized":
				delete(current["spec"].(map[string]any), "skip_pg_catalog_updates")
			case "replica":
				current["spec"].(map[string]any)["mode"] = "Replica"
				spec["mode"] = "Replica"
			case "null_mode":
				current["spec"].(map[string]any)["mode"] = nil
				spec["mode"] = nil
			}
			if validComputeCatalogRequest(current, request) {
				t.Fatal("unsafe native configure admitted")
			}
		})
	}
	after, _ := json.Marshal(source)
	now, _ := json.Marshal(apply)
	if string(before) != string(after) || string(body) != string(now) {
		t.Fatal("admission mutated source or requested config")
	}
}

type catalogGatewayTransport func(*http.Request) (*http.Response, error)

func (f catalogGatewayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestComputeCatalogBootstrapGatewayForwardsOnlyAuthorizedTarget(t *testing.T) {
	source, apply, key, p := catalogGatewayFixture(t)
	raw, _ := json.Marshal(source)
	labels := credentialLabels(p.ProjectID, p.EndpointID)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/secrets/") {
			jsonResponse(w, 200, record{"metadata": record{"labels": labels}, "data": record{"config.json": base64.StdEncoding.EncodeToString(raw)}})
			return
		}
		if strings.Contains(r.URL.Path, "/services/") {
			jsonResponse(w, 200, record{"metadata": record{"labels": labels}, "spec": record{"clusterIP": "127.0.0.1"}})
			return
		}
		w.WriteHeader(404)
	}))
	defer peer.Close()
	forwards := 0
	gateway := &computeGateway{kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}, http: &http.Client{Transport: catalogGatewayTransport(func(r *http.Request) (*http.Response, error) {
		forwards++
		var got map[string]any
		if json.NewDecoder(r.Body).Decode(&got) != nil || !validComputeCatalogRequest(source, got) || r.URL.String() != "http://127.0.0.1:3080/configure" || r.Header.Get("Authorization") == "" {
			t.Error("native forwarding changed authority or body")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"running"}`))}, nil
	})}}
	tlsName := kubeName(p.EndpointID) + "-management.neon.svc"
	send := func(value map[string]any, token string) int {
		raw, _ := json.Marshal(value)
		r := httptest.NewRequest(http.MethodPost, "/computes/"+kubeName(p.EndpointID)+"/configure", strings.NewReader(string(raw)))
		r.SetPathValue("compute", kubeName(p.EndpointID))
		r.TLS = &tls.ConnectionState{ServerName: tlsName}
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		gateway.configure(w, r.WithContext(context.Background()))
		return w.Code
	}
	if send(apply, computeJWT(key, p.EndpointID, time.Now())) != 200 || forwards != 1 {
		t.Fatal("real handler denied exact transient role request")
	}
	if send(apply, computeJWT(key, "ep_fedcba9876543210", time.Now())) != 401 || forwards != 1 {
		t.Fatal("handler forwarded wrong-endpoint JWT")
	}
	tlsName = "cp-fedcba9876543210-management.neon.svc"
	if send(apply, computeJWT(key, p.EndpointID, time.Now())) != 403 || forwards != 1 {
		t.Fatal("handler forwarded wrong endpoint SNI")
	}
	tlsName = kubeName(p.EndpointID) + "-management.neon.svc"
	apply["spec"].(map[string]any)["tenant_id"] = strings.Repeat("c", 32)
	if send(apply, computeJWT(key, p.EndpointID, time.Now())) != 403 || forwards != 1 {
		t.Fatal("handler forwarded topology override")
	}
}
