package control

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	if !managedAuthEnabled() {
		t.Fatal("explicit digest-pinned lab profile rejected")
	}
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
