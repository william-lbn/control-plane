package control

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type user struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	CSRFHash   string `json:"-"`
	APIKey     bool   `json:"-"`
	KeyID      string `json:"-"`
	KeyOrg     string `json:"-"`
	KeyProject string `json:"-"`
	KeyRole    string `json:"-"`
}

func digest(value string) string {
	b := sha256.Sum256([]byte(value))
	return hex.EncodeToString(b[:])
}

func pbkdf2(password string, salt []byte, iterations, keyLen int) []byte {
	key := make([]byte, 0, keyLen)
	for block := uint32(1); len(key) < keyLen; block++ {
		msg := append(append([]byte{}, salt...), byte(block>>24), byte(block>>16), byte(block>>8), byte(block))
		mac := hmac.New(sha256.New, []byte(password))
		mac.Write(msg)
		u := mac.Sum(nil)
		t := append([]byte{}, u...)
		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, []byte(password))
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		key = append(key, t...)
	}
	return key[:keyLen]
}

func hashPassword(password string) string {
	salt := make([]byte, 24)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	return "pbkdf2_sha256$310000$" + hex.EncodeToString(salt) + "$" +
		hex.EncodeToString(pbkdf2(password, salt, 310000, 32))
}

func verifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 100000 || iterations > 1000000 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(parts[3])
	if err != nil || len(expected) == 0 {
		return false
	}
	return hmac.Equal(pbkdf2(password, salt, iterations, len(expected)), expected)
}

func randomToken(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *server) bootstrapAdmin(ctx context.Context) error {
	var count int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		// Membership revocation must survive process restart. Bootstrap is not
		// a permanent global-role bypass or automatic regrant mechanism.
		return nil
	}
	file := os.Getenv("NEON_V2_ADMIN_PASSWORD_FILE")
	if file == "" {
		return errors.New("NEON_V2_ADMIN_PASSWORD_FILE required for first bootstrap")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	password := strings.TrimSpace(string(b))
	if len(password) < 12 {
		return errors.New("admin password too short")
	}
	_, err = s.db.Exec(ctx, "INSERT INTO users(id,username,password_hash,role) VALUES('usr_local_admin','admin',$1,'owner') ON CONFLICT DO NOTHING", hashPassword(password))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO organization_members(org_id,user_id,role) VALUES('local','usr_local_admin','owner')
		ON CONFLICT(org_id,user_id) DO NOTHING`)
	return err
}

func (s *server) currentUser(r *http.Request) (user, error) {
	if r.Header.Get("Authorization") != "" {
		return s.bearerUser(r)
	}
	cookie, err := r.Cookie("neon_v2_session")
	if err != nil {
		return user{}, err
	}
	var u user
	err = s.db.QueryRow(r.Context(), `SELECT u.id,u.username,u.role,s.csrf_hash
        FROM sessions s JOIN users u ON u.id=s.user_id
        WHERE s.token_hash=$1 AND s.expires_at > now() AND NOT u.disabled`, digest(cookie.Value)).
		Scan(&u.ID, &u.Username, &u.Role, &u.CSRFHash)
	return u, err
}

func (s *server) auth(next http.Handler, write bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.currentUser(r)
		if err != nil {
			fail(w, r, 401, "unauthenticated", "Please sign in")
			return
		}
		if write && !u.APIKey {
			cookie, err := r.Cookie("neon_v2_csrf")
			if err != nil || cookie.Value == "" || !hmac.Equal([]byte(cookie.Value), []byte(r.Header.Get("X-CSRF-Token"))) ||
				!hmac.Equal([]byte(digest(cookie.Value)), []byte(u.CSRFHash)) {
				fail(w, r, 403, "csrf_failed", "CSRF token missing or invalid")
				return
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), userKey, u))
		if authorized, ok := s.authorizeRoute(w, r, u); ok {
			next.ServeHTTP(w, authorized)
		}
	})
}

func userFrom(r *http.Request) user { u, _ := r.Context().Value(userKey).(user); return u }

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		fail(w, r, 422, "invalid_request", "Username and password are required")
		return
	}
	ip, _, splitErr := net.SplitHostPort(r.RemoteAddr)
	if splitErr != nil {
		ip = r.RemoteAddr
	}
	ipScope := "ip:" + digest(ip)
	userScope := "user:" + digest(strings.ToLower(strings.TrimSpace(body.Username)))
	var ipFailures, userFailures int
	if err := s.db.QueryRow(r.Context(), `SELECT
		(SELECT count(*) FROM login_failures WHERE scope_key=$1 AND occurred_at>now()-interval '15 minutes'),
		(SELECT count(*) FROM login_failures WHERE scope_key=$2 AND occurred_at>now()-interval '15 minutes')`, ipScope, userScope).Scan(&ipFailures, &userFailures); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check login policy")
		return
	}
	if ipFailures >= 10 || userFailures >= 20 {
		w.Header().Set("Retry-After", "900")
		fail(w, r, 429, "login_rate_limited", "Too many login attempts; retry later")
		return
	}
	var u user
	var stored string
	err := s.db.QueryRow(r.Context(), "SELECT id,username,role,password_hash FROM users WHERE username=$1 AND NOT disabled", body.Username).
		Scan(&u.ID, &u.Username, &u.Role, &stored)
	if err != nil || !verifyPassword(body.Password, stored) {
		_, _ = s.db.Exec(r.Context(), "INSERT INTO login_failures(scope_key) VALUES($1),($2)", ipScope, userScope)
		fail(w, r, 401, "invalid_credentials", "Username or password is incorrect")
		return
	}
	token, csrf := randomToken(48), randomToken(32)
	_, err = s.db.Exec(r.Context(), "INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '8 hours')",
		digest(token), u.ID, digest(csrf))
	if err != nil {
		s.logger.Error("session insert", "error", err)
		fail(w, r, 503, "metadata_unavailable", "Could not create session")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "neon_v2_session", Value: token, Path: "/", HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 28800})
	http.SetCookie(w, &http.Cookie{Name: "neon_v2_csrf", Value: csrf, Path: "/", Secure: s.secureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 28800})
	jsonResponse(w, 200, map[string]any{"user": u})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie("neon_v2_session")
	if cookie != nil {
		_, _ = s.db.Exec(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", digest(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "neon_v2_session", Path: "/", MaxAge: -1, Secure: s.secureCookies, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: "neon_v2_csrf", Path: "/", MaxAge: -1, Secure: s.secureCookies, SameSite: http.SameSiteStrictMode})
	jsonResponse(w, 200, map[string]bool{"ok": true})
}

func (s *server) session(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, map[string]any{"user": userFrom(r)})
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func (s *server) audit(ctx context.Context, actor, action, resourceType, resourceID, outcome, requestID string) {
	_, err := s.db.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id)
        VALUES($1,$2,$3,$4,$5,$6)`, actor, action, resourceType, resourceID, outcome, requestID)
	if err != nil {
		s.logger.Error("audit insert failed", "error", err, "request_id", requestID)
	}
}

func (s *server) cleanSessions(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.db.Exec(ctx, "DELETE FROM sessions WHERE expires_at < now()")
			_, _ = s.db.Exec(ctx, "DELETE FROM login_failures WHERE occurred_at < now()-interval '1 day'")
		}
	}
}
