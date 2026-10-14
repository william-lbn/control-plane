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

func TestManagedAuthOTPPolicyIsOptInAndDoesNotExposeCodeRetrieval(t *testing.T) {
	t.Setenv("NEON_AUTH_MAIL_ENABLED", "")
	t.Setenv("NEON_AUTH_MAIL_SECRET", "")
	p := managedAuthPayload{BranchID: "br_0123456789abcdef", Spec: ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}, EmailOTPEnabled: true}}
	if validateManagedAuth(p) == nil {
		t.Fatal("OTP enabled without operator mail")
	}
	if managedAuthAllowedAction("sign-in/email-otp", "POST") {
		t.Fatal("OTP exposed without operator mail")
	}
	t.Setenv("NEON_AUTH_MAIL_ENABLED", "true")
	t.Setenv("NEON_AUTH_MAIL_SECRET", "auth-mail-ci")
	if validateManagedAuth(p) != nil {
		t.Fatal("Configured OTP policy rejected")
	}
	for _, route := range []string{"email-otp/send-verification-otp", "email-otp/verify-email", "email-otp/check-verification-otp", "email-otp/request-password-reset", "email-otp/reset-password", "sign-in/email-otp"} {
		if !managedAuthAllowedAction(route, "POST") || managedAuthAllowedAction(route, "GET") {
			t.Fatal("OTP method boundary violated")
		}
	}
	if managedAuthAllowedAction("email-otp/get-verification-otp", "POST") {
		t.Fatal("Private stored code retrieval exposed")
	}
	data, e := json.Marshal(ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}})
	if e != nil || strings.Contains(string(data), "email_otp_enabled") {
		t.Fatal("Default policy changes legacy request hashes")
	}
}

func TestManagedAuthMagicLinkPolicyAndMethods(t *testing.T) {
	t.Setenv("NEON_AUTH_MAIL_ENABLED", "")
	t.Setenv("NEON_AUTH_MAIL_SECRET", "")
	p := managedAuthPayload{BranchID: "br_0123456789abcdef", Spec: ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}, MagicLinkEnabled: true}}
	if validateManagedAuth(p) == nil || managedAuthAllowedAction("sign-in/magic-link", "POST") {
		t.Fatal("Magic Link requires operator mail")
	}
	t.Setenv("NEON_AUTH_MAIL_ENABLED", "true")
	t.Setenv("NEON_AUTH_MAIL_SECRET", "auth-mail-ci")
	if validateManagedAuth(p) != nil {
		t.Fatal("Default lifetime rejected")
	}
	p.Spec.MagicLinkExpiresIn = 86400
	if validateManagedAuth(p) != nil {
		t.Fatal("Documented maximum lifetime rejected")
	}
	for _, seconds := range []int{-1, 299, 86401} {
		p.Spec.MagicLinkExpiresIn = seconds
		if validateManagedAuth(p) == nil {
			t.Fatal("Invalid link lifetime accepted")
		}
	}
	if !managedAuthAllowedAction("sign-in/magic-link", "POST") || managedAuthAllowedAction("sign-in/magic-link", "GET") || !managedAuthAllowedAction("magic-link/verify", "GET") || managedAuthAllowedAction("magic-link/verify", "POST") {
		t.Fatal("Magic Link method boundary violated")
	}
	data, e := json.Marshal(ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}})
	if e != nil || strings.Contains(string(data), "magic_link") {
		t.Fatal("Legacy request hash changed")
	}
}
