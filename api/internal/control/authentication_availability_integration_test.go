package control

import (
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

// An unavailable authentication store must never authorize a request or label
// a valid session invalid. No credentials or raw database details serialize.
func TestAuthenticationStateAvailabilityFailsClosed(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_auth_available_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
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
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('availability_actor','availability_actor','unused','owner')`)
	exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,'availability_actor',$2,now()+interval '1 hour')`, digest("availability-session"), digest("availability-csrf"))
	s := &server{db: db}
	forwarded := false
	handler := s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true; w.WriteHeader(200) }), false)
	call := func(cookie, bearer string, status int) *httptest.ResponseRecorder {
		t.Helper()
		forwarded = false
		r := httptest.NewRequest("GET", "/api/v1/session", nil)
		r = r.WithContext(ctx)
		if cookie != "" {
			r.Header.Set("Cookie", "neon_v2_session="+cookie)
		}
		if bearer != "" {
			r.Header.Set("Authorization", bearer)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status || forwarded != (status == 200) {
			t.Fatalf("status/authorization %d/%v expected %d: %s", w.Code, forwarded, status, w.Body.String())
		}
		return w
	}
	call("availability-session", "", 200)
	call("", "", 401)
	call("unknown-session", "", 401)
	call("", "Bearer malformed", 401)
	call("", "Bearer ncp_unknown", 401)
	exec(`UPDATE sessions SET revoked_at=now()`)
	call("availability-session", "", 401)
	exec(`UPDATE sessions SET revoked_at=NULL,expires_at=now()-interval '1 second'`)
	call("availability-session", "", 401)
	exec(`UPDATE sessions SET expires_at=now()+interval '1 hour'`)
	exec(`UPDATE users SET disabled=true`)
	call("availability-session", "", 401)
	exec(`UPDATE users SET disabled=false`)
	call("availability-session", "", 200)
	db.Close()
	for _, input := range [][2]string{{"availability-session", ""}, {"", "Bearer ncp_unknown"}} {
		w := call(input[0], input[1], 503)
		var body struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != "authentication_unavailable" || !body.Retryable || w.Header().Get("Retry-After") != "1" {
			t.Fatal("unavailable auth contract")
		}
		for _, private := range []string{"pool closed", "postgres://", "availability-session", "availability-csrf"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatal("private error material escaped")
			}
		}
	}
	call("", "", 401)
	call("", "Bearer malformed", 401)
}
