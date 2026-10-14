package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManagedAuthMailPolicyPreservesDefaultHashAndLimitsRoutes(t *testing.T) {
	t.Setenv("NEON_AUTH_MAIL_ENABLED", "")
	t.Setenv("NEON_AUTH_MAIL_SECRET", "")
	data, e := json.Marshal(ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}})
	if e != nil || strings.Contains(string(data), "require_email_verification") {
		t.Fatal("Old durable request shape changed")
	}
	if managedAuthAllowedAction("reset-password", "POST") {
		t.Fatal("Unconfigured mail action exposed")
	}
	t.Setenv("NEON_AUTH_MAIL_ENABLED", "true")
	t.Setenv("NEON_AUTH_MAIL_SECRET", "auth-mail-ci")
	if !managedAuthAllowedAction("request-password-reset", "POST") || !managedAuthAllowedAction("verify-email", "GET") || !managedAuthAllowedAction("reset-password/abcdefghijklmnopqrstuvwxyz", "GET") {
		t.Fatal("Configured mail routes unavailable")
	}
	for _, action := range []string{"reset-password/../token", "reset-password/x", "unknown", "reset-password/abcdefghijklmnopqrstuvwxyz/more"} {
		if managedAuthAllowedAction(action, "GET") {
			t.Fatal("Unallowlisted action exposed")
		}
	}
	if managedAuthAllowedAction("request-password-reset", "GET") {
		t.Fatal("Mail mutation accepted GET")
	}
	p := managedAuthPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", SecretRef: "auth-ci", Generation: 1}
	deployment := managedAuthDeployment(p, "neon", true)
	if !managedAuthMailMounted(deployment) {
		t.Fatal("Operator mail mount missing")
	}
	containers := nested(deployment, "spec", "template", "spec", "containers").([]any)
	container := containers[0].(map[string]any)
	container["env"] = []any{}
	if managedAuthMailMounted(deployment) {
		t.Fatal("Volume alone falsely claims configured mail runtime")
	}
}

func TestManagedAuthMailCallbackTrust(t *testing.T) {
	spec := ManagedAuthSpec{AllowedOrigins: []string{"https://app.example.test"}}
	for _, target := range []string{"https://auth.example.test/?auth_branch=br_0123456789abcdef#/projects/x/auth", "https://app.example.test/recovery"} {
		if !managedAuthCallbackTrusted(target, spec, "https://auth.example.test") {
			t.Fatal("Trusted callback denied")
		}
	}
	for _, target := range []string{"//app.example.test/reset", "https://app.example.test.attacker.test/", "https://user:pass@app.example.test/", "javascript:alert(1)", "https://unrelated.example/reset", "https://app.example.test:444/reset"} {
		if managedAuthCallbackTrusted(target, spec, "https://auth.example.test") {
			t.Fatal("Untrusted callback accepted")
		}
	}
}
