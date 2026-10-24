package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed assets/auth-phone-schema.sql
var managedAuthPhoneSchema string

//go:embed assets/auth-webhook-schema.sql
var managedAuthWebhookSchema string

//go:embed assets/auth-oauth-schema.sql
var managedAuthOAuthSchema string

var managedAuthIntegrationID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)
var managedAuthWebhookEvents = []string{"send.otp", "send.magic_link", "user.before_create", "user.created", "phone_number.verified", "organization.invitation.created", "organization.invitation.accepted"}

type managedAuthDestination struct {
	URL            string   `json:"url"`
	Addresses      []string `json:"addresses"`
	AllowLabHTTP   bool     `json:"allowLabHTTP,omitempty"`
	CAPEM          string   `json:"caPEM,omitempty"`
	EgressProxyURL string   `json:"egressProxyURL,omitempty"`
}
type managedAuthOAuthProvider struct {
	ID               string                  `json:"id"`
	Kind             string                  `json:"kind"`
	ClientID         string                  `json:"clientID"`
	ClientSecret     string                  `json:"clientSecret"`
	AuthorizationURL string                  `json:"authorizationURL"`
	Token            managedAuthDestination  `json:"token"`
	UserInfo         *managedAuthDestination `json:"userInfo,omitempty"`
	Emails           *managedAuthDestination `json:"emails,omitempty"`
	JWKS             *managedAuthDestination `json:"jwks,omitempty"`
	Issuer           string                  `json:"issuer,omitempty"`
	Scopes           []string                `json:"scopes"`
}
type managedAuthIntegrationScope struct {
	ProjectIDs []string `json:"project_ids"`
	BranchIDs  []string `json:"branch_ids,omitempty"`
}
type managedAuthOperatorRegistry struct {
	Version  int `json:"version"`
	Webhooks map[string]struct {
		managedAuthIntegrationScope
		Destination managedAuthDestination `json:"destination"`
	} `json:"webhooks"`
	OAuth map[string]struct {
		managedAuthIntegrationScope
		Provider managedAuthOAuthProvider `json:"provider"`
	} `json:"oauth"`
}

func managedAuthHookSelected(spec ManagedAuthSpec, event string) bool {
	return spec.WebhookID != "" && slices.Contains(spec.WebhookEvents, event)
}
func managedAuthUsesIntegrations(spec ManagedAuthSpec) bool {
	return spec.WebhookID != "" || len(spec.OAuthProviders) > 0
}
func validateManagedAuthIntegrations(spec ManagedAuthSpec) error {
	bad := errors.New("invalid approved Auth integration selection")
	if spec.WebhookID == "" {
		if len(spec.WebhookEvents) > 0 || spec.WebhookTimeoutSeconds != 0 {
			return bad
		}
	} else {
		if !managedAuthIntegrationID.MatchString(spec.WebhookID) || len(spec.WebhookEvents) == 0 || len(spec.WebhookEvents) > 7 {
			return bad
		}
		seen := map[string]bool{}
		for _, event := range spec.WebhookEvents {
			if seen[event] || !slices.Contains(managedAuthWebhookEvents, event) {
				return bad
			}
			seen[event] = true
		}
		if spec.WebhookTimeoutSeconds != 0 && (spec.WebhookTimeoutSeconds < 1 || spec.WebhookTimeoutSeconds > 10) {
			return bad
		}
	}
	if !spec.PhoneNumberEnabled && spec.PhoneNumberExpiresIn != 0 || spec.PhoneNumberEnabled && (!managedAuthHookSelected(spec, "send.otp") || spec.PhoneNumberExpiresIn != 0 && (spec.PhoneNumberExpiresIn < 60 || spec.PhoneNumberExpiresIn > 600)) {
		return bad
	}
	if len(spec.OAuthProviders) > 8 {
		return bad
	}
	seen := map[string]bool{}
	for _, id := range spec.OAuthProviders {
		if !managedAuthIntegrationID.MatchString(id) || seen[id] {
			return bad
		}
		seen[id] = true
	}
	if managedAuthUsesIntegrations(spec) && !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`).MatchString(os.Getenv("NEON_AUTH_INTEGRATIONS_SECRET")) {
		return errors.New("operator Auth integration registry is required")
	}
	return nil
}
func validManagedAuthDestination(d managedAuthDestination) bool {
	u, e := url.Parse(d.URL)
	if e != nil || len(d.URL) > 2048 || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Host == "" || strings.ContainsAny(u.Host, "%\\* \t\r\n") || (u.Scheme != "https" && !(d.AllowLabHTTP && os.Getenv("NEON_AUTH_LAB_HTTP") == "true" && u.Scheme == "http")) || d.AllowLabHTTP && os.Getenv("NEON_AUTH_LAB_HTTP") != "true" || len(d.Addresses) < 1 || len(d.Addresses) > 8 || len(d.CAPEM) > 65536 || d.CAPEM != "" && !strings.Contains(d.CAPEM, "-----BEGIN CERTIFICATE-----") {
		return false
	}
	seen := map[netip.Addr]bool{}
	for _, raw := range d.Addresses {
		ip, err := netip.ParseAddr(raw)
		ip = ip.Unmap()
		if err != nil || ip.Zone() != "" || seen[ip] || !d.AllowLabHTTP && !managedAuthPublicAddress(ip) {
			return false
		}
		seen[ip] = true
	}
	if !d.AllowLabHTTP {
		if _, err := netip.ParseAddr(u.Hostname()); err == nil {
			return false
		}
	}
	if d.EgressProxyURL != "" {
		p, e := url.Parse(d.EgressProxyURL)
		if e != nil || p.Scheme != "http" || p.User != nil || p.RawQuery != "" || p.Fragment != "" || p.Path != "/" || p.Opaque != "" {
			return false
		}
		ip, e := netip.ParseAddr(p.Hostname())
		if e != nil || ip.Zone() != "" || p.Port() == "" {
			return false
		}
	}
	return true
}
func managedAuthPublicAddress(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && (!netip.MustParsePrefix("2000::/3").Contains(ip) || netip.MustParsePrefix("2002::/16").Contains(ip)) {
		return false
	}
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "2001:db8::/32", "2001::/32", "2001:10::/28", "2001:20::/28", "2001:2::/48"} {
		if netip.MustParsePrefix(raw).Contains(ip) {
			return false
		}
	}
	return true
}
func managedAuthIntegrationScopeAllows(scope managedAuthIntegrationScope, p managedAuthPayload) bool {
	if len(scope.ProjectIDs) == 0 || len(scope.ProjectIDs) > 100 || len(scope.BranchIDs) > 100 {
		return false
	}
	for _, id := range scope.ProjectIDs {
		if !regexp.MustCompile(`^prj_[a-f0-9]{16}$`).MatchString(id) {
			return false
		}
	}
	for _, id := range scope.BranchIDs {
		if !dataBranchID.MatchString(id) {
			return false
		}
	}
	return slices.Contains(scope.ProjectIDs, p.ProjectID) && (len(scope.BranchIDs) == 0 || slices.Contains(scope.BranchIDs, p.BranchID))
}

// Only selected, authorized entries enter a branch's immutable configuration.
// The global operator registry is never mounted in a branch workload.
func (s *server) selectedManagedAuthIntegrations(ctx context.Context, p managedAuthPayload) (record, string, error) {
	result := record{}
	if !managedAuthUsesIntegrations(p.Spec) {
		return result, "", nil
	}
	unavailable := errors.New("approved Auth integration unavailable")
	registry, uid, err := s.readManagedAuthOperatorRegistry(ctx, p.IntegrationsSecretName, p.IntegrationsSecretUID)
	if err != nil {
		return nil, "", err
	}
	if p.Spec.WebhookID != "" {
		entry, ok := registry.Webhooks[p.Spec.WebhookID]
		if !ok || !managedAuthIntegrationScopeAllows(entry.managedAuthIntegrationScope, p) || !validManagedAuthDestination(entry.Destination) {
			return nil, "", unavailable
		}
		seed, err := s.managedAuthIntegrationKey(p, "webhook.ed25519")
		if err != nil {
			return nil, "", err
		}
		timeout := p.Spec.WebhookTimeoutSeconds
		if timeout == 0 {
			timeout = 5
		}
		result["webhook"] = record{"destination": entry.Destination, "events": p.Spec.WebhookEvents, "timeoutSeconds": timeout, "signingSeed": seed}
	}
	if len(p.Spec.OAuthProviders) > 0 {
		providers := []managedAuthOAuthProvider{}
		for _, id := range p.Spec.OAuthProviders {
			entry, ok := registry.OAuth[id]
			v := entry.Provider
			if !ok || v.ID != id || !managedAuthIntegrationScopeAllows(entry.managedAuthIntegrationScope, p) || !validManagedAuthOAuthProvider(v) {
				return nil, "", unavailable
			}
			providers = append(providers, v)
		}
		result["oauthProviders"] = providers
	}
	return result, uid, nil
}
func (s *server) readManagedAuthOperatorRegistry(ctx context.Context, name, expectedUID string) (managedAuthOperatorRegistry, string, error) {
	unavailable := errors.New("approved Auth integration unavailable")
	var registry managedAuthOperatorRegistry
	if name == "" {
		return registry, "", unavailable
	}
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", name), nil)
	uid := stringVal(nested(secret, "metadata", "uid"))
	if err != nil || secret["immutable"] != true || uid == "" || expectedUID != "" && expectedUID != uid {
		return registry, "", unavailable
	}
	raw, err := secretText(secret, "config.json")
	if err != nil || len(raw) > 131072 {
		return registry, "", unavailable
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&registry) != nil || decoder.Decode(new(any)) != io.EOF || registry.Version != 1 || len(registry.Webhooks) > 32 || len(registry.OAuth) > 32 {
		return registry, "", unavailable
	}
	return registry, uid, nil
}
func (s *server) availableManagedAuthIntegrations(ctx context.Context, p managedAuthPayload) record {
	result := record{"available": false, "webhooks": []record{}, "oauth_providers": []record{}}
	if os.Getenv("NEON_AUTH_INTEGRATIONS_SECRET") == "" {
		return result
	}
	registry, _, err := s.readManagedAuthOperatorRegistry(ctx, os.Getenv("NEON_AUTH_INTEGRATIONS_SECRET"), "")
	if err != nil {
		return result
	}
	result["available"] = true
	hooks, providers := []record{}, []record{}
	hookIDs, providerIDs := []string{}, []string{}
	for id := range registry.Webhooks {
		hookIDs = append(hookIDs, id)
	}
	for id := range registry.OAuth {
		providerIDs = append(providerIDs, id)
	}
	slices.Sort(hookIDs)
	slices.Sort(providerIDs)
	for _, id := range hookIDs {
		entry := registry.Webhooks[id]
		if managedAuthIntegrationID.MatchString(id) && managedAuthIntegrationScopeAllows(entry.managedAuthIntegrationScope, p) && validManagedAuthDestination(entry.Destination) {
			hooks = append(hooks, record{"id": id})
		}
	}
	for _, id := range providerIDs {
		entry := registry.OAuth[id]
		if entry.Provider.ID == id && managedAuthIntegrationScopeAllows(entry.managedAuthIntegrationScope, p) && validManagedAuthOAuthProvider(entry.Provider) {
			providers = append(providers, record{"id": id, "kind": entry.Provider.Kind, "callback_url": managedAuthBase(p.BranchID) + "/callback/" + id})
		}
	}
	result["webhooks"] = hooks
	result["oauth_providers"] = providers
	return result
}
func validManagedAuthOAuthProvider(v managedAuthOAuthProvider) bool {
	if !managedAuthIntegrationID.MatchString(v.ID) || (v.Kind != "oidc" && v.Kind != "github" && v.Kind != "vercel") || len(v.ClientID) < 1 || len(v.ClientID) > 512 || len(v.ClientSecret) < 1 || len(v.ClientSecret) > 4096 || strings.ContainsAny(v.ClientID+v.ClientSecret, "\r\n\x00") || len(v.Scopes) < 1 || len(v.Scopes) > 8 || !validManagedAuthDestination(v.Token) {
		return false
	}
	for _, scope := range v.Scopes {
		if !regexp.MustCompile(`^[a-zA-Z0-9:_./-]{1,64}$`).MatchString(scope) {
			return false
		}
	}
	for _, d := range []*managedAuthDestination{v.UserInfo, v.Emails, v.JWKS} {
		if d != nil && !validManagedAuthDestination(*d) {
			return false
		}
	}
	u, err := url.Parse(v.AuthorizationURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || len(v.AuthorizationURL) > 2048 || (u.Scheme != "https" && !(v.Token.AllowLabHTTP && u.Scheme == "http")) {
		return false
	}
	if v.Kind == "oidc" {
		issuer, e := url.Parse(v.Issuer)
		return e == nil && issuer.Host != "" && issuer.User == nil && issuer.RawQuery == "" && issuer.Fragment == "" && v.JWKS != nil && slices.Contains(v.Scopes, "openid") && (issuer.Scheme == "https" || v.Token.AllowLabHTTP && issuer.Scheme == "http")
	}
	if v.Kind == "github" {
		return v.UserInfo != nil && v.Emails != nil
	}
	return v.UserInfo != nil
}
func (s *server) managedAuthIntegrationKey(p managedAuthPayload, purpose string) (string, error) {
	if len(s.idempotencyKey) < 32 || !dataBranchID.MatchString(p.BranchID) || !regexp.MustCompile(`^prj_[a-f0-9]{16}$`).MatchString(p.ProjectID) || (purpose != "phone.otp" && purpose != "webhook.ed25519") {
		return "", errors.New("Auth integration key identity unavailable")
	}
	mac := hmac.New(sha256.New, s.idempotencyKey)
	mac.Write([]byte("neon.auth.integration." + purpose + ".v1\x00" + p.ProjectID + "\x00" + p.BranchID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func applyManagedAuthIntegrationsTx(ctx context.Context, tx pgx.Tx, p managedAuthPayload, advanced bool) error {
	if len(p.Spec.OAuthProviders) > 0 {
		if _, err := tx.Exec(ctx, managedAuthOAuthSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
	}
	var phone, hooks bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('neon_auth.control_phone_challenge') IS NOT NULL,to_regclass('neon_auth.control_webhook_policy') IS NOT NULL`).Scan(&phone, &hooks); err != nil {
		return err
	}
	if p.Spec.PhoneNumberEnabled || phone {
		if _, err := tx.Exec(ctx, managedAuthPhoneSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		if advanced {
			if _, err := tx.Exec(ctx, `DELETE FROM neon_auth.control_phone_challenge`); err != nil {
				return err
			}
		}
	}
	if p.Spec.WebhookID == "" && !hooks {
		return nil
	}
	if _, err := tx.Exec(ctx, managedAuthWebhookSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('neon-auth-webhook:'||branch_id,922751)) FROM neon_auth.control_webhook_policy WHERE singleton`); err != nil {
		return err
	}
	if advanced {
		if _, err := tx.Exec(ctx, `UPDATE neon_auth.control_webhook_event SET state='discarded',lease_token=NULL,lease_until=NULL,last_error='issuer_generation_changed' WHERE state IN ('pending','delivering')`); err != nil {
			return err
		}
	}
	events := p.Spec.WebhookEvents
	if events == nil {
		events = []string{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO neon_auth.control_webhook_policy(singleton,enabled,branch_id,project_id,endpoint_id,generation,events) VALUES(true,$1,$2,$3,$4,$5,$6) ON CONFLICT(singleton) DO UPDATE SET enabled=EXCLUDED.enabled,branch_id=EXCLUDED.branch_id,project_id=EXCLUDED.project_id,endpoint_id=EXCLUDED.endpoint_id,generation=EXCLUDED.generation,events=EXCLUDED.events`, p.Spec.WebhookID != "", p.BranchID, p.ProjectID, p.EndpointID, p.Generation, events)
	return err
}
func grantManagedAuthIntegrationsTx(ctx context.Context, tx pgx.Tx, login string, p ManagedAuthSpec) error {
	for _, table := range []string{"control_phone_challenge", "control_webhook_policy", "control_webhook_event"} {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "neon_auth."+table).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.Exec(ctx, "REVOKE ALL ON TABLE neon_auth."+pgQuote(table)+" FROM "+pgQuote(login)); err != nil {
			return err
		}
		if table == "control_webhook_event" {
			if _, err := tx.Exec(ctx, "REVOKE UPDATE(state,delivery_batches,lease_token,lease_until,last_error,delivered_at) ON TABLE neon_auth.control_webhook_event FROM "+pgQuote(login)); err != nil {
				return err
			}
		}
		grants := ""
		if table == "control_phone_challenge" && p.PhoneNumberEnabled {
			grants = "SELECT,INSERT,UPDATE,DELETE"
		} else if table != "control_phone_challenge" && p.WebhookID != "" {
			grants = "SELECT"
		}
		if grants != "" {
			if _, err := tx.Exec(ctx, "GRANT "+grants+" ON TABLE neon_auth."+pgQuote(table)+" TO "+pgQuote(login)); err != nil {
				return err
			}
		}
		if table == "control_webhook_event" && p.WebhookID != "" {
			if _, err := tx.Exec(ctx, "GRANT UPDATE(state,delivery_batches,lease_token,lease_until,last_error,delivered_at) ON TABLE neon_auth.control_webhook_event TO "+pgQuote(login)); err != nil {
				return err
			}
		}
	}
	return nil
}
