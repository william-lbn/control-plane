package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Uses the real route middleware and a fresh PG schema, with concurrent token
// acceptance and membership revocation. No control-plane or Neon data fixtures.
func TestConsoleInvitationsIntegration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA") + "_inv"
	if !regexp.MustCompile(`^v2_auth_[a-z0-9_]+$`).MatchString(schema) || len(schema) > 63 {
		t.Fatal("Fresh v2_auth_* schema required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_inv','inv','Invitation test'),('org_other','other','Other')`)
	for _, name := range []string{"admin_inv", "viewer_inv", "other_inv", "existing_inv"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,$2,'owner')`, name, hashPassword("Existing-Password-123"))
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(name+"-session"), name, digest(name+"-csrf"))
	}
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_inv','admin_inv','admin'),('org_inv','viewer_inv','viewer'),('org_other','other_inv','admin')`)
	s := &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.routes()
	request := func(actor, method, path string, body any, key string) (int, record) {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		if actor != "" {
			r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
			r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
			r.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var v record
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	want := func(actor, method, path string, body any, key string, status int) record {
		t.Helper()
		code, v := request(actor, method, path, body, key)
		if code != status {
			t.Fatalf("%s %s: got %d want %d (error code %v)", method, path, code, status, nested(v, "error", "code"))
		}
		return v
	}
	path := "/api/v1/organizations/org_inv/invitations"
	invite := func(name, key string) record {
		t.Helper()
		return want("admin_inv", "POST", path, record{"username": name, "role": "collaborator", "expires_hours": 24}, key, 201)
	}
	signup := func(v record, name string) record {
		return record{"token": v["token"], "username": name, "password": "New-User-Password-123"}
	}
	t.Run("admin scope csrf API key and secret boundaries", func(t *testing.T) {
		body := record{"username": "new_one", "role": "collaborator", "expires_hours": 24}
		want("viewer_inv", "POST", path, body, "invite-key-one", 403)
		want("other_inv", "GET", path, nil, "", 404)
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"username":"new_one","role":"viewer","expires_hours":24}`))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "admin_inv-session"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("Missing CSRF accepted")
		}
		key := want("admin_inv", "POST", "/api/v1/organizations/org_inv/api-keys", record{"name": "test", "max_role": "admin", "expires_days": 1}, "", 201)
		r = httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+key["token"].(string))
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("API key managed invitations")
		}
		inv := invite("new_one", "invite-key-one")
		if inv["secret_available"] != true || !validInvitationToken(inv["token"].(string)) {
			t.Fatal("Missing one-time token")
		}
		var hash string
		if err := db.QueryRow(ctx, `SELECT token_hash FROM console_invitations WHERE id=$1`, inv["id"]).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if hash != digest(inv["token"].(string)) {
			t.Fatal("Invitation token persisted in clear")
		}
		replay := want("admin_inv", "POST", path, body, "invite-key-one", 200)
		if replay["id"] != inv["id"] || replay["token"] != nil || replay["secret_available"] != false {
			t.Fatal("Replay leaked token or duplicated invitation")
		}
		body["role"] = "admin"
		want("admin_inv", "POST", path, body, "invite-key-one", 409)
		list := want("admin_inv", "GET", path, nil, "", 200)
		b, _ := json.Marshal(list)
		if strings.Contains(string(b), inv["token"].(string)) || strings.Contains(string(b), hash) {
			t.Fatal("List leaks token/hash")
		}
		want("", "POST", "/auth/signup", signup(inv, "wrong_name"), "", 422)
		got := want("", "POST", "/auth/signup", signup(inv, "new_one"), "", 201)
		var role, stored string
		if err := db.QueryRow(ctx, `SELECT m.role,u.password_hash FROM users u JOIN organization_members m ON m.user_id=u.id WHERE u.id=$1`, got["user_id"]).Scan(&role, &stored); err != nil {
			t.Fatal(err)
		}
		if role != "collaborator" || !verifyPassword("New-User-Password-123", stored) {
			t.Fatal("Membership/password mismatch")
		}
		want("", "POST", "/auth/signup", signup(inv, "new_one"), "", 422)
		want("admin_inv", "DELETE", path+"/"+inv["id"].(string), nil, "", 409)
		want("", "POST", "/auth/login", record{"username": "new_one", "password": "New-User-Password-123"}, "", 200)
	})
	t.Run("existing accounts require own session and retain passwords", func(t *testing.T) {
		inv := invite("existing_inv", "existing-invite-key")
		var before string
		_ = db.QueryRow(ctx, `SELECT password_hash FROM users WHERE id='existing_inv'`).Scan(&before)
		want("", "POST", "/auth/signup", signup(inv, "existing_inv"), "", 409)
		want("viewer_inv", "POST", "/api/v1/invitations/accept", record{"token": inv["token"]}, "", 403)
		want("existing_inv", "POST", "/api/v1/invitations/accept", record{"token": inv["token"], "password": "Dont-reset-this-123"}, "", 422)
		want("existing_inv", "POST", "/api/v1/invitations/accept", record{"token": inv["token"]}, "", 201)
		var after string
		_ = db.QueryRow(ctx, `SELECT password_hash FROM users WHERE id='existing_inv'`).Scan(&after)
		if after != before {
			t.Fatal("Existing password changed")
		}
		want("existing_inv", "POST", "/api/v1/invitations/accept", record{"token": inv["token"]}, "", 422)
		want("admin_inv", "DELETE", "/api/v1/organizations/org_inv/members/existing_inv", nil, "", 200)
		want("existing_inv", "GET", path, nil, "", 404)
	})
	t.Run("expiry revoke inviter demotion fail closed", func(t *testing.T) {
		for i, name := range []string{"expired_one", "revoked_one", "demoted_one"} {
			inv := invite(name, "invalid-invite-"+name)
			if i == 0 {
				exec(`UPDATE console_invitations SET created_at=now()-interval '2 hour',expires_at=now()-interval '1 hour' WHERE id=$1`, inv["id"])
			}
			if i == 1 {
				want("admin_inv", "DELETE", path+"/"+inv["id"].(string), nil, "", 200)
				want("admin_inv", "DELETE", path+"/"+inv["id"].(string), nil, "", 200)
			}
			if i == 2 {
				exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_inv','other_inv','admin')`)
				want("other_inv", "PATCH", "/api/v1/organizations/org_inv/members/admin_inv", record{"role": "viewer"}, "", 200)
			}
			want("", "POST", "/auth/signup", signup(inv, name), "", 422)
			if i == 2 {
				want("other_inv", "PATCH", "/api/v1/organizations/org_inv/members/admin_inv", record{"role": "admin"}, "", 200)
				want("", "POST", "/auth/signup", signup(inv, name), "", 422)
			}
		}
		var count int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM users WHERE username IN ('expired_one','revoked_one','demoted_one')`).Scan(&count)
		if count != 0 {
			t.Fatal("Rejected registration persisted user")
		}
	})
	t.Run("concurrent accept consumes once", func(t *testing.T) {
		inv := invite("concurrent_one", "concurrent-invite-key")
		codes := make(chan int, 4)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, _ := request("", "POST", "/auth/signup", signup(inv, "concurrent_one"), "")
				codes <- code
			}()
		}
		wg.Wait()
		close(codes)
		successes := 0
		for code := range codes {
			if code == 201 {
				successes++
			} else if code != 422 {
				t.Fatalf("Unexpected racing acceptance %d", code)
			}
		}
		if successes != 1 {
			t.Fatal("Invitation consumed more or less than once")
		}
		var audits int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action='register_console_account'`, inv["id"]).Scan(&audits)
		if audits != 1 {
			t.Fatal("Audit is not atomic with acceptance")
		}
	})
	t.Run("anonymous cross-origin and atomic admission cap", func(t *testing.T) {
		inv := invite("limited_one", "limited-invite-key")
		data, _ := json.Marshal(signup(inv, "limited_one"))
		r := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(string(data)))
		r.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("Cross-origin registration accepted")
		}
		exec(`INSERT INTO console_registration_limits(scope_hash,window_at,attempts) VALUES($1,date_trunc('minute',now()),20) ON CONFLICT(scope_hash,window_at) DO UPDATE SET attempts=20`, digest("console-signup:192.0.2.1"))
		want("", "POST", "/auth/signup", signup(inv, "limited_one"), "", 429)
	})
}
