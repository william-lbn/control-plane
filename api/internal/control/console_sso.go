package control

import (
	"context"
	"crypto/hmac"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

type consoleSSOPolicy struct {
	ProviderID, Revision, Mode string
	Version                    int64
}

func (s *server) consoleSSOPolicy(ctx context.Context, org string) (consoleSSOPolicy, error) {
	p := consoleSSOPolicy{Mode: "disabled"}
	err := s.db.QueryRow(ctx, `SELECT provider_id,provider_revision,mode,version FROM console_sso_policies WHERE org_id=$1`, org).Scan(&p.ProviderID, &p.Revision, &p.Mode, &p.Version)
	if isNoRows(err) {
		err = nil
	}
	return p, err
}
func (s *server) consoleSSOProvider(org string, p consoleSSOPolicy) *consoleOIDCProvider {
	if s.consoleOIDC == nil || p.Mode == "disabled" {
		return nil
	}
	provider := s.consoleOIDC.byID[p.ProviderID]
	if provider == nil || provider.Organization != org || provider.revision != p.Revision {
		return nil
	}
	return provider
}
func ssoNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
func (s *server) ssoCookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/auth/sso", HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func (s *server) consoleSessionCookies(w http.ResponseWriter, token, csrf string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: "neon_v2_session", Value: token, Path: "/", HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteStrictMode, MaxAge: seconds})
	http.SetCookie(w, &http.Cookie{Name: "neon_v2_csrf", Value: csrf, Path: "/", Secure: s.secureCookies, SameSite: http.SameSiteStrictMode, MaxAge: seconds})
}

func (s *server) consoleSSOPublic(w http.ResponseWriter, r *http.Request) {
	ssoNoStore(w)
	org := r.URL.Query().Get("organization")
	if !aiProviderUserID.MatchString(org) {
		fail(w, r, 422, "invalid_organization", "Organization ID required")
		return
	}
	p, err := s.consoleSSOPolicy(r.Context(), org)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read SSO policy")
		return
	}
	provider := s.consoleSSOProvider(org, p)
	if provider == nil {
		jsonResponse(w, 200, record{"available": false})
		return
	}
	jsonResponse(w, 200, record{"available": true, "name": provider.Name, "organization_id": org, "required": p.Mode == "required"})
}

// The original authenticated session is recorded for linking. Its cookie may
// be Strict and absent on a cross-site callback; the separate single-use Lax
// browser cookie binds that callback to the browser which proved its password.
func (s *server) consoleSSOStart(w http.ResponseWriter, r *http.Request) {
	ssoNoStore(w)
	if s.consoleOIDC == nil || s.backendKeys == nil {
		fail(w, r, 503, "sso_unavailable", "Console SSO is not configured")
		return
	}
	if r.Header.Get("Origin") != s.consoleOIDC.PublicOrigin {
		fail(w, r, 403, "sso_origin", "Start SSO from the configured Console origin")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Organization string `json:"organization_id"`
		Password     string `json:"password"`
	}
	if readJSON(r, &input) != nil || len(input.Password) > 1024 {
		fail(w, r, 422, "invalid_request", "Organization ID required")
		return
	}
	org := r.PathValue("org")
	purpose := "login"
	var owner, session any
	if org != "" {
		purpose = "link"
	} else {
		org = input.Organization
	}
	if !aiProviderUserID.MatchString(org) {
		fail(w, r, 422, "invalid_organization", "Organization ID required")
		return
	}
	p, err := s.consoleSSOPolicy(r.Context(), org)
	provider := s.consoleSSOProvider(org, p)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read SSO policy")
		return
	}
	if provider == nil {
		fail(w, r, 409, "sso_unavailable", "SSO provider is unavailable")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not start SSO")
		return
	}
	defer tx.Rollback(r.Context())
	ip, _, splitErr := net.SplitHostPort(r.RemoteAddr)
	if splitErr != nil {
		ip = r.RemoteAddr
	}
	if !s.consoleSSOAttempt(w, r, tx, "sso-start:"+digest(ip), 20, true) {
		return
	}
	if purpose == "link" {
		u := userFrom(r)
		if u.APIKey {
			fail(w, r, 403, "console_session_required", "Console session required")
			return
		}
		if !s.consoleSSOPassword(w, r, tx, u, input.Password) {
			return
		}
		cookie, _ := r.Cookie("neon_v2_session")
		if cookie == nil {
			fail(w, r, 401, "unauthenticated", "Sign in again")
			return
		}
		owner = u.ID
		session = digest(cookie.Value)
	}
	state, browser, nonce, verifier := randomToken(32), randomToken(32), randomToken(32), oauth2.GenerateVerifier()
	version := s.backendKeys.Active
	sealed, err := s.sealSavedSQL(version, verifier, []byte("console-oidc-verifier-v1:"+digest(state)))
	if err != nil {
		fail(w, r, 503, "sso_key_unavailable", "Could not protect SSO state")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO console_sso_flows(state_hash,browser_hash,nonce_hash,key_version,sealed_verifier,org_id,provider_id,provider_revision,policy_version,purpose,user_id,session_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now()+interval '10 minutes')`, digest(state), digest(browser), digest(nonce), version, sealed, org, provider.ID, provider.revision, p.Version, purpose, owner, session)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "SSO start outcome unknown; start again")
		return
	}
	s.ssoCookie(w, "neon_v2_oidc", browser, 600)
	jsonResponse(w, 200, record{"authorization_url": provider.oauth(s.consoleOIDC).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce), oauth2.SetAuthURLParam("prompt", "login")), "expires_in": 600})
}

// Caller owns its transaction. Failure counters are committed even when the
// attempted action is rejected; they cannot be rolled back by a bad password.
func (s *server) consoleSSOAttempt(w http.ResponseWriter, r *http.Request, tx pgx.Tx, scope string, limit int, recordAttempt bool) bool {
	if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,771843))`, scope); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check SSO policy")
		return false
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM login_failures WHERE scope_key=$1 AND occurred_at>now()-interval '15 minutes'`, scope).Scan(&n); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check SSO policy")
		return false
	}
	if n >= limit {
		w.Header().Set("Retry-After", "900")
		fail(w, r, 429, "sso_rate_limited", "Retry later")
		return false
	}
	if recordAttempt {
		if _, err := tx.Exec(r.Context(), `INSERT INTO login_failures(scope_key) VALUES($1)`, scope); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not record SSO policy")
			return false
		}
	}
	return true
}
func (s *server) consoleSSOPassword(w http.ResponseWriter, r *http.Request, tx pgx.Tx, u user, password string) bool {
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console session required")
		return false
	}
	if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,771842))`, u.ID); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check account")
		return false
	}
	if !s.consoleSSOAttempt(w, r, tx, "sso-account:"+digest(u.ID), 10, false) {
		return false
	}
	cookie, e := r.Cookie("neon_v2_session")
	if e != nil {
		fail(w, r, 401, "unauthenticated", "Sign in again")
		return false
	}
	var stored string
	err := tx.QueryRow(r.Context(), `SELECT u.password_hash FROM users u JOIN sessions se ON se.user_id=u.id WHERE u.id=$1 AND NOT u.disabled AND se.token_hash=$2 AND se.revoked_at IS NULL AND se.expires_at>now() AND NOT EXISTS(SELECT 1 FROM console_mfa f WHERE f.user_id=u.id AND f.enabled_at IS NOT NULL AND (se.mfa_verified_at IS NULL OR se.mfa_verified_at<f.enabled_at)) FOR UPDATE OF u FOR SHARE OF se`, u.ID, digest(cookie.Value)).Scan(&stored)
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 401, "unauthenticated", "Sign in again")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not check account")
		}
		return false
	}
	if !verifyPassword(password, stored) {
		if _, err = tx.Exec(r.Context(), `INSERT INTO login_failures(scope_key) VALUES($1)`, "sso-account:"+digest(u.ID)); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not record account policy")
			return false
		}
		if tx.Commit(r.Context()) != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not record account policy")
		} else {
			fail(w, r, 401, "invalid_current_password", "Current password is incorrect")
		}
		return false
	}
	return true
}

func (s *server) consoleSSOCallback(w http.ResponseWriter, r *http.Request) {
	ssoNoStore(w)
	q := r.URL.Query()
	state := q.Get("state")
	cookie, err := r.Cookie("neon_v2_oidc")
	if s.consoleOIDC == nil || err != nil || len(state) != 64 || len(q["state"]) != 1 || len(cookie.Value) != 64 || len(q["code"]) > 1 || len(q["error"]) > 1 {
		fail(w, r, 400, "invalid_sso_state", "Start SSO again")
		return
	}
	var org, providerID, revision, purpose, nonce, version string
	var policyVersion int64
	var sealed []byte
	var owner, session *string
	// Consumed before the exchange. A token endpoint failure never causes an
	// authorization code to be silently replayed by this service.
	err = s.db.QueryRow(r.Context(), `UPDATE console_sso_flows SET used_at=now() WHERE state_hash=$1 AND browser_hash=$2 AND used_at IS NULL AND expires_at>now() RETURNING org_id,provider_id,provider_revision,policy_version,purpose,nonce_hash,key_version,sealed_verifier,user_id,session_hash`, digest(state), digest(cookie.Value)).Scan(&org, &providerID, &revision, &policyVersion, &purpose, &nonce, &version, &sealed, &owner, &session)
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 400, "invalid_sso_state", "State expired or already used")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not consume SSO state")
		}
		return
	}
	s.ssoCookie(w, "neon_v2_oidc", "", -1)
	if q.Get("error") != "" || q.Get("code") == "" || len(q.Get("code")) > 4096 {
		fail(w, r, 400, "sso_denied", "Identity provider did not complete sign in")
		return
	}
	p, err := s.consoleSSOPolicy(r.Context(), org)
	provider := s.consoleSSOProvider(org, p)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check SSO policy")
		return
	}
	if provider == nil || p.Version != policyVersion || providerID != p.ProviderID || revision != p.Revision {
		fail(w, r, 409, "sso_policy_changed", "SSO policy changed; start again")
		return
	}
	verifier, err := s.openSavedSQL(version, sealed, []byte("console-oidc-verifier-v1:"+digest(state)))
	if err != nil {
		fail(w, r, 503, "sso_key_unavailable", "Could not open SSO state")
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), provider.client), 15*time.Second)
	defer cancel()
	token, err := provider.oauth(s.consoleOIDC).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		fail(w, r, 401, "sso_exchange_failed", "Identity provider exchange failed; start again")
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || len(raw) > 64<<10 {
		fail(w, r, 401, "invalid_id_token", "Identity token required")
		return
	}
	id, err := provider.verifier.Verify(ctx, raw)
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if err != nil || id.Issuer != provider.Issuer || id.Subject == "" || len(id.Subject) > 255 || !hmac.Equal([]byte(digest(id.Nonce)), []byte(nonce)) || id.IssuedAt.IsZero() || id.IssuedAt.After(time.Now().Add(time.Minute)) || id.Claims(&claims) != nil || len(id.Audience) > 1 && claims.AuthorizedParty != provider.ClientID || claims.AuthorizedParty != "" && claims.AuthorizedParty != provider.ClientID {
		fail(w, r, 401, "invalid_id_token", "Identity token validation failed")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not accept identity")
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize policy changes and membership removal with the final decision.
	var currentVersion int64
	err = tx.QueryRow(r.Context(), `SELECT version FROM console_sso_policies WHERE org_id=$1 AND provider_id=$2 AND provider_revision=$3 AND mode<>'disabled' FOR SHARE`, org, provider.ID, provider.revision).Scan(&currentVersion)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not check SSO policy")
		return
	}
	if err != nil || currentVersion != policyVersion {
		fail(w, r, 409, "sso_policy_changed", "SSO policy changed; start again")
		return
	}
	if purpose == "link" {
		if owner == nil || session == nil {
			fail(w, r, 401, "invalid_sso_state", "Link session required")
			return
		}
		if !s.consoleSSOLinkIdentity(w, r, tx, org, provider, id.Subject, *owner, *session) {
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Link outcome unknown; inspect account bindings")
			return
		}
		http.Redirect(w, r, s.consoleOIDC.PublicOrigin+"/#/organization", http.StatusSeeOther)
		return
	}
	var binding, userID string
	var disabled bool
	err = tx.QueryRow(r.Context(), `SELECT b.id,b.user_id,u.disabled FROM console_sso_bindings b JOIN users u ON u.id=b.user_id JOIN organization_members m ON m.user_id=b.user_id AND m.org_id=b.org_id JOIN organizations o ON o.id=b.org_id AND o.state='active' WHERE b.org_id=$1 AND b.provider_revision=$2 AND b.subject=$3 AND b.revoked_at IS NULL FOR SHARE OF b,m,o`, org, provider.revision, id.Subject).Scan(&binding, &userID, &disabled)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not check identity binding")
		return
	}
	if err != nil || disabled {
		fail(w, r, 401, "sso_account_unlinked", "Sign in locally and explicitly link this identity")
		return
	}
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,771842))`, userID); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check MFA")
		return
	}
	var mfa bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM console_mfa WHERE user_id=$1 AND enabled_at IS NOT NULL)`, userID).Scan(&mfa)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check MFA")
		return
	}
	if mfa {
		challenge := randomToken(32)
		_, err = tx.Exec(r.Context(), `INSERT INTO console_sso_challenges(challenge_hash,binding_id,policy_version,expires_at,token_expires_at) VALUES($1,$2,$3,LEAST(now()+interval '5 minutes',$4),$4)`, digest(challenge), binding, policyVersion, id.Expiry)
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not create MFA challenge")
			return
		}
		s.ssoCookie(w, "neon_v2_sso_mfa", challenge, 300)
		http.Redirect(w, r, s.consoleOIDC.PublicOrigin+"/#/sso-mfa", http.StatusSeeOther)
		return
	}
	if !s.consoleSSOIssue(w, r, tx, binding, userID, policyVersion, id.Expiry, nil) {
		return
	}
	http.Redirect(w, r, s.consoleOIDC.PublicOrigin+"/#/organization", http.StatusSeeOther)
}

func (s *server) consoleSSOLinkIdentity(w http.ResponseWriter, r *http.Request, tx pgx.Tx, org string, p *consoleOIDCProvider, subject, owner, session string) bool {
	if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,771842))`, owner); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check link")
		return false
	}
	var valid bool
	err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users u JOIN sessions s ON s.user_id=u.id JOIN organization_members m ON m.user_id=u.id AND m.org_id=$3 JOIN organizations o ON o.id=m.org_id AND o.state='active' WHERE u.id=$1 AND NOT u.disabled AND s.token_hash=$2 AND s.revoked_at IS NULL AND s.expires_at>now() AND NOT EXISTS(SELECT 1 FROM console_mfa f WHERE f.user_id=u.id AND f.enabled_at IS NOT NULL AND (s.mfa_verified_at IS NULL OR s.mfa_verified_at<f.enabled_at)))`, owner, session, org).Scan(&valid)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check link")
		return false
	}
	if !valid {
		fail(w, r, 401, "link_session_expired", "Original authenticated session is no longer valid")
		return false
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO console_sso_bindings(id,org_id,provider_id,provider_revision,subject,user_id) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(provider_revision,subject) DO UPDATE SET revoked_at=NULL WHERE console_sso_bindings.user_id=EXCLUDED.user_id AND console_sso_bindings.org_id=EXCLUDED.org_id`, newID("ssob_"), org, p.ID, p.revision, subject, owner)
	if err != nil {
		fail(w, r, 409, "identity_already_linked", "Identity or account already has another binding")
		return false
	}
	var linked bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM console_sso_bindings WHERE org_id=$1 AND provider_revision=$2 AND subject=$3 AND user_id=$4 AND revoked_at IS NULL)`, org, p.revision, subject, owner).Scan(&linked)
	if err != nil || !linked {
		fail(w, r, 409, "identity_already_linked", "Identity belongs to a different account")
		return false
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'console_sso_link','organization',$2,'succeeded',$3)`, owner, org, requestID(r))
	if err != nil {
		fail(w, r, 503, "audit_unavailable", "Identity link was not committed")
		return false
	}
	return true
}

func (s *server) consoleSSOIssue(w http.ResponseWriter, r *http.Request, tx pgx.Tx, binding, owner string, policy int64, expires time.Time, mfa any) bool {
	expires = expires.UTC()
	if max := time.Now().UTC().Add(8 * time.Hour); expires.After(max) {
		expires = max
	}
	if !expires.After(time.Now().Add(time.Second)) {
		fail(w, r, 401, "identity_expired", "Start SSO again")
		return false
	}
	token, csrf := randomToken(48), randomToken(32)
	_, err := tx.Exec(r.Context(), `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at,mfa_verified_at,sso_binding_id,sso_policy_version) VALUES($1,$2,$3,$4,$5,$6,$7)`, digest(token), owner, digest(csrf), expires, mfa, binding, policy)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'console_sso_login','account_sso',$2,'succeeded',$3)`, owner, binding, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "SSO session outcome unknown; start again")
		return false
	}
	s.consoleSessionCookies(w, token, csrf, int(time.Until(expires).Seconds()))
	return true
}

func (s *server) consoleSSOMFA(w http.ResponseWriter, r *http.Request) {
	ssoNoStore(w)
	cookie, err := r.Cookie("neon_v2_sso_mfa")
	if err != nil || len(cookie.Value) != 64 || s.consoleOIDC == nil || r.Header.Get("Origin") != s.consoleOIDC.PublicOrigin {
		fail(w, r, 401, "invalid_sso_challenge", "Start SSO again")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Code     string `json:"code"`
		Recovery bool   `json:"recovery"`
	}
	if readJSON(r, &input) != nil || len(input.Code) > 64 {
		fail(w, r, 422, "invalid_mfa_proof", "MFA proof required")
		return
	}
	var binding, owner, org, providerID, revision string
	var policy int64
	var expiry time.Time
	err = s.db.QueryRow(r.Context(), `SELECT b.id,b.user_id,b.org_id,b.provider_id,b.provider_revision,c.policy_version,c.token_expires_at FROM console_sso_challenges c JOIN console_sso_bindings b ON b.id=c.binding_id WHERE c.challenge_hash=$1 AND c.used_at IS NULL AND c.expires_at>now() AND c.attempts<5 AND b.revoked_at IS NULL`, digest(cookie.Value)).Scan(&binding, &owner, &org, &providerID, &revision, &policy, &expiry)
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 401, "invalid_sso_challenge", "Start SSO again")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read MFA challenge")
		}
		return
	}
	p, e := s.consoleSSOPolicy(r.Context(), org)
	provider := s.consoleSSOProvider(org, p)
	if e != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check SSO policy")
		return
	}
	if provider == nil || providerID != p.ProviderID || revision != p.Revision || policy != p.Version {
		fail(w, r, 401, "sso_policy_changed", "Start SSO again")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not verify MFA")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,771842))`, owner); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock account")
		return
	}
	var valid bool
	err = tx.QueryRow(r.Context(), `SELECT NOT u.disabled FROM users u JOIN organization_members m ON m.user_id=u.id AND m.org_id=$2 JOIN organizations o ON o.id=m.org_id AND o.state='active' JOIN console_sso_policies p ON p.org_id=o.id AND p.version=$3 AND p.provider_revision=$4 AND p.mode<>'disabled' JOIN console_sso_bindings b ON b.id=$5 AND b.user_id=u.id AND b.org_id=o.id AND b.revoked_at IS NULL WHERE u.id=$1 FOR SHARE OF u,m,o,p,b`, owner, org, policy, revision, binding).Scan(&valid)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not check account policy")
		return
	}
	if err != nil || !valid {
		fail(w, r, 401, "invalid_sso_challenge", "Account or membership changed")
		return
	}
	var exists bool
	err = tx.QueryRow(r.Context(), `UPDATE console_sso_challenges SET attempts=attempts+1 WHERE challenge_hash=$1 AND used_at IS NULL AND expires_at>now() AND attempts<5 RETURNING true`, digest(cookie.Value)).Scan(&exists)
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 401, "invalid_sso_challenge", "Start SSO again")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not consume MFA challenge")
		}
		return
	}
	if !s.consoleSSOAttempt(w, r, tx, "sso-mfa:"+digest(owner), 20, true) {
		return
	}
	valid, err = s.consoleMFAProof(r, tx, owner, input.Code, input.Recovery)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "mfa_key_unavailable", "Could not verify MFA")
		return
	}
	if !valid {
		if tx.Commit(r.Context()) != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not record MFA attempt")
		} else {
			fail(w, r, 401, "invalid_mfa_proof", "MFA code is incorrect, expired or already used")
		}
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE console_sso_challenges SET used_at=now() WHERE challenge_hash=$1`, digest(cookie.Value)); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not consume challenge")
		return
	}
	if !s.consoleSSOIssue(w, r, tx, binding, owner, policy, expiry, time.Now().UTC()) {
		return
	}
	s.ssoCookie(w, "neon_v2_sso_mfa", "", -1)
	jsonResponse(w, 200, record{"ok": true})
}

func ssoPolicyVersion(r *http.Request) (int64, bool) {
	tag := r.Header.Get("If-Match")
	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return 0, false
	}
	n, e := strconv.ParseInt(strings.Trim(tag, "\""), 10, 64)
	return n, e == nil && n >= 0 && n < 9223372036854775807
}

// Console-only organization policy; scoped machine credentials retain their
// existing independent authentication and current membership checks.
func (s *server) consoleSSOAuthorize(w http.ResponseWriter, r *http.Request, u user, org string) bool {
	if u.APIKey || org == "" || strings.HasSuffix(r.URL.Path, "/sso") || strings.HasSuffix(r.URL.Path, "/sso/link") || strings.Contains(r.URL.Path, "/sso/bindings") {
		return true
	}
	p, err := s.consoleSSOPolicy(r.Context(), org)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check Console SSO policy")
		return false
	}
	if p.Mode == "required" && (u.SSOOrganization != org || u.SSOProvider != p.ProviderID || u.SSORevision != p.Revision || u.SSOPolicyVersion != p.Version) {
		fail(w, r, 403, "organization_sso_required", "Sign in with this organization's SSO provider")
		return false
	}
	return true
}
