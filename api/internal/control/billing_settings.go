package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/billing"
)

type billingOffer struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AmountMinor  int64  `json:"amount_minor"`
	CreditMicros int64  `json:"credit_micros"`
	Revision     string `json:"revision,omitempty"`
}

type billingConfig struct {
	Version             int      `json:"version"`
	Operators           []string `json:"operator_ids"`
	Organizations       []string `json:"allowed_organizations,omitempty"`
	ReturnOrigin        string   `json:"return_origin"`
	AllowNonLiveCredits bool     `json:"allow_non_live_credit_issuance,omitempty"`
	Provider            struct {
		Type           string   `json:"type"`
		Origin         string   `json:"origin"`
		CheckoutOrigin string   `json:"checkout_origin"`
		Account        string   `json:"account_id"`
		APIVersion     string   `json:"api_version"`
		Live           bool     `json:"livemode"`
		Fixture        bool     `json:"private_fixture"`
		Key            string   `json:"api_key"`
		WebhookSecrets []string `json:"webhook_secrets"`
	} `json:"provider"`
	Offers []billingOffer `json:"offers"`
}

type billingSettings struct {
	config                   billingConfig
	identity                 string
	operators, organizations map[string]bool
	offers                   map[string]billingOffer
	provider                 *billing.Stripe
}

var errBillingConfig = errors.New("approved billing configuration unavailable or invalid")
var billingOfferID = regexp.MustCompile(`^offer_[a-f0-9]{24}$`)

func billingOrigin(raw string, fixture bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Opaque != "" || u.Scheme != "https" && u.Scheme != "http" {
		return nil, errBillingConfig
	}
	if u.Port() != "" {
		port, err := net.LookupPort("tcp", u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errBillingConfig
		}
	}
	if fixture {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() && !ip.IsPrivate() {
			return nil, errBillingConfig
		}
	} else if u.Scheme != "https" {
		return nil, errBillingConfig
	}
	return u, nil
}

func newBillingSettings(raw []byte) (*billingSettings, error) {
	if len(raw) == 0 || len(raw) > 65536 {
		return nil, errBillingConfig
	}
	var cfg billingConfig
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if billing.DecodeJSON(raw, &cfg) != nil || d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF || cfg.Version != 1 || len(cfg.Operators) < 1 || len(cfg.Operators) > 16 || len(cfg.Organizations) > 128 || len(cfg.Offers) < 1 || len(cfg.Offers) > 64 || cfg.Provider.Type != "stripe" || !regexp.MustCompile(`^acct_[A-Za-z0-9]{3,128}$`).MatchString(cfg.Provider.Account) || len(cfg.Provider.WebhookSecrets) < 1 || len(cfg.Provider.WebhookSecrets) > 2 {
		return nil, errBillingConfig
	}
	// Non-live issuance needs explicit protected approval and an exact scoped
	// allowlist; test payments must never implicitly fund every organization.
	if !cfg.Provider.Live && (!cfg.AllowNonLiveCredits || len(cfg.Organizations) == 0) || cfg.Provider.Live && cfg.AllowNonLiveCredits {
		return nil, errBillingConfig
	}
	if cfg.Provider.Origin == "" {
		cfg.Provider.Origin = "https://api.stripe.com"
	}
	if cfg.Provider.CheckoutOrigin == "" {
		cfg.Provider.CheckoutOrigin = "https://checkout.stripe.com"
	}
	provider, err := billingOrigin(cfg.Provider.Origin, cfg.Provider.Fixture)
	if err != nil {
		return nil, err
	}
	if _, err = billingOrigin(cfg.Provider.CheckoutOrigin, cfg.Provider.Fixture); err != nil {
		return nil, err
	}
	if _, err = billingOrigin(cfg.ReturnOrigin, cfg.Provider.Fixture); err != nil {
		return nil, err
	}
	if cfg.Provider.Fixture {
		if cfg.Provider.Live || len(cfg.Provider.Key) < 24 || len(cfg.Provider.Key) > 512 {
			return nil, errBillingConfig
		}
	} else {
		prefix := "sk_test_"
		if cfg.Provider.Live {
			prefix = "sk_live_"
		}
		if cfg.Provider.Origin != "https://api.stripe.com" || cfg.Provider.CheckoutOrigin != "https://checkout.stripe.com" || !strings.HasPrefix(cfg.Provider.Key, prefix) || len(cfg.Provider.Key) < 24 || len(cfg.Provider.Key) > 512 {
			return nil, errBillingConfig
		}
	}
	seenSecrets := map[string]bool{}
	for _, secret := range cfg.Provider.WebhookSecrets {
		if !strings.HasPrefix(secret, "whsec_") || len(secret) < 24 || len(secret) > 256 || strings.ContainsAny(secret, "\r\n") || seenSecrets[secret] {
			return nil, errBillingConfig
		}
		seenSecrets[secret] = true
	}
	p := &billingSettings{config: cfg, operators: map[string]bool{}, organizations: map[string]bool{}, offers: map[string]billingOffer{}}
	for _, id := range cfg.Operators {
		if !aiProviderUserID.MatchString(id) || p.operators[id] {
			return nil, errBillingConfig
		}
		p.operators[id] = true
	}
	for _, id := range cfg.Organizations {
		if !aiProviderUserID.MatchString(id) || p.organizations[id] {
			return nil, errBillingConfig
		}
		p.organizations[id] = true
	}
	identity, _ := json.Marshal([]any{"neon-billing-provider-v1", cfg.Provider.Type, cfg.Provider.Origin, cfg.Provider.Account, cfg.Provider.Live, cfg.Provider.Fixture})
	hash := sha256.Sum256(identity)
	p.identity = hex.EncodeToString(hash[:])
	for _, offer := range cfg.Offers {
		if !billingOfferID.MatchString(offer.ID) || !displayName(offer.Name) || offer.AmountMinor < billing.MinMinor || offer.AmountMinor > billing.MaxMinor || offer.CreditMicros < 1 || offer.CreditMicros > billing.MaxCredits || offer.Revision != "" || p.offers[offer.ID].ID != "" {
			return nil, errBillingConfig
		}
		encoded, _ := json.Marshal([]any{p.identity, offer})
		digest := sha256.Sum256(encoded)
		offer.Revision = hex.EncodeToString(digest[:])
		p.offers[offer.ID] = offer
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	// Literal private fixture requests never use the WAN proxy. Production is
	// pinned to the approved Stripe origin and uses the existing WAN environment.
	transport.Proxy = func(r *http.Request) (*url.URL, error) {
		if cfg.Provider.Fixture {
			return nil, nil
		}
		return http.ProxyFromEnvironment(r)
	}
	if cfg.Provider.Fixture {
		dial := &net.Dialer{Timeout: 10 * time.Second}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(address)
			if err != nil || host != provider.Hostname() {
				return nil, errBillingConfig
			}
			return dial.DialContext(ctx, network, address)
		}
	}
	p.provider, err = billing.NewStripe(&http.Client{Transport: transport}, cfg.Provider.Origin, cfg.Provider.Key, cfg.Provider.APIVersion)
	if err != nil {
		return nil, errBillingConfig
	}
	return p, nil
}

func loadBillingSettings() (*billingSettings, error) {
	path := os.Getenv("NEON_BILLING_CONFIG_FILE")
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errBillingConfig
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, errBillingConfig
	}
	return newBillingSettings(raw)
}

func (p *billingSettings) permits(u user, org string) bool {
	return p != nil && !u.APIKey && p.operators[u.ID] && (org == "" || len(p.organizations) == 0 || p.organizations[org])
}
func (p *billingSettings) allowsOrg(org string) bool {
	return p != nil && (len(p.organizations) == 0 || p.organizations[org])
}
