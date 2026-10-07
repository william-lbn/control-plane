package control

import (
	"context"
	"crypto/hmac"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

const invitationColumns = `id,org_id,username,role,invited_by,created_at,expires_at,accepted_at,accepted_by,revoked_at,
    CASE WHEN accepted_at IS NOT NULL THEN 'accepted' WHEN revoked_at IS NOT NULL THEN 'revoked'
    WHEN expires_at<=now() THEN 'expired' ELSE 'pending' END AS state`

// No token in URLs, emails, audit bodies or operation payloads. Existing account
// acceptance requires that account's Console session; a token cannot reset it.
func (s *server) consoleInvitations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	org, actor := r.PathValue("org"), userFrom(r).ID
	if r.Method == http.MethodGet {
		rows, err := s.many(r.Context(), `SELECT `+invitationColumns+` FROM console_invitations WHERE org_id=$1 ORDER BY created_at DESC,id LIMIT 200`, org)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not list invitations")
			return
		}
		jsonResponse(w, 200, page(rows))
		return
	}
	var body struct {
		Username     string `json:"username"`
		Role         string `json:"role"`
		ExpiresHours int    `json:"expires_hours"`
	}
	if readJSON(r, &body) != nil || !accountName.MatchString(body.Username) || !organizationRole(body.Role) || body.ExpiresHours < 1 || body.ExpiresHours > 168 {
		fail(w, r, 422, "invalid_invitation", "Username, organization role and expiry of 1-168 hours required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	hash := requestHash("POST:/organizations/"+org+"/invitations", body)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	if err = lockInvitationAdmin(r.Context(), tx, org, actor); err != nil {
		fail(w, r, 403, "forbidden", "Current organization Admin required")
		return
	}
	var id, previous string
	err = tx.QueryRow(r.Context(), `SELECT id,request_hash FROM console_invitations WHERE invited_by=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&id, &previous)
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key used with different input")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not replay invitation")
			return
		}
		s.invitationResponse(w, r, id, "", 200)
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect invitation")
		return
	}
	var member bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM organization_members m JOIN users u ON u.id=m.user_id WHERE m.org_id=$1 AND u.username=$2)`, org, body.Username).Scan(&member)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect membership")
		return
	}
	if member {
		fail(w, r, 409, "already_member", "Account already belongs to this organization")
		return
	}
	// Expired history remains queryable; its reservation no longer blocks reissue.
	_, err = tx.Exec(r.Context(), `UPDATE console_invitations SET revoked_at=now() WHERE org_id=$1 AND username=$2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at<=now()`, org, body.Username)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not retire expired reservation")
		return
	}
	id = newID("inv_")
	token := "ncp_inv_" + randomToken(32)
	_, err = tx.Exec(r.Context(), `INSERT INTO console_invitations(id,org_id,username,role,invited_by,token_hash,key_hash,request_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,now()+$9*interval '1 hour')`, id, org, body.Username, body.Role, actor, digest(token), keyHash(key), hash, body.ExpiresHours)
	if err == nil {
		err = invitationAudit(r.Context(), tx, actor, "create_console_invitation", id, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		status, code, msg := conflictCode(err)
		fail(w, r, status, code, msg)
		return
	}
	s.invitationResponse(w, r, id, token, 201)
}

func (s *server) invitationResponse(w http.ResponseWriter, r *http.Request, id, token string, status int) {
	item, err := s.one(r.Context(), `SELECT `+invitationColumns+` FROM console_invitations WHERE id=$1`, id)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load invitation; retry with the same key")
		return
	}
	item["secret_available"] = token != ""
	if token != "" {
		item["token"] = token
	}
	jsonResponse(w, status, item)
}

func lockInvitationAdmin(ctx context.Context, tx pgx.Tx, org, actor string) error {
	var id, role string
	err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id=$1 AND state='active' FOR UPDATE`, org).Scan(&id)
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT m.role FROM organization_members m JOIN users u ON u.id=m.user_id AND NOT u.disabled WHERE m.org_id=$1 AND m.user_id=$2`, org, actor).Scan(&role)
	}
	if err != nil {
		return err
	}
	if roleLevel(role) < 3 {
		return errors.New("inviter is not admin")
	}
	return nil
}

func invitationAudit(ctx context.Context, tx pgx.Tx, actor, action, id, request string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,$2,'console_invitation',$3,'succeeded',$4)`, actor, action, id, request)
	return err
}

func (s *server) revokeConsoleInvitation(w http.ResponseWriter, r *http.Request) {
	org, id := r.PathValue("org"), r.PathValue("invitation")
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	if err = lockInvitationAdmin(r.Context(), tx, org, userFrom(r).ID); err != nil {
		fail(w, r, 403, "forbidden", "Current organization Admin required")
		return
	}
	var accepted bool
	err = tx.QueryRow(r.Context(), `SELECT accepted_at IS NOT NULL FROM console_invitations WHERE id=$1 AND org_id=$2 FOR UPDATE`, id, org).Scan(&accepted)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Invitation not found")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read invitation")
		return
	}
	if accepted {
		fail(w, r, 409, "invitation_already_accepted", "Remove the organization member to revoke access")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE console_invitations SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1`, id)
	if err == nil {
		err = invitationAudit(r.Context(), tx, userFrom(r).ID, "revoke_console_invitation", id, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not revoke invitation")
		return
	}
	jsonResponse(w, 200, record{"id": id, "revoked": true})
}

func validInvitationToken(token string) bool {
	if !strings.HasPrefix(token, "ncp_inv_") || len(token) != 72 {
		return false
	}
	for _, ch := range token[8:] {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// Reject browser cross-site POSTs, including signup without a Console session.
// Host comes from the trusted ingress's Host forwarding, never a client DSN.
func sameOriginRegistration(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
	}
	return true
}

func (s *server) signupAdmission(w http.ResponseWriter, r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	var count int
	err = s.db.QueryRow(r.Context(), `INSERT INTO console_registration_limits(scope_hash,window_at,attempts) VALUES($1,date_trunc('minute',now()),1)
        ON CONFLICT(scope_hash,window_at) DO UPDATE SET attempts=console_registration_limits.attempts+1 WHERE console_registration_limits.attempts<20 RETURNING attempts`, digest("console-signup:"+ip)).Scan(&count)
	if isNoRows(err) {
		w.Header().Set("Retry-After", "60")
		fail(w, r, 429, "registration_rate_limited", "Too many registration attempts; retry later")
		return false
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check registration policy")
		return false
	}
	return true
}

func (s *server) signup(w http.ResponseWriter, r *http.Request) {
	s.acceptConsoleInvitation(w, r, true)
}
func (s *server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	s.acceptConsoleInvitation(w, r, false)
}

func (s *server) acceptConsoleInvitation(w http.ResponseWriter, r *http.Request, newAccount bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOriginRegistration(r) {
		fail(w, r, 403, "origin_forbidden", "Same-origin registration required")
		return
	}
	var body struct {
		Token    string `json:"token"`
		Username string `json:"username,omitempty"`
		Password string `json:"password,omitempty"`
	}
	if readJSON(r, &body) != nil || !validInvitationToken(body.Token) || (newAccount && (!accountName.MatchString(body.Username) || len(body.Password) < 12 || len(body.Password) > 256)) || (!newAccount && (body.Password != "" || body.Username != "")) {
		fail(w, r, 422, "invalid_registration", "Invitation token and valid account input required")
		return
	}
	if newAccount && !s.signupAdmission(w, r) {
		return
	}
	// Find immutable organization identity, then use the same organization ->
	// invitation lock order as create/revoke/member mutations (no deadlock inversion).
	var org string
	err := s.db.QueryRow(r.Context(), `SELECT org_id FROM console_invitations WHERE token_hash=$1`, digest(body.Token)).Scan(&org)
	if isNoRows(err) {
		fail(w, r, 422, "invitation_unavailable", "Invitation is invalid or unavailable")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect invitation")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var orgState string
	err = tx.QueryRow(r.Context(), `SELECT state FROM organizations WHERE id=$1 FOR UPDATE`, org).Scan(&orgState)
	var id, name, role, inviter, stored string
	var eligible bool
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT id,username,role,invited_by,token_hash,(accepted_at IS NULL AND revoked_at IS NULL AND expires_at>now()) FROM console_invitations WHERE token_hash=$1 FOR UPDATE`, digest(body.Token)).Scan(&id, &name, &role, &inviter, &stored, &eligible)
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock invitation")
		return
	}
	var inviterRole string
	err = tx.QueryRow(r.Context(), `SELECT m.role FROM organization_members m JOIN users u ON u.id=m.user_id AND NOT u.disabled WHERE m.org_id=$1 AND m.user_id=$2`, org, inviter).Scan(&inviterRole)
	if !isNoRows(err) && err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect inviter")
		return
	}
	if orgState != "active" || !eligible || roleLevel(inviterRole) < 3 || !hmac.Equal([]byte(stored), []byte(digest(body.Token))) {
		fail(w, r, 422, "invitation_unavailable", "Invitation is invalid or unavailable")
		return
	}
	uid := userFrom(r).ID
	if newAccount {
		if body.Username != name {
			fail(w, r, 422, "invitation_unavailable", "Invitation is invalid or unavailable")
			return
		}
		var exists bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)`, name).Scan(&exists)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not inspect account")
			return
		}
		if exists {
			fail(w, r, 409, "sign_in_required", "Sign in as the invited account to accept; existing passwords are never changed")
			return
		}
		uid = newID("usr_")
		_, err = tx.Exec(r.Context(), `INSERT INTO users(id,username,password_hash,role) VALUES($1,$2,$3,'viewer')`, uid, name, hashPassword(body.Password))
	} else if userFrom(r).Username != name {
		fail(w, r, 403, "invited_account_required", "Sign in as the invited account")
		return
	} else {
		var active bool
		err = tx.QueryRow(r.Context(), `SELECT NOT disabled FROM users WHERE id=$1 AND username=$2 FOR UPDATE`, uid, name).Scan(&active)
		if err != nil || !active {
			fail(w, r, 403, "invited_account_required", "Active invited account required")
			return
		}
	}
	// Membership is created once, never silently promoted by a second invite.
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO organization_members(org_id,user_id,role) VALUES($1,$2,$3)`, org, uid, role)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE console_invitations SET accepted_at=now(),accepted_by=$2 WHERE id=$1`, id, uid)
	}
	action := "accept_console_invitation"
	if newAccount {
		action = "register_console_account"
	}
	if err == nil {
		err = invitationAudit(r.Context(), tx, uid, action, id, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		status, code, msg := conflictCode(err)
		fail(w, r, status, code, msg)
		return
	}
	jsonResponse(w, 201, record{"user_id": uid, "username": name, "organization_id": org, "role": role, "sign_in_required": newAccount})
}

// Used by the existing session cleanup controller; only admission counters expire.
func (s *server) cleanRegistrationLimits(ctx context.Context) {
	_, _ = s.db.Exec(ctx, `DELETE FROM console_registration_limits WHERE window_at < now()-interval '1 day'`)
}
