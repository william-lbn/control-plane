package control

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagedAuthBrowserOriginBoundary(t *testing.T) {
	public := "https://auth.example.test"
	spec := ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{"https://app.example.test"}}
	for _, tc := range []struct {
		name, method, origin, requestedMethod, requestedHeaders string
		status                                                  int
		stop                                                    bool
	}{
		{"server request", "GET", "", "", "", 200, false},
		{"same origin", "POST", public, "", "", 200, false},
		{"trusted app", "POST", "https://app.example.test", "", "", 200, false},
		{"unrelated app", "GET", "https://evil.example.test", "", "", 403, true},
		{"opaque origin", "POST", "null", "", "", 403, true},
		{"configured preflight", "OPTIONS", "https://app.example.test", "POST", "content-type", 204, true},
		{"unknown header", "OPTIONS", public, "POST", "Authorization", 403, true},
		{"unsafe method", "OPTIONS", public, "DELETE", "", 403, true},
		{"missing origin", "OPTIONS", "", "POST", "", 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/auth/v1/br_0123456789abcdef/token", nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Access-Control-Request-Method", tc.requestedMethod)
			r.Header.Set("Access-Control-Request-Headers", tc.requestedHeaders)
			w := httptest.NewRecorder()
			if managedAuthCORS(w, r, spec, public) != tc.stop || w.Code != tc.status {
				t.Fatal("browser origin boundary did not fail closed", w.Code)
			}
			if tc.status < 300 && tc.origin != "" && (w.Header().Get("Access-Control-Allow-Origin") != tc.origin || w.Header().Get("Access-Control-Allow-Credentials") != "true") {
				t.Fatal("credentialed browser response needs an exact origin")
			}
		})
	}
}

func TestManagedAuthConfigurationFailClosed(t *testing.T) {
	t.Setenv("NEON_AUTH_LAB_HTTP", "true")
	p := managedAuthPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", Generation: 1, SecretRef: "neon-auth-0123456789abcdef-g1", Spec: ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}}}
	if validateManagedAuth(p) != nil {
		t.Fatal("valid branch configuration rejected")
	}
	for _, origin := range []string{"https://*.example.test", "https://name:password@example.test", "https://example.test/", "https://example.test?x=a", "https://example.test#fragment", "file:///etc/passwd", "https://example.test\n"} {
		t.Run(origin, func(t *testing.T) {
			bad := p
			bad.Spec.AllowedOrigins = []string{origin}
			if validateManagedAuth(bad) == nil {
				t.Fatal("unsafe trusted origin accepted")
			}
		})
	}
	t.Setenv("NEON_AUTH_ENABLED", "true")
	t.Setenv("NEON_AUTH_RUNTIME_IMAGE", "example.test/auth@sha256:"+strings.Repeat("a", 64))
	t.Setenv("NEON_AUTH_PUBLIC_ORIGIN", "http://192.0.2.1:30788")
	t.Setenv("NEON_AUTH_PG_CA_SECRET", "proxy-public-ca")
	t.Setenv("NEON_AUTH_PG_CA_KEY", "ca.crt")
	t.Setenv("NEON_AUTH_PG_SERVER_NAME", "proxy.example.test")
	if !managedAuthEnabled() {
		t.Fatal("explicit digest-pinned lab profile rejected")
	}
	t.Setenv("NEON_AUTH_PG_SERVER_NAME", "*.example.test")
	if managedAuthEnabled() {
		t.Fatal("wildcard SQL certificate identity accepted")
	}
	t.Setenv("NEON_AUTH_PG_SERVER_NAME", "proxy.example.test")
	t.Setenv("NEON_AUTH_RUNTIME_IMAGE", "example.test/auth:latest")
	if managedAuthEnabled() {
		t.Fatal("floating runtime accepted")
	}
	t.Setenv("NEON_AUTH_RUNTIME_IMAGE", "example.test/auth@sha256:"+strings.Repeat("a", 64))
	t.Setenv("NEON_AUTH_LAB_HTTP", "false")
	if managedAuthEnabled() {
		t.Fatal("unaccepted production transport advertised")
	}
}
func TestManagedAuthWorkloadIsolation(t *testing.T) {
	p := managedAuthPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", Generation: 2, SecretRef: "neon-auth-0123456789abcdef-g2"}
	body := managedAuthDeployment(p, "neon", true)
	spec := nested(body, "spec", "template", "spec").(record)
	if spec["automountServiceAccountToken"] != false {
		t.Fatal("Auth runtime has a Kubernetes token")
	}
	container := spec["containers"].([]any)[0].(record)
	if number(nested(container, "securityContext", "runAsUser")) != 0 {
		t.Fatal("container identity must be set by the Pod")
	}
	if nested(container, "securityContext", "readOnlyRootFilesystem") != true || nested(spec, "securityContext", "runAsNonRoot") != true {
		t.Fatal("Auth runtime must be unprivileged and read-only")
	}
	encoded, _ := json.Marshal(body)
	for _, private := range []string{"probePassword", "adminVerifier", "/var/run/secrets/kubernetes.io", "DATABASE_URL", "privileged", "hostNetwork"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("unsafe runtime capability: %s", private)
		}
	}
	if !strings.Contains(string(encoded), p.SecretRef) {
		t.Fatal("generation-scoped credential reference missing")
	}
	if number(nested(managedAuthDeployment(p, "neon", false), "spec", "replicas")) != 0 {
		t.Fatal("disable leaves Auth running")
	}
}
