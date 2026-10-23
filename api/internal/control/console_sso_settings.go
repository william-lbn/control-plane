package control

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func (s *server) consoleSSOView(w http.ResponseWriter, r *http.Request, org string) {
	u := userFrom(r)
	p, err := s.consoleSSOPolicy(r.Context(), org)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read SSO policy")
		return
	}
	providers := []record{}
	if s.consoleOIDC != nil {
		for _, provider := range s.consoleOIDC.Providers {
			if provider.Organization == org {
				providers = append(providers, record{"id": provider.ID, "name": provider.Name})
			}
		}
	}
	bindings, err := s.many(r.Context(), `SELECT id,provider_id,subject,revoked_at,created_at FROM console_sso_bindings WHERE org_id=$1 AND user_id=$2 ORDER BY created_at,id LIMIT 50`, org, u.ID)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read account bindings")
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(p.Version, 10)+`"`)
	jsonResponse(w, 200, record{"provider_id": p.ProviderID, "mode": p.Mode, "version": p.Version, "providers": providers, "bindings": bindings, "can_manage": accessFrom(r).Level >= 3, "provider_available": s.consoleSSOProvider(org, p) != nil, "current_sso": u.SSOOrganization == org && u.SSORevision == p.Revision, "machine_credentials_independent": true})
}
func (s *server) consoleSSOSettings(w http.ResponseWriter, r *http.Request) {
	ssoNoStore(w)
	u := userFrom(r)
	org := r.PathValue("org")
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console session required")
		return
	}
	if r.Method == http.MethodGet {
		s.consoleSSOView(w, r, org)
		return
	}
	if accessFrom(r).Level < 3 {
		fail(w, r, 403, "forbidden", "Organization administrator required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Provider string `json:"provider_id"`
		Mode     string `json:"mode"`
		Password string `json:"password"`
	}
	if readJSON(r, &input) != nil || (input.Mode != "optional" && input.Mode != "required" && input.Mode != "disabled") || len(input.Password) > 1024 {
		fail(w, r, 422, "invalid_sso_policy", "Provider, policy mode and current password required")
		return
	}
	if s.consoleOIDC == nil {
		fail(w, r, 503, "sso_unavailable", "Operator must configure an OIDC provider")
		return
	}
	provider := s.consoleOIDC.byID[input.Provider]
	if provider == nil || provider.Organization != org {
		fail(w, r, 422, "invalid_sso_provider", "Choose an approved organization provider")
		return
	}
	version, ok := ssoPolicyVersion(r)
	if !ok {
		fail(w, r, 428, "precondition_required", "Current quoted If-Match version required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, "\r\n") || len(s.idempotencyKey) < 32 {
		fail(w, r, 422, "invalid_idempotency_key", "Durable Idempotency-Key required")
		return
	}
	keyHash := s.createRequestHash("console-sso-policy-key", key)
	requestHash := s.createRequestHash("console-sso-policy-request", record{"provider": provider.ID, "revision": provider.revision, "mode": input.Mode, "version": version})
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not update SSO policy")
		return
	}
	defer tx.Rollback(r.Context())
	if !s.consoleSSOPassword(w, r, tx, u, input.Password) {
		return
	}
	var admin bool
	err = tx.QueryRow(r.Context(), `SELECT m.role IN ('owner','admin') FROM organization_members m JOIN organizations o ON o.id=m.org_id AND o.state='active' WHERE m.org_id=$1 AND m.user_id=$2 FOR UPDATE OF o FOR SHARE OF m`, org, u.ID).Scan(&admin)
	if err != nil || !admin {
		fail(w, r, 403, "forbidden", "Current organization administrator required")
		return
	}
	var oldHash string
	var oldResult []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,result FROM console_sso_policy_receipts WHERE org_id=$1 AND user_id=$2 AND key_hash=$3`, org, u.ID, keyHash).Scan(&oldHash, &oldResult)
	if err == nil {
		if oldHash != requestHash {
			fail(w, r, 409, "idempotency_mismatch", "Original key belongs to a different policy request")
			return
		}
		if tx.Commit(r.Context()) != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not record account proof")
			return
		}
		w.Header().Set("Idempotent-Replay", "true")
		w.Header().Set("Content-Type", "application/json")
		w.Write(oldResult)
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read policy receipt")
		return
	}
	current := consoleSSOPolicy{Mode: "disabled"}
	err = tx.QueryRow(r.Context(), `SELECT provider_id,provider_revision,mode,version FROM console_sso_policies WHERE org_id=$1 FOR UPDATE`, org).Scan(&current.ProviderID, &current.Revision, &current.Mode, &current.Version)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read SSO policy")
		return
	}
	if current.Version != version {
		fail(w, r, 412, "sso_policy_changed", "Reload and review current SSO policy")
		return
	}
	if current.Mode == "required" && (u.SSOOrganization != org || u.SSORevision != current.Revision || u.SSOPolicyVersion != current.Version) {
		fail(w, r, 403, "organization_sso_required", "An existing organization SSO session is required to change this policy")
		return
	}
	if input.Mode == "required" && (u.SSOOrganization != org || u.SSORevision != provider.revision || u.SSOPolicyVersion != current.Version) {
		fail(w, r, 409, "sso_login_required", "Link your identity and sign in with the selected provider before requiring SSO")
		return
	}
	var next int64
	err = tx.QueryRow(r.Context(), `INSERT INTO console_sso_policies(org_id,provider_id,provider_revision,mode) VALUES($1,$2,$3,$4) ON CONFLICT(org_id) DO UPDATE SET provider_id=EXCLUDED.provider_id,provider_revision=EXCLUDED.provider_revision,mode=EXCLUDED.mode,version=console_sso_policies.version+1,updated_at=now() RETURNING version`, org, provider.ID, provider.revision, input.Mode).Scan(&next)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not update SSO policy")
		return
	}
	// Preserve only the proven initiating SSO session across the policy CAS.
	// Other old-generation SSO sessions fail their next request immediately.
	if u.SSOOrganization == org && u.SSORevision == provider.revision && input.Mode != "disabled" {
		cookie, _ := r.Cookie("neon_v2_session")
		if cookie == nil {
			fail(w, r, 401, "unauthenticated", "Sign in again")
			return
		}
		_, err = tx.Exec(r.Context(), `UPDATE sessions SET sso_policy_version=$1 WHERE token_hash=$2 AND user_id=$3 AND revoked_at IS NULL AND expires_at>now()`, next, digest(cookie.Value), u.ID)
	}
	result := record{"provider_id": provider.ID, "mode": input.Mode, "version": next, "policy_updated": true}
	raw, _ := json.Marshal(result)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO console_sso_policy_receipts(org_id,user_id,key_hash,request_hash,result) VALUES($1,$2,$3,$4,$5)`, org, u.ID, keyHash, requestHash, raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'console_sso_policy','organization',$2,'succeeded',$3)`, u.ID, org, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Policy outcome unknown; retry the original request")
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(next, 10)+`"`)
	jsonResponse(w, 200, result)
}

func (s *server) consoleSSOUnlink(w http.ResponseWriter, r *http.Request) {
	ssoNoStore(w)
	u := userFrom(r)
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console session required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Password string `json:"password"`
	}
	if readJSON(r, &input) != nil || len(input.Password) > 1024 {
		fail(w, r, 422, "invalid_request", "Current password required")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not unlink identity")
		return
	}
	defer tx.Rollback(r.Context())
	if !s.consoleSSOPassword(w, r, tx, u, input.Password) {
		return
	}
	command, err := tx.Exec(r.Context(), `UPDATE console_sso_bindings SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1 AND user_id=$2 AND org_id=$3`, r.PathValue("binding"), u.ID, r.PathValue("org"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not unlink identity")
		return
	}
	if command.RowsAffected() == 0 {
		fail(w, r, 404, "not_found", "Account binding not found")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE sessions SET revoked_at=COALESCE(revoked_at,now()),expires_at=LEAST(expires_at,now()) WHERE sso_binding_id=$1`, r.PathValue("binding"))
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE console_sso_challenges SET used_at=COALESCE(used_at,now()) WHERE binding_id=$1`, r.PathValue("binding"))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'console_sso_unlink','account_sso',$2,'succeeded',$3)`, u.ID, r.PathValue("binding"), requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Unlink outcome unknown; inspect bindings")
		return
	}
	jsonResponse(w, 200, record{"revoked": true})
}
