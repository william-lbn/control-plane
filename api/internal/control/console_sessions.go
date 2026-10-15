package control

import (
	"net/http"
	"regexp"
	"time"
)

var consoleSessionID = regexp.MustCompile(`^cs_[a-f0-9]{32}$`)

func (s *server) consoleSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := userFrom(r)
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console login required")
		return
	}
	cookie, err := r.Cookie("neon_v2_session")
	if err != nil {
		fail(w, r, 401, "unauthenticated", "Please sign in")
		return
	}
	items, err := s.many(r.Context(), `SELECT id,created_at,expires_at,(token_hash=$2) AS current FROM sessions WHERE user_id=$1 AND revoked_at IS NULL AND expires_at>now() ORDER BY (token_hash=$2) DESC,created_at DESC,id DESC LIMIT 51`, u.ID, digest(cookie.Value))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read account sessions")
		return
	}
	more := len(items) > 50
	if more {
		items = items[:50]
	}
	jsonResponse(w, 200, record{"items": items, "limit": 50, "has_more": more})
}

// Revocation and audit are one transaction. A fresh password check applies to
// every account mutation; API keys and other users' sessions are never accepted.
// Expire the token as well so older API readers cannot revive it after rollback.
func (s *server) revokeConsoleSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := userFrom(r)
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console login required")
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if readJSON(r, &input) != nil || input.Password == "" || len(input.Password) > 1024 {
		fail(w, r, 422, "invalid_request", "Current account password required")
		return
	}
	id := r.PathValue("session")
	if id != "" && !consoleSessionID.MatchString(id) {
		fail(w, r, 404, "not_found", "Session not found")
		return
	}
	cookie, err := r.Cookie("neon_v2_session")
	if err != nil {
		fail(w, r, 401, "unauthenticated", "Please sign in")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not revoke sessions")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,771842))`, u.ID); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check account policy")
		return
	}
	var password, currentID string
	err = tx.QueryRow(r.Context(), `SELECT u.password_hash,s.id FROM users u JOIN sessions s ON s.user_id=u.id WHERE u.id=$1 AND NOT u.disabled AND s.token_hash=$2 AND s.revoked_at IS NULL AND s.expires_at>now() FOR UPDATE OF s`, u.ID, digest(cookie.Value)).Scan(&password, &currentID)
	if err != nil {
		fail(w, r, 401, "unauthenticated", "Please sign in again")
		return
	}
	scope := "account-session:" + digest(u.ID)
	var failures int
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM login_failures WHERE scope_key=$1 AND occurred_at>now()-interval '15 minutes'`, scope).Scan(&failures); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check account policy")
		return
	}
	if failures >= 10 {
		w.Header().Set("Retry-After", "900")
		fail(w, r, 429, "account_rate_limited", "Retry later")
		return
	}
	if !verifyPassword(input.Password, password) {
		if _, err = tx.Exec(r.Context(), `INSERT INTO login_failures(scope_key) VALUES($1)`, scope); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not record account policy")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not record account policy")
			return
		}
		fail(w, r, 401, "invalid_current_password", "Current password is incorrect")
		return
	}
	if id == currentID {
		fail(w, r, 409, "current_session", "Use logout to close this current session")
		return
	}
	var changed int64
	var revoked *time.Time
	if id != "" {
		err = tx.QueryRow(r.Context(), `SELECT revoked_at FROM sessions WHERE id=$1 AND user_id=$2`, id, u.ID).Scan(&revoked)
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Session not found")
			return
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not inspect session")
			return
		}
		if revoked == nil {
			err = tx.QueryRow(r.Context(), `UPDATE sessions SET revoked_at=now(),expires_at=LEAST(expires_at,now()) WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL RETURNING revoked_at`, id, u.ID).Scan(&revoked)
			if err != nil {
				fail(w, r, 503, "metadata_unavailable", "Could not revoke session")
				return
			}
			changed = 1
		}
	} else {
		command, e := tx.Exec(r.Context(), `UPDATE sessions SET revoked_at=now(),expires_at=LEAST(expires_at,now()) WHERE user_id=$1 AND id<>$2 AND revoked_at IS NULL AND expires_at>now()`, u.ID, currentID)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not revoke other sessions")
			return
		}
		changed = command.RowsAffected()
	}
	if changed > 0 {
		resource := id
		if resource == "" {
			resource = u.ID
		}
		if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'revoke_console_session','account_session',$2,'succeeded',$3)`, u.ID, resource, requestID(r)); err != nil {
			fail(w, r, 503, "audit_unavailable", "Revocation not committed; audit unavailable")
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Revocation outcome unknown; inspect session list")
		return
	}
	jsonResponse(w, 200, record{"revoked_count": changed, "session_id": id, "revoked_at": revoked, "current_session_retained": true})
}
