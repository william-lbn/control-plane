package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConsoleSessionManagementIsolationRevocationAndStepUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "v2_console_sessions_" + randomToken(6)
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	password := "Console-Session-Test-Password-37"
	for _, actor := range []string{"alice", "bob"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,$2,'viewer')`, actor, hashPassword(password))
	}
	for _, name := range []string{"alice", "alice-target", "alice-keep", "alice-expired", "bob"} {
		actor := "alice"
		if name == "bob" {
			actor = "bob"
		}
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(name+"-token"), actor, digest(name+"-csrf"))
	}
	exec(`UPDATE sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, digest("alice-expired-token"))
	ids := map[string]string{}
	var idsValue string
	for _, name := range []string{"alice", "alice-target", "alice-keep", "bob"} {
		if e = db.QueryRow(ctx, `SELECT id FROM sessions WHERE token_hash=$1`, digest(name+"-token")).Scan(&idsValue); e != nil {
			t.Fatal(e)
		}
		ids[name] = idsValue
	}
	s := &server{db: db}
	call := func(actor, method, path, pwd string, csrf bool, want int) map[string]any {
		t.Helper()
		body, _ := json.Marshal(record{"password": pwd})
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-token"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		if csrf {
			r.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d expected %d: %s", method, path, w.Code, want, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "token_hash") || strings.Contains(w.Body.String(), "csrf_hash") || strings.Contains(w.Body.String(), password) {
			t.Fatal("Credential leak")
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	base := "/api/v1/account/sessions"
	list := call("alice", "GET", base, "", false, 200)
	if len(list["items"].([]any)) != 3 {
		t.Fatal("Other user or expired session exposed")
	}
	if list["items"].([]any)[0].(map[string]any)["current"] != true {
		t.Fatal("Current session not first")
	}
	path := base + "/" + ids["alice-target"] + "/revoke"
	call("alice", "POST", path, password, false, 403)
	call("alice", "POST", path, "wrong", true, 401)
	call("alice-target", "GET", "/api/v1/session", "", false, 200)
	call("alice", "POST", base+"/"+ids["bob"]+"/revoke", password, true, 404)
	call("alice", "POST", base+"/"+ids["alice"]+"/revoke", password, true, 409)
	out := call("alice", "POST", path, password, true, 200)
	if out["revoked_count"] != float64(1) {
		t.Fatal(out)
	}
	call("alice-target", "GET", "/api/v1/session", "", false, 401)
	if repeat := call("alice", "POST", path, password, true, 200); repeat["revoked_count"] != float64(0) || repeat["revoked_at"] != out["revoked_at"] {
		t.Fatal("Revocation not effect-idempotent")
	}
	if all := call("alice", "POST", base+"/revoke-others", password, true, 200); all["revoked_count"] != float64(1) {
		t.Fatal(all)
	}
	call("alice-keep", "GET", "/api/v1/session", "", false, 401)
	call("alice", "GET", "/api/v1/session", "", false, 200)
	call("bob", "GET", "/api/v1/session", "", false, 200)
	var legacyActive int
	if e = db.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id='alice' AND token_hash<>$1 AND expires_at>now()`, digest("alice-token")).Scan(&legacyActive); e != nil || legacyActive != 0 {
		t.Fatal("Legacy API readers could revive revoked sessions", e)
	}
	var audits int
	if e = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='revoke_console_session' AND actor_id='alice'`).Scan(&audits); e != nil || audits != 2 {
		t.Fatal("Audit missing/duplicated", e, audits)
	}
	exec(`INSERT INTO login_failures(scope_key) SELECT $1 FROM generate_series(1,9)`, "account-session:"+digest("alice"))
	call("alice", "POST", base+"/revoke-others", password, true, 429)
}

func TestConsoleSessionsRejectAPIKeyIdentity(t *testing.T) {
	s := &server{}
	for _, write := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/api/v1/account/sessions", nil)
		r = r.WithContext(context.WithValue(r.Context(), userKey, user{ID: "alice", APIKey: true}))
		w := httptest.NewRecorder()
		if write {
			s.revokeConsoleSessions(w, r)
		} else {
			s.consoleSessions(w, r)
		}
		if w.Code != 403 {
			t.Fatal("API key admitted to account sessions")
		}
	}
}
