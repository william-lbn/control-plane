package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"
)

func TestConsoleMFAReplayRecoveryIsolationAndLegacyGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_mfa_" + randomToken(6)
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
	pwd := "Console-MFA-disposable-test-password-1"
	for _, actor := range []string{"alice", "bob"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,$2,'viewer')`, actor, hashPassword(pwd))
	}
	for _, name := range []string{"alice", "alice-old", "bob"} {
		actor := strings.Split(name, "-")[0]
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(name+"-token"), actor, digest(name+"-csrf"))
	}
	s := &server{db: db, backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{8}, 32)}}}
	call := func(actor, method, path string, body any, want int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-token"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s %d expected %d: %s", method, path, w.Code, want, w.Body.String())
		}
		var result map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		return result
	}
	base := "/api/v1/account/mfa"
	proof := func(code string, recovery bool) record {
		return record{"password": pwd, "code": code, "recovery": recovery}
	}
	call("alice", "POST", base+"/enroll", record{"password": "wrong"}, 401)
	enroll := call("alice", "POST", base+"/enroll", proof("", false), 200)
	secret := enroll["secret"].(string)
	var sealed []byte
	exec(`UPDATE console_mfa SET pending_until=now()-interval '1 second' WHERE user_id='alice'`)
	call("alice", "POST", base+"/confirm", proof("000000", false), 409)
	enroll = call("alice", "POST", base+"/enroll", proof("", false), 200)
	secret = enroll["secret"].(string)
	if err = db.QueryRow(ctx, `SELECT sealed_secret FROM console_mfa WHERE user_id='alice'`).Scan(&sealed); err != nil || bytes.Contains(sealed, []byte(secret)) {
		t.Fatal("Factor secret not encrypted", err)
	}
	other := call("bob", "GET", base, nil, 200)
	if other["enabled"] != false || strings.Contains(fmt.Sprint(other), secret) {
		t.Fatal("Factor leaked across accounts")
	}
	call("alice", "POST", base+"/confirm", proof("invalid", false), 401)
	prior, _ := totp.GenerateCode(secret, time.Now().Add(-30*time.Second))
	confirmed := call("alice", "POST", base+"/confirm", proof(prior, false), 200)
	values := confirmed["recovery_codes"].([]any)
	if len(values) != 10 {
		t.Fatal("Recovery code count")
	}
	codes := make([]string, 10)
	for i, v := range values {
		codes[i] = v.(string)
	}
	call("alice-old", "GET", "/api/v1/session", nil, 401)
	call("alice", "GET", "/api/v1/session", nil, 200)
	call("alice", "POST", base+"/confirm", proof(prior, false), 409)
	if _, err = db.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES('legacy','alice','legacy',now()+interval '1 hour')`); err == nil {
		t.Fatal("Legacy password-only session accepted")
	}
	login := func(code string, recovery bool) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(record{"username": "alice", "password": pwd, "mfa_code": code, "recovery": recovery})
		r := httptest.NewRequest("POST", "/auth/login", bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.20:1234"
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	if w := login("", false); w.Code != 401 || len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "mfa_required") {
		t.Fatal("Password bypassed factor")
	}
	if w := login(prior, false); w.Code != 401 {
		t.Fatal("Enrollment code replay accepted")
	}
	current, _ := totp.GenerateCode(secret, time.Now())
	if w := login(current, false); w.Code != 200 {
		t.Fatal("Valid authenticator rejected", w.Code, w.Body.String())
	}
	if w := login(current, false); w.Code != 401 {
		t.Fatal("TOTP replay accepted")
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); statuses <- login(codes[0], true).Code }()
	}
	wg.Wait()
	close(statuses)
	successes, denials := 0, 0
	for code := range statuses {
		if code == 200 {
			successes++
		}
		if code == 401 {
			denials++
		}
	}
	if successes != 1 || denials != 1 {
		t.Fatal("Recovery code not consumed atomically", successes, denials)
	}
	state := call("alice", "GET", base, nil, 200)
	if state["recovery_codes_remaining"] != float64(9) || strings.Contains(fmt.Sprint(state), secret) || strings.Contains(fmt.Sprint(state), codes[0]) {
		t.Fatal("Status leaked secret or recovery consumption lost")
	}
	call("alice", "POST", base+"/disable", proof(codes[0], true), 401)
	call("alice", "POST", base+"/disable", proof(codes[1], true), 200)
	call("alice", "GET", "/api/v1/session", nil, 200)
	call("bob", "GET", "/api/v1/session", nil, 200)
	if w := login("", false); w.Code != 200 {
		t.Fatal("Disabled factor still required")
	}
	var audits int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_type='account_mfa'`).Scan(&audits); err != nil || audits != 6 {
		t.Fatal("MFA mutation/login audit incomplete", audits, err)
	}
}

func TestConsoleMFACounterWindowAndReplay(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	at := time.Unix(1700000010, 0)
	for _, delta := range []int{-1, 0, 1} {
		code, _ := totp.GenerateCode(secret, at.Add(time.Duration(delta)*30*time.Second))
		step, ok := consoleMFAStep(secret, code, at, -1)
		if !ok {
			t.Fatal("Window rejected", delta)
		}
		if _, ok = consoleMFAStep(secret, code, at, step); ok {
			t.Fatal("Consumed counter accepted")
		}
	}
	code, _ := totp.GenerateCode(secret, at.Add(-90*time.Second))
	if _, ok := consoleMFAStep(secret, code, at, -1); ok {
		t.Fatal("Expired code accepted")
	}
}
