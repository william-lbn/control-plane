package control

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSQLHistoryDeleteIsolationProtectionReplayAndAtomicAudit(t *testing.T) {
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
	schema := "v2_history_delete_" + randomToken(6)
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
	for _, actor := range []string{"alice", "bob", "viewer", "outsider"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','viewer')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-token"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_history_delete','history-delete','History deletion')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_history_delete','alice','editor'),('org_history_delete','bob','admin'),('org_history_delete','viewer','viewer')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_history_delete','org_history_delete','History deletion','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_history_delete','prj_history_delete','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_history_delete','prj_history_delete','br_history_delete','ep-history-delete','VirtualMachine','history-delete','history-delete','suspended','history_role','history_db',now(),now())`)
	s := &server{db: db, backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{3}, 32)}}}
	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "true")
	admit := func(owner, outcome string) string {
		t.Helper()
		r := httptest.NewRequest("POST", "/query", nil)
		r.SetPathValue("project", "prj_history_delete")
		r.SetPathValue("endpoint", "ep_history_delete")
		r = r.WithContext(context.WithValue(ctx, userKey, user{ID: owner}))
		h, e := s.beginSQLHistory(r, record{"branch_id": "br_history_delete"}, "single", "history_role", "history_db", "SELECT 'disposable-delete-test'")
		if e != nil || h == nil {
			t.Fatal("admission", e)
		}
		if outcome != "admitted" {
			h.finish(httptest.NewRecorder(), outcome, "")
		}
		return h.id
	}
	completed, failed, unsubmitted := admit("alice", "completed"), admit("alice", "failed"), admit("alice", "not_submitted")
	pending, unknown, missing := admit("alice", "admitted"), admit("alice", "outcome_unknown"), admit("alice", "completed")
	foreign := admit("bob", "completed")
	exec(`UPDATE sql_query_history SET finished_at=NULL WHERE id=$1`, missing)
	base := "/api/v1/projects/prj_history_delete/query-history/"
	call := func(actor, id, query, csrf string) int {
		r := httptest.NewRequest("DELETE", base+id+query, nil)
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-token"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Error("Deletion response can be cached")
		}
		return w.Code
	}
	ack := "?acknowledge_history_deletion=true"
	want := func(actor, id, query, csrf string, status int) {
		t.Helper()
		if n := call(actor, id, query, csrf); n != status {
			t.Fatalf("delete status %d expected %d", n, status)
		}
	}
	want("alice", completed, "", "alice-csrf", 422)
	want("alice", completed, ack+"&acknowledge_history_deletion=true", "alice-csrf", 422)
	want("alice", completed, ack, "", 403)
	want("outsider", completed, ack, "outsider-csrf", 404)
	want("viewer", completed, ack, "viewer-csrf", 403)
	want("bob", completed, ack, "bob-csrf", 204) // Even a project admin cannot delete Alice's entry.
	want("alice", foreign, ack, "alice-csrf", 204)
	for _, id := range []string{pending, unknown, missing} {
		want("alice", id, ack, "alice-csrf", 409)
	}
	// API keys cannot operate on private Console history, even with project access.
	r := httptest.NewRequest("DELETE", base+completed+ack, nil)
	r = r.WithContext(context.WithValue(ctx, userKey, user{ID: "alice", APIKey: true}))
	w := httptest.NewRecorder()
	s.deleteSQLHistory(w, r)
	if w.Code != 403 {
		t.Fatal("API key deleted personal history")
	}
	exec(`CREATE FUNCTION reject_history_delete_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='delete_sql_history' THEN RAISE EXCEPTION 'injected audit failure'; END IF;RETURN NEW;END $$`)
	exec(`CREATE TRIGGER reject_history_delete_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_history_delete_audit()`)
	want("alice", completed, ack, "alice-csrf", 503)
	var exists bool
	if err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sql_query_history WHERE id=$1)`, completed).Scan(&exists); err != nil || !exists {
		t.Fatal("Audit failure lost history", err)
	}
	exec(`DROP TRIGGER reject_history_delete_audit ON audit_events`)
	// No keyring or enabled capture is required to remove an otherwise eligible entry.
	s.backendKeys = nil
	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "false")
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); statuses <- call("alice", completed, ack, "alice-csrf") }()
	}
	wg.Wait()
	close(statuses)
	for n := range statuses {
		if n != 204 {
			t.Fatal("Concurrent DELETE not idempotent", n)
		}
	}
	want("alice", completed, ack, "alice-csrf", 204)
	for _, id := range []string{failed, unsubmitted} {
		want("alice", id, ack, "alice-csrf", 204)
	}
	var count, audits int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM sql_query_history`).Scan(&count); err != nil || count != 4 {
		t.Fatal("Protected or foreign history changed", count, err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='delete_sql_history'`).Scan(&audits); err != nil || audits != 3 {
		t.Fatal("Audit missing or replay duplicated it", audits, err)
	}
	for _, id := range []string{pending, unknown, missing, foreign} {
		if err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sql_query_history WHERE id=$1)`, id).Scan(&exists); err != nil || !exists {
			t.Fatal("Protected/foreign entry lost", err)
		}
	}
}
