package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/pquerna/otp/totp"
)

func consoleMFAAAD(owner string) []byte {
	raw, _ := json.Marshal([]string{"console-totp-v1", owner})
	return raw
}

// Match the RFC 6238 counter explicitly so a successful code can be consumed
// atomically across API instances. The clock window is 30 seconds either side.
func consoleMFAStep(secret, code string, at time.Time, last int64) (int64, bool) {
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		return 0, false
	}
	current := at.Unix() / 30
	for _, step := range []int64{current, current - 1, current + 1} {
		if step < 0 || step <= last {
			continue
		}
		valid, err := hotp.ValidateCustom(code, uint64(step), secret, hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err == nil && valid {
			return step, true
		}
	}
	return 0, false
}

// Caller holds the user's row lock. Recovery hashes are account-bound and each
// successful proof is consumed in the same transaction as the resulting action.
func (s *server) consoleMFAProof(r *http.Request, tx pgx.Tx, owner, code string, recovery bool) (bool, error) {
	var version string
	var sealed, raw []byte
	var last int64
	err := tx.QueryRow(r.Context(), `SELECT key_version,sealed_secret,last_step,recovery_hashes FROM console_mfa WHERE user_id=$1 AND enabled_at IS NOT NULL FOR UPDATE`, owner).Scan(&version, &sealed, &last, &raw)
	if err != nil {
		return false, err
	}
	var hashes []string
	if json.Unmarshal(raw, &hashes) != nil {
		return false, errors.New("invalid factor state")
	}
	if recovery {
		if len(code) != 32 {
			return false, nil
		}
		hash := digest("console-mfa-recovery-v1:" + owner + ":" + code)
		for i, stored := range hashes {
			if hash != stored {
				continue
			}
			hashes = append(hashes[:i], hashes[i+1:]...)
			raw, _ = json.Marshal(hashes)
			_, err = tx.Exec(r.Context(), `UPDATE console_mfa SET recovery_hashes=$2 WHERE user_id=$1`, owner, raw)
			return err == nil, err
		}
		return false, nil
	}
	secret, err := s.openSavedSQL(version, sealed, consoleMFAAAD(owner))
	if err != nil {
		return false, err
	}
	step, valid := consoleMFAStep(secret, code, time.Now(), last)
	if !valid {
		return false, nil
	}
	_, err = tx.Exec(r.Context(), `UPDATE console_mfa SET last_step=$2 WHERE user_id=$1`, owner, step)
	return err == nil, err
}

func (s *server) consoleMFA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	u := userFrom(r)
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console login required")
		return
	}
	if r.Method == http.MethodGet {
		var enabled bool
		var pending *time.Time
		var remaining int
		err := s.db.QueryRow(r.Context(), `SELECT enabled_at IS NOT NULL,pending_until,jsonb_array_length(recovery_hashes) FROM console_mfa WHERE user_id=$1`, u.ID).Scan(&enabled, &pending, &remaining)
		if err != nil && !isNoRows(err) {
			fail(w, r, 503, "metadata_unavailable", "Could not read MFA policy")
			return
		}
		jsonResponse(w, 200, record{"enabled": enabled, "pending_until": pending, "recovery_codes_remaining": remaining, "enrollment_available": s.backendKeys != nil})
		return
	}
	if s.backendKeys == nil || s.backendKeys.decoded[s.backendKeys.Active] == nil {
		fail(w, r, 503, "mfa_key_unavailable", "Console MFA encryption key unavailable")
		return
	}
	var input struct {
		Password string `json:"password"`
		Code     string `json:"code"`
		Recovery bool   `json:"recovery"`
	}
	if readJSON(r, &input) != nil || input.Password == "" || len(input.Password) > 1024 || len(input.Code) > 64 {
		fail(w, r, 422, "invalid_request", "Current password and a bounded MFA proof required")
		return
	}
	cookie, err := r.Cookie("neon_v2_session")
	if err != nil {
		fail(w, r, 401, "unauthenticated", "Please sign in")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not update MFA")
		return
	}
	defer tx.Rollback(r.Context())
	// Match the account-session lock order and serialize enrollment/login.
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,771842))`, u.ID); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock account")
		return
	}
	var password string
	var activeSession bool
	err = tx.QueryRow(r.Context(), `SELECT password_hash,EXISTS(SELECT 1 FROM sessions WHERE user_id=$1 AND token_hash=$2 AND revoked_at IS NULL AND expires_at>now()) FROM users WHERE id=$1 AND NOT disabled FOR UPDATE`, u.ID, digest(cookie.Value)).Scan(&password, &activeSession)
	if err != nil || !activeSession {
		fail(w, r, 401, "unauthenticated", "Please sign in again")
		return
	}
	scope := "console-mfa:" + digest(u.ID)
	var failures int
	if tx.QueryRow(r.Context(), `SELECT count(*) FROM login_failures WHERE scope_key=$1 AND occurred_at>now()-interval '15 minutes'`, scope).Scan(&failures) != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check account policy")
		return
	}
	if failures >= 10 {
		w.Header().Set("Retry-After", "900")
		fail(w, r, 429, "mfa_rate_limited", "Retry later")
		return
	}
	reject := func() {
		if _, err = tx.Exec(r.Context(), `INSERT INTO login_failures(scope_key) VALUES($1)`, scope); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not record account policy")
			return
		}
		fail(w, r, 401, "invalid_mfa_proof", "Password or MFA proof is incorrect, expired or already used")
	}
	if !verifyPassword(input.Password, password) {
		reject()
		return
	}
	var enabled bool
	err = tx.QueryRow(r.Context(), `SELECT enabled_at IS NOT NULL FROM console_mfa WHERE user_id=$1`, u.ID).Scan(&enabled)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read factor")
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/api/v1/account/mfa/")
	output := record{}
	switch action {
	case "enroll":
		if enabled {
			fail(w, r, 409, "mfa_already_enabled", "Disable the existing factor before replacing it")
			return
		}
		key, e := totp.Generate(totp.GenerateOpts{Issuer: "Neon Console", AccountName: u.Username, SecretSize: 20, Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if e != nil {
			fail(w, r, 503, "mfa_key_unavailable", "Could not create factor")
			return
		}
		version := s.backendKeys.Active
		sealed, e := s.sealSavedSQL(version, key.Secret(), consoleMFAAAD(u.ID))
		if e != nil {
			fail(w, r, 503, "mfa_key_unavailable", "Could not encrypt factor")
			return
		}
		expires := time.Now().UTC().Add(10 * time.Minute)
		_, err = tx.Exec(r.Context(), `INSERT INTO console_mfa(user_id,key_version,sealed_secret,pending_until) VALUES($1,$2,$3,$4) ON CONFLICT(user_id) DO UPDATE SET key_version=$2,sealed_secret=$3,pending_until=$4,last_step=-1,recovery_hashes='[]'`, u.ID, version, sealed, expires)
		output = record{"secret": key.Secret(), "otpauth_uri": key.URL(), "pending_until": expires}
	case "confirm":
		if enabled {
			fail(w, r, 409, "mfa_already_enabled", "MFA already enabled; recovery codes cannot be replayed")
			return
		}
		var version string
		var sealed []byte
		var expires time.Time
		if tx.QueryRow(r.Context(), `SELECT key_version,sealed_secret,pending_until FROM console_mfa WHERE user_id=$1 AND enabled_at IS NULL FOR UPDATE`, u.ID).Scan(&version, &sealed, &expires) != nil || !expires.After(time.Now()) {
			fail(w, r, 409, "mfa_enrollment_expired", "Start a new enrollment")
			return
		}
		secret, e := s.openSavedSQL(version, sealed, consoleMFAAAD(u.ID))
		if e != nil {
			fail(w, r, 503, "mfa_key_unavailable", "Could not decrypt factor")
			return
		}
		step, valid := consoleMFAStep(secret, input.Code, time.Now(), -1)
		if !valid || input.Recovery {
			reject()
			return
		}
		codes, hashes := make([]string, 10), make([]string, 10)
		for i := range codes {
			codes[i] = randomToken(16)
			hashes[i] = digest("console-mfa-recovery-v1:" + u.ID + ":" + codes[i])
		}
		raw, _ := json.Marshal(hashes)
		_, err = tx.Exec(r.Context(), `UPDATE console_mfa SET enabled_at=now(),pending_until=NULL,last_step=$2,recovery_hashes=$3 WHERE user_id=$1`, u.ID, step, raw)
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE sessions SET mfa_verified_at=now() WHERE user_id=$1 AND token_hash=$2`, u.ID, digest(cookie.Value))
		}
		output = record{"enabled": true, "recovery_codes": codes, "recovery_codes_remaining": 10, "other_sessions_revoked": true}
	case "disable":
		if !enabled {
			fail(w, r, 409, "mfa_not_enabled", "MFA is not enabled")
			return
		}
		valid, e := s.consoleMFAProof(r, tx, u.ID, input.Code, input.Recovery)
		if e != nil {
			fail(w, r, 503, "mfa_key_unavailable", "Could not verify factor")
			return
		}
		if !valid {
			reject()
			return
		}
		_, err = tx.Exec(r.Context(), `DELETE FROM console_mfa WHERE user_id=$1`, u.ID)
		output = record{"enabled": false, "other_sessions_revoked": true}
	default:
		http.NotFound(w, r)
		return
	}
	if err == nil && action != "enroll" {
		_, err = tx.Exec(r.Context(), `UPDATE sessions SET revoked_at=now(),expires_at=LEAST(expires_at,now()) WHERE user_id=$1 AND token_hash<>$2 AND revoked_at IS NULL AND expires_at>now()`, u.ID, digest(cookie.Value))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,$2,'account_mfa',$1,'succeeded',$3)`, u.ID, "console_mfa_"+action, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "mfa_outcome_unconfirmed", "MFA outcome unconfirmed; reload current policy before retrying")
		return
	}
	jsonResponse(w, 200, output)
}
