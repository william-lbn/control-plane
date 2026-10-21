package control

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
	"net/http/httptest"
	"testing"
)

func providerSettingsFixture(t *testing.T, base string, lab bool) *aiProviderSettings {
	t.Helper()
	pub, priv, err := aigateway.GenerateProviderKeysets("test1")
	if err != nil {
		t.Fatal(err)
	}
	policy := aiProviderPolicyInput{Version: 1, OperatorIDs: []string{"provider_operator"}, ApprovedEndpoints: []aigateway.ApprovedProviderEndpoint{{BaseURL: base, AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true}}, AllowLabSecretInput: lab}
	raw, _ := json.Marshal(policy)
	settings, err := newAIProviderSettings(raw, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}
func TestAIProviderSettingsOperatorAndSecretTransport(t *testing.T) {
	p := providerSettingsFixture(t, "http://127.0.0.1:11437/v1", true)
	for _, actor := range []user{{ID: "owner", Role: "owner"}, {ID: "admin", Role: "admin"}, {ID: "provider_operator", APIKey: true}} {
		if p.isOperator(actor) {
			t.Fatal("account role or API key gained platform operator permission")
		}
	}
	if !p.isOperator(user{ID: "provider_operator", Role: "viewer"}) {
		t.Fatal("explicit operator identity ignored")
	}
	r := httptest.NewRequest("POST", "http://console.local/api", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	if p.permitsSecretInput(r, "https://example.com/v1", true, true) {
		t.Fatal("forged forwarded HTTPS trusted")
	}
	if p.permitsSecretInput(r, "http://127.0.0.1:11437/v1", false, false) {
		t.Fatal("lab acknowledgment omitted")
	}
	if !p.permitsSecretInput(r, "http://127.0.0.1:11437/v1", true, false) {
		t.Fatal("exact lab exception rejected")
	}
	if p.permitsSecretInput(r, "http://127.0.0.1:11438/v1", true, false) || p.permitsSecretInput(r, "https://example.com/v1", true, false) {
		t.Fatal("lab exception escaped approved origin")
	}
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}
	if p.permitsSecretInput(r, "https://example.com/v1", false, false) {
		t.Fatal("insecure cookie session admitted raw provider key")
	}
	if !p.permitsSecretInput(r, "https://example.com/v1", false, true) {
		t.Fatal("real TLS input rejected")
	}
	p.policy.AllowLabSecretInput = false
	r.TLS = nil
	if p.permitsSecretInput(r, "http://127.0.0.1:11437/v1", true, false) {
		t.Fatal("disabled lab mode admitted secret")
	}
}
func TestAIProviderSettingsApprovedSourcesAndKeyProjection(t *testing.T) {
	p := providerSettingsFixture(t, "http://127.0.0.1:11437/v1", false)
	policy := p.policy
	policy.CredentialSources = []aiProviderCredentialSource{{ID: "lab-key", Name: "Approved lab transport key", Namespace: "neon", SecretName: "approved-key", SecretUID: "11111111-2222-4333-8444-555555555555", DataKey: "provider-key", AllowedBaseURLs: []string{"http://127.0.0.1:11437/v1/"}}}
	raw, _ := json.Marshal(policy)
	pub, _ := p.sealer.PublicKeyset()
	settings, err := newAIProviderSettings(raw, pub, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings.sourceFor("lab-key", "http://127.0.0.1:11437/v1"); !ok {
		t.Fatal("approved source rejected")
	}
	if _, ok := settings.sourceFor("lab-key", "http://127.0.0.1:11438/v1"); ok {
		t.Fatal("credential source crossed endpoints")
	}
	_, different, _ := aigateway.GenerateProviderKeysets("test1")
	if _, err = newAIProviderSettings(raw, pub, different); err == nil {
		t.Fatal("mismatched private keyring accepted")
	}
	policy.CredentialSources[0].Namespace = "other"
	raw, _ = json.Marshal(policy)
	if _, err = newAIProviderSettings(raw, pub, nil); err == nil {
		t.Fatal("arbitrary namespace accepted")
	}
	t.Setenv("NEON_AI_PROVIDER_MANAGEMENT_ENABLED", "false")
	t.Setenv("NEON_AI_PROVIDER_PRIVATE_KEYRING_FILE", "missing")
	if value, err := loadAIProviderSettings(true); value != nil || err != nil {
		t.Fatal("disabled feature read key files")
	}
	encoded, _ := json.Marshal(p)
	if !bytes.Equal(encoded, []byte("{}")) {
		t.Fatal("settings exported private material")
	}
}
