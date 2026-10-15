package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSQLHistoryPolicyReplayPrivacyCaptureAndAtomicRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	schema := "v2_history_policy_" + randomToken(6)
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
	for _, actor := range []string{"alice", "bob"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','viewer')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-token"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_history','local','History','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_history','prj_history','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_history','prj_history','br_history','ep-history-policy','VirtualMachine','history-policy','history-policy','suspended','history_role','history_db',now(),now())`)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{4}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{3}, 32)}}}
	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "true")
	request := func(actor string) *http.Request {
		r := httptest.NewRequest("POST", "/query", nil)
		r.SetPathValue("project", "prj_history")
		r.SetPathValue("endpoint", "ep_history")
		return r.WithContext(context.WithValue(ctx, userKey, user{ID: actor}))
	}
	admit := func(actor, outcome string, age int) *sqlHistoryAdmission {
		t.Helper()
		h, e := s.beginSQLHistory(request(actor), record{"branch_id": "br_history"}, "single", "history_role", "history_db", "SELECT 'synthetic-disposable-history'")
		if e != nil || h == nil {
			t.Fatal("History admission missing", e)
		}
		if outcome != "admitted" {
			h.finish(httptest.NewRecorder(), outcome, "")
		}
		exec(`UPDATE sql_query_history SET started_at=now()-make_interval(days=>$2),deadline_at=now()-interval '1 day' WHERE id=$1`, h.id, age)
		return h
	}
	for _, outcome := range []string{"completed", "failed", "not_submitted"} {
		admit("alice", outcome, 10)
	}
	unknown := admit("alice", "outcome_unknown", 10)
	pending := admit("alice", "admitted", 10)
	recent := admit("alice", "completed", 2)
	missing := admit("alice", "completed", 10)
	exec(`UPDATE sql_query_history SET finished_at=NULL WHERE id=$1`, missing.id)
	bob := admit("bob", "completed", 20)
	if n, e := s.pruneSQLHistory(ctx, "", "default-held"); e != nil || n != 0 {
		t.Fatal("Default policy deleted records", n, e)
	}
	call := func(actor, method, path, key, version string, body any, want int) record {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-token"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", version)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("Policy %s %d expected %d: %s", method, w.Code, want, w.Body.String())
		}
		var v record
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	base := "/api/v1/account/query-history-policy"
	body := record{"capture_enabled": false, "retain_days": 7, "acknowledge_expired_history_deletion": false}
	initial := call("alice", "GET", base, "", "", nil, 200)
	if initial["version_token"] != "0" || initial["retain_days"] != float64(0) {
		t.Fatal("Unsafe virtual default")
	}
	call("alice", "PATCH", base, "history-policy-first", `"0"`, body, 422)
	body["acknowledge_expired_history_deletion"] = true
	call("alice", "PATCH", base, "history-policy-first", "", body, 428)
	value := call("alice", "PATCH", base, "history-policy-first", `"0"`, body, 200)
	if value["version_token"] != "1" || value["capture_enabled"] != false {
		t.Fatal("Policy not applied")
	}
	replay := call("alice", "PATCH", base, "history-policy-first", `"0"`, body, 200)
	if replay["version_token"] != "1" {
		t.Fatal("Policy replay mutated generation")
	}
	changed := record{"capture_enabled": true, "retain_days": 7, "acknowledge_expired_history_deletion": true}
	call("alice", "PATCH", base, "history-policy-first", `"0"`, changed, 409)
	call("alice", "PATCH", base, "history-policy-stale", `"0"`, changed, 412)
	if p := call("bob", "GET", base, "", "", nil, 200); p["version_token"] != "0" || p["capture_enabled"] != true {
		t.Fatal("Other account policy changed")
	}
	if h, e := s.beginSQLHistory(request("alice"), record{"branch_id": "br_history"}, "single", "history_role", "history_db", "SELECT 'must-not-be-recorded'"); e != nil || h != nil {
		t.Fatal("Capture opt-out ignored", e)
	}
	if n, e := s.pruneSQLHistory(ctx, "bob", "bob-held"); e != nil || n != 0 {
		t.Fatal("Cross-account cleanup", n, e)
	}
	call("alice", "POST", "/api/v1/account/query-history-cleanup", "", "", record{"acknowledge_expired_history_deletion": false}, 422)
	exec(`CREATE FUNCTION reject_history_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='prune_sql_history' THEN RAISE EXCEPTION 'injected audit failure'; END IF;RETURN NEW;END $$`)
	exec(`CREATE TRIGGER reject_history_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_history_audit()`)
	if _, e := s.pruneSQLHistory(ctx, "alice", "audit-failure"); e == nil {
		t.Fatal("Audit failure ignored")
	}
	var count int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM sql_query_history`).Scan(&count); err != nil || count != 8 {
		t.Fatal("Failed audit did not roll back physical deletion", count, err)
	}
	exec(`DROP TRIGGER reject_history_audit ON audit_events`)
	var wg sync.WaitGroup
	numbers := make(chan int, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, e := s.pruneSQLHistory(ctx, "alice", "concurrent-retention")
			numbers <- n
			errs <- e
		}()
	}
	wg.Wait()
	close(numbers)
	close(errs)
	total := 0
	for n := range numbers {
		total += n
	}
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if total != 3 {
		t.Fatal("Concurrent cleanup lost or repeated rows", total)
	}
	for _, h := range []*sqlHistoryAdmission{unknown, pending, recent, missing, bob} {
		var exists bool
		if err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sql_query_history WHERE id=$1)`, h.id).Scan(&exists); err != nil || !exists {
			t.Fatal("Protected/other/recent history deleted", err)
		}
	}
	call("bob", "PATCH", base, "history-policy-bob", `"0"`, changed, 200)
	if n, e := s.pruneSQLHistory(ctx, "", "automatic-retention"); e != nil || n != 1 {
		t.Fatal("Automatic retention did not honor explicit owner policy", n, e)
	}
	restore := record{"capture_enabled": true, "retain_days": 0, "acknowledge_expired_history_deletion": false}
	call("alice", "PATCH", base, "history-policy-restore", `"1"`, restore, 200)
	admit("alice", "completed", 0)
	var audits int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('update_sql_history_policy','prune_sql_history')`).Scan(&audits); err != nil || audits != 5 {
		t.Fatal("Policy/cleanup audit not atomic", audits, err)
	}
}
