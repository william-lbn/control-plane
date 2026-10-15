package control

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSQLHistoryDurabilityPrivacyPaginationAndFailureStates(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, e := pgxpool.ParseConfig(dsn)
	if dsn == "" || e != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "v2_saved_" + randomToken(6)
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
	for _, actor := range []string{"alice", "bob", "viewer", "outsider"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_saved','saved','Saved'),('org_other','other','Other')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_saved','alice','admin'),('org_saved','bob','editor'),('org_saved','viewer','viewer'),('org_other','outsider','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_saved','org_saved','Saved','lab',16,'ready','managed',now(),now()),('prj_other','org_other','Other','lab',16,'ready','managed',now(),now())`)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{0x42}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{2}, 32)}}}
	call := func(actor, method, path, key, version string, body any, want int) (map[string]any, *httptest.ResponseRecorder) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", version)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s status %d expected %d; body omitted", method, w.Code, want)
		}
		var v map[string]any
		json.Unmarshal(w.Body.Bytes(), &v)
		return v, w
	}

	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "true")
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_saved','prj_saved','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_saved','prj_saved','br_saved','ep-history-test','VirtualMachine','cp-history-test','cp-history-test','suspended','example_role','example_db',now(),now())`)
	request := httptest.NewRequest("POST", "/api/v1/projects/prj_saved/endpoints/ep_saved/query", nil)
	request.SetPathValue("project", "prj_saved")
	request.SetPathValue("endpoint", "ep_saved")
	request = request.WithContext(context.WithValue(ctx, userKey, user{ID: "alice"}))
	text := "SELECT 'private-history-only'; /*" + strings.Repeat("中文", 2500) + "*/"
	item := record{"branch_id": "br_saved"}
	first, e := s.beginSQLHistory(request, item, "single", "example_role", "example_db", text)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	first.finish(w, "completed", "")
	if w.Header().Get("X-Neon-SQL-History-ID") != first.id {
		t.Fatal("history receipt missing")
	}
	first.finish(httptest.NewRecorder(), "failed", "42601")
	for i := 0; i < 29; i++ {
		if _, e = s.beginSQLHistory(request, item, "script", "example_role", "example_db", "SELECT 1"); e != nil {
			t.Fatal(e)
		}
	}
	path := "/api/v1/projects/prj_saved/query-history"
	v, w := call("alice", "GET", path, "", "", nil, 200)
	if len(v["items"].([]any)) != 25 || v["next_cursor"] == nil || strings.Contains(w.Body.String(), "SELECT") || strings.Contains(w.Body.String(), "private-history") {
		t.Fatal("metadata pagination or privacy failed")
	}
	cursor := v["next_cursor"].(string)
	v, _ = call("alice", "GET", path+"?cursor="+cursor, "", "", nil, 200)
	if len(v["items"].([]any)) != 5 {
		t.Fatal("keyset continuation lost rows")
	}
	call("bob", "GET", path+"?cursor="+cursor, "", "", nil, 422)
	call("bob", "GET", path+"/"+first.id, "", "", nil, 404)
	call("viewer", "GET", path, "", "", nil, 403)
	call("outsider", "GET", path, "", "", nil, 404)
	v, _ = call("alice", "GET", path+"/"+first.id, "", "", nil, 200)
	saved := v["sql"].(string)
	if len(saved) > sqlHistoryLimit || !strings.HasPrefix(saved, "-- QUERY TRUNCATED\n") || v["truncated"] != true || v["outcome"] != "completed" {
		t.Fatal("truncation or finish-once state failed")
	}
	var sealed []byte
	if e = db.QueryRow(ctx, `SELECT sealed_sql FROM sql_query_history WHERE id=$1`, first.id).Scan(&sealed); e != nil || bytes.Contains(sealed, []byte("private-history")) {
		t.Fatal("plaintext SQL persisted", e)
	}
	if _, e = s.openSavedSQL("v1", sealed, sqlHistoryAAD("prj_saved", "bob", first.id)); e == nil {
		t.Fatal("other owner AAD admitted")
	}
	s.backendKeys.Active = "v2"
	call("alice", "GET", path+"/"+first.id, "", "", nil, 200)
	old := s.backendKeys.decoded["v1"]
	delete(s.backendKeys.decoded, "v1")
	call("alice", "GET", path+"/"+first.id, "", "", nil, 503)
	s.backendKeys.decoded["v1"] = old
	incomplete, e := s.beginSQLHistory(request, item, "single", "example_role", "example_db", "SELECT 9")
	if e != nil {
		t.Fatal(e)
	}
	exec(`UPDATE sql_query_history SET deadline_at=now()-interval '1 second' WHERE id=$1`, incomplete.id)
	v, _ = call("alice", "GET", path+"/"+incomplete.id, "", "", nil, 200)
	if v["completion_unconfirmed"] != true || v["outcome"] != "admitted" {
		t.Fatal("crash was mislabeled as failure/completion")
	}
	if _, e = s.beginSQLHistory(request, record{"branch_id": "br_foreign"}, "single", "example_role", "example_db", "SELECT 2"); e == nil {
		t.Fatal("foreign endpoint identity admitted")
	}
	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "false")
	v, _ = call("alice", "GET", path, "", "", nil, 200)
	if v["enabled"] != false {
		t.Fatal("operator opt-out missing")
	}
	history, e := s.beginSQLHistory(request, item, "single", "example_role", "example_db", "SELECT 2")
	if e != nil || history != nil {
		t.Fatal("disabled history changed SQL admission")
	}
}
func TestSQLHistoryUTF8AndOutcomeClassification(t *testing.T) {
	plain := "SELECT '中文'"
	value, truncated := sqlHistoryText(plain)
	if value != plain || truncated {
		t.Fatal("short SQL changed")
	}
	value, truncated = sqlHistoryText(strings.Repeat("中", 4000))
	if !truncated || len(value) > sqlHistoryLimit || !strings.HasPrefix(value, "-- QUERY TRUNCATED") {
		t.Fatal("bounded SQL failed")
	}
	if !json.Valid([]byte(`"` + strings.ReplaceAll(value, "\n", `\n`) + `"`)) {
		t.Fatal("invalid UTF-8 truncation")
	}
	outcome, _ := sqlHistoryFailure(sqlExecutionError{stage: "connect", cause: context.DeadlineExceeded})
	if outcome != "not_submitted" {
		t.Fatal("connection failure claimed submitted SQL")
	}
	outcome, _ = sqlHistoryFailure(sqlExecutionError{stage: "query", cause: context.DeadlineExceeded})
	if outcome != "outcome_unknown" {
		t.Fatal("ambiguous outcome was replayable/completed")
	}
}
