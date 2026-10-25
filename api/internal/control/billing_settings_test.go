package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func billingFixtureConfig() billingConfig {
	cfg := billingConfig{Version: 1, Operators: []string{"billing_operator"}, Organizations: []string{"org_billing"}, ReturnOrigin: "http://127.0.0.1:31888", Offers: []billingOffer{{ID: "offer_" + strings.Repeat("a", 24), Name: "Synthetic prepaid AI offer", AmountMinor: 125, CreditMicros: 1000000}}}
	cfg.Provider.Type = "stripe"
	cfg.AllowNonLiveCredits = true
	cfg.Provider.Origin = "http://127.0.0.1:31889"
	cfg.Provider.CheckoutOrigin = "http://127.0.0.1:31889"
	cfg.Provider.Fixture = true
	cfg.Provider.Key = "synthetic-provider-secret-at-least-32-characters"
	cfg.Provider.Account = "acct_fixture"
	cfg.Provider.APIVersion = "2024-06-20"
	cfg.Provider.WebhookSecrets = []string{"whsec_synthetic_fixture_not_an_actual_credential"}
	return cfg
}

func TestBillingPolicySeparateApprovalOfferRevisionAndRotation(t *testing.T) {
	cfg := billingFixtureConfig()
	encode := func(cfg billingConfig) []byte { raw, _ := json.Marshal(cfg); return raw }
	p, err := newBillingSettings(encode(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !p.permits(user{ID: "billing_operator"}, "org_billing") || p.permits(user{ID: "provider_operator", Role: "admin"}, "org_billing") || p.permits(user{ID: "billing_operator", APIKey: true}, "org_billing") || p.permits(user{ID: "billing_operator"}, "org_other") {
		t.Fatal("billing approval boundary")
	}
	offer := p.offers[cfg.Offers[0].ID]
	if len(offer.Revision) != 64 {
		t.Fatal("immutable offer revision absent")
	}
	cfg.Provider.Key = "synthetic-rotated-provider-secret-at-least-32-characters"
	cfg.Provider.WebhookSecrets = []string{"whsec_synthetic_second_valid_signing_secret"}
	rotated, err := newBillingSettings(encode(cfg))
	if err != nil || rotated.identity != p.identity || rotated.offers[offer.ID].Revision != offer.Revision {
		t.Fatal("credential rotation changed merchant/price identity", err)
	}
	cfg.Offers[0].CreditMicros++
	changed, err := newBillingSettings(encode(cfg))
	if err != nil || changed.offers[offer.ID].Revision == offer.Revision {
		t.Fatal("price revision failed to bind credited units", err)
	}
	cfg.Provider.Account = "acct_othermerchant"
	other, err := newBillingSettings(encode(cfg))
	if err != nil || other.identity == p.identity {
		t.Fatal("merchant identity ignored", err)
	}
}

func TestBillingPolicyRejectsUnapprovedNetworkAndImplicitMoney(t *testing.T) {
	base := billingFixtureConfig()
	encode := func(cfg billingConfig) []byte { raw, _ := json.Marshal(cfg); return raw }
	for _, change := range []func(*billingConfig){
		func(c *billingConfig) { c.AllowNonLiveCredits = false },
		func(c *billingConfig) { c.Organizations = nil },
		func(c *billingConfig) { c.Provider.Live = true },
		func(c *billingConfig) { c.Provider.Origin = "http://example.com" },
		func(c *billingConfig) { c.Provider.Origin = "http://127.0.0.1:31889/path" },
		func(c *billingConfig) { c.Provider.Origin = "http://user:secret@127.0.0.1:31889" },
		func(c *billingConfig) { c.Provider.Origin = "http://127.0.0.1:31889?redirect=other" },
		func(c *billingConfig) { c.Provider.Fixture = false },
		func(c *billingConfig) {
			c.Provider.WebhookSecrets = append(c.Provider.WebhookSecrets, c.Provider.WebhookSecrets[0])
		},
		func(c *billingConfig) { c.Offers[0].AmountMinor = 49 },
		func(c *billingConfig) { c.Offers[0].AmountMinor = 100000000 },
		func(c *billingConfig) { c.Offers[0].CreditMicros = 0 },
		func(c *billingConfig) { c.Offers[0].Revision = strings.Repeat("a", 64) },
		func(c *billingConfig) { c.Operators = append(c.Operators, c.Operators[0]) },
	} {
		// Independent JSON copies avoid aliasing slice mutations between cases.
		var cfg billingConfig
		json.Unmarshal(encode(base), &cfg)
		change(&cfg)
		if _, err := newBillingSettings(encode(cfg)); err == nil {
			t.Fatal("unsafe billing configuration accepted")
		}
	}
	raw := strings.Replace(string(encode(base)), `"version":1`, `"version":1,"version":1`, 1)
	if _, err := newBillingSettings([]byte(raw)); err == nil {
		t.Fatal("duplicate approval key")
	}
	var absent *billingSettings
	if absent.permits(user{ID: "billing_operator"}, "org_billing") || absent.allowsOrg("org_billing") {
		t.Fatal("absent billing enabled")
	}
}
