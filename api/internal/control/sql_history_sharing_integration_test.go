package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSQLHistorySharingConsentIsolationRevocationAndLifecycle(t *testing.T) {
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
	schema := "v2_history_sharing_" + randomToken(6)
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
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-token"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_history_sharing','history-sharing','History sharing')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_history_sharing','alice','editor'),('org_history_sharing','bob','editor'),('org_history_sharing','viewer','viewer')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_history_sharing','org_history_sharing','History sharing','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_history_sharing','prj_history_sharing','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_history_sharing','prj_history_sharing','br_history_sharing','ep-history-sharing','VirtualMachine','history-sharing','history-sharing','suspended','history_role','history_db',now(),now())`)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{4}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{3}, 32)}}}
	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "true")
	const privateSQL = "SELECT 'explicit-shared-private-history-中文'"
	admit := func(owner, outcome string) string {
		t.Helper()
		r := httptest.NewRequest("POST", "/query", nil)
		r.SetPathValue("project", "prj_history_sharing")
		r.SetPathValue("endpoint", "ep_history_sharing")
		r = r.WithContext(context.WithValue(ctx, userKey, user{ID: owner}))
		h, e := s.beginSQLHistory(r, record{"branch_id": "br_history_sharing"}, "single", "history_role", "history_db", privateSQL)
		if e != nil || h == nil {
			t.Fatal("history admission", e)
		}
		if outcome != "admitted" {
			h.finish(httptest.NewRecorder(), outcome, "")
		}
		return h.id
	}
	id, unknown, pending := admit("alice", "completed"), admit("alice", "outcome_unknown"), admit("alice", "admitted")
	foreign := admit("bob", "completed")
	personal := "/api/v1/projects/prj_history_sharing/query-history/"
	shared := "/api/v1/projects/prj_history_sharing/shared-query-history"
	schemas := loadOpenAPI(t)["components"].(map[string]any)["schemas"].(map[string]any)
	checkShape := func(name string, value map[string]any) {
		t.Helper()
		schema := schemas[name].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Fatalf("History contract must declare its closed fields: %s", name)
		}
		properties := schema["properties"].(map[string]any)
		for field := range value {
			if properties[field] == nil {
				t.Fatalf("Actual HTTP field missing from %s contract: %s", name, field)
			}
		}
		for _, raw := range schema["required"].([]any) {
			if _, ok := value[raw.(string)]; !ok {
				t.Fatalf("Actual HTTP response missing %s.%s", name, raw)
			}
		}
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
			t.Fatalf("%s expected %d got %d: %s", method, want, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("Shared response cacheable")
		}
		var value record
		_ = json.Unmarshal(w.Body.Bytes(), &value)
		if method == "GET" && want == 200 && value["id"] != nil {
			name := "SQLHistoryDetail"
			if strings.HasPrefix(path, shared+"/") {
				name = "SharedSQLHistoryDetail"
			}
			checkShape(name, value)
		}
		if method == "GET" && want == 200 && strings.HasPrefix(path, shared) && value["items"] != nil {
			for _, item := range value["items"].([]any) {
				checkShape("SharedSQLHistoryEntry", item.(map[string]any))
			}
		}
		return value
	}
	list := func(actor string) record { return call(actor, "GET", shared, "", "", nil, 200) }
	policy := personal + id + "/sharing"
	yes := record{"shared": true, "acknowledge_sensitive_sql_sharing": true}
	no := record{"shared": false}
	if len(list("bob")["items"].([]any)) != 0 {
		t.Fatal("History shared by default")
	}
	call("bob", "GET", shared+"/"+id, "", "", nil, 404)
	call("bob", "GET", policy, "", "", nil, 404)
	call("viewer", "GET", shared, "", "", nil, 403)
	call("outsider", "GET", shared, "", "", nil, 404)
	call("alice", "PUT", policy, "share-history-original", `"0"`, record{"shared": true}, 422)
	call("alice", "PUT", policy, "share-history-original", "", yes, 428)
	call("bob", "PUT", policy, "share-history-foreign", `"0"`, yes, 404)
	call("alice", "PUT", personal+unknown+"/sharing", "share-history-unknown", `"0"`, yes, 409)
	call("alice", "PUT", personal+pending+"/sharing", "share-history-pending", `"0"`, yes, 409)
	// Concurrent copies of the same durable request must admit one generation
	// and one audit, using PG state rather than a process-local mutex.
	var concurrent sync.WaitGroup
	values := make(chan record, 2)
	for range 2 {
		concurrent.Add(1)
		go func() {
			defer concurrent.Done()
			values <- call("alice", "PUT", policy, "share-history-original", `"0"`, yes, 200)
		}()
	}
	concurrent.Wait()
	close(values)
	var value record
	for next := range values {
		if next["version_token"] != "1" {
			t.Fatal("Concurrent sharing repeated admission")
		}
		value = next
	}
	var audits int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='share_sql_history' AND resource_id=$1`, id).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("Concurrent sharing duplicated audit", audits, err)
	}
	if value["is_shared"] != true || value["version_token"] != "1" {
		t.Fatal("Consent not committed")
	}
	call("alice", "PUT", policy, "share-history-original", `"0"`, yes, 200)
	call("alice", "PUT", policy, "share-history-original", `"0"`, no, 409)
	call("alice", "PUT", policy, "share-history-stale", `"0"`, no, 412)
	call("bob", "GET", personal+id, "", "", nil, 404)
	visible := list("bob")["items"].([]any)
	if len(visible) != 1 {
		t.Fatal("Shared listing lost entry")
	}
	metadata := visible[0].(map[string]any)
	if metadata["sql"] != nil || metadata["sealed_sql"] != nil || metadata["key_version"] != nil || metadata["owner_username"] != "alice" {
		t.Fatal("Metadata leaked text/envelope or wrong owner")
	}
	if call("bob", "GET", shared+"/"+id, "", "", nil, 200)["sql"] != privateSQL {
		t.Fatal("Shared read did not use original owner AAD")
	}
	call("alice", "GET", shared+"/"+foreign, "", "", nil, 404)
	ring := s.backendKeys
	s.backendKeys = nil
	call("bob", "GET", shared+"/"+id, "", "", nil, 503)
	s.backendKeys = ring
	call("alice", "PUT", policy, "share-history-revoke", `"1"`, no, 200)
	call("bob", "GET", shared+"/"+id, "", "", nil, 404)
	if call("alice", "GET", personal+id, "", "", nil, 200)["sql"] != privateSQL {
		t.Fatal("Revocation changed original text")
	}
	// A replay returns the original receipt but must never re-enable sharing.
	call("alice", "PUT", policy, "share-history-original", `"0"`, yes, 200)
	call("bob", "GET", shared+"/"+id, "", "", nil, 404)
	call("alice", "PUT", policy, "share-history-enable-again", `"2"`, yes, 200)
	exec(`UPDATE users SET disabled=true WHERE id='alice'`)
	call("bob", "GET", shared+"/"+id, "", "", nil, 404)
	exec(`UPDATE users SET disabled=false WHERE id='alice'`)
	exec(`UPDATE organization_members SET role='viewer' WHERE user_id='alice'`)
	call("bob", "GET", shared+"/"+id, "", "", nil, 404)
	exec(`INSERT INTO project_grants(project_id,user_id,role) VALUES('prj_history_sharing','alice','editor')`)
	call("bob", "GET", shared+"/"+id, "", "", nil, 200)
	exec(`DELETE FROM project_grants WHERE user_id='alice'`)
	call("bob", "GET", shared+"/"+id, "", "", nil, 404)
	exec(`UPDATE organization_members SET role='editor' WHERE user_id='alice'`)
	exec(`UPDATE organization_members SET role='viewer' WHERE user_id='bob'`)
	call("bob", "GET", shared+"/"+id, "", "", nil, 403)
	exec(`UPDATE organization_members SET role='editor' WHERE user_id='bob'`)
	exec(`CREATE FUNCTION reject_share_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='revoke_sql_history_sharing' THEN RAISE EXCEPTION 'injected audit failure'; END IF;RETURN NEW;END $$`)
	exec(`CREATE TRIGGER reject_share_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_share_audit()`)
	call("alice", "PUT", policy, "share-audit-rollback", `"3"`, no, 503)
	call("bob", "GET", shared+"/"+id, "", "", nil, 200)
	exec(`DROP TRIGGER reject_share_audit ON audit_events`)
	call("alice", "PUT", policy, "share-audit-rollback", `"3"`, no, 200)
	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "false")
	call("bob", "GET", shared+"/"+id, "", "", nil, 404)
	if list("bob")["enabled"] != false {
		t.Fatal("Operator opt-out ignored")
	}
	call("alice", "PUT", policy, "share-disabled-enable", `"4"`, yes, 503)
	call("alice", "PUT", policy, "share-disabled-revoke", `"4"`, no, 200)
	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "true")
	// Every Console-only shared handler independently rejects API identities.
	for _, handler := range []http.HandlerFunc{s.sharedSQLHistory, s.sqlHistorySharing} {
		r := httptest.NewRequest("GET", shared, nil)
		r = r.WithContext(context.WithValue(ctx, userKey, user{ID: "bob", APIKey: true}))
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != 403 {
			t.Fatal("API key read shared SQL")
		}
	}
	ids := []string{}
	for i := 0; i < 30; i++ {
		next := admit("alice", "completed")
		ids = append(ids, next)
		call("alice", "PUT", personal+next+"/sharing", "share-pagination-"+next, `"0"`, yes, 200)
	}
	page := list("bob")
	if len(page["items"].([]any)) != 25 || page["next_cursor"] == nil {
		t.Fatal("Shared keyset page incomplete")
	}
	cursor := page["next_cursor"].(string)
	next := call("bob", "GET", shared+"?cursor="+cursor, "", "", nil, 200)
	if len(next["items"].([]any)) != 5 {
		t.Fatal("Shared keyset continuation incorrect")
	}
	call("alice", "GET", shared+"?cursor="+cursor, "", "", nil, 422)
	call("alice", "PUT", policy, "share-before-delete", `"5"`, yes, 200)
	call("alice", "DELETE", personal+id+"?acknowledge_history_deletion=true", "", "", nil, 204)
	call("bob", "GET", shared+"/"+id, "", "", nil, 404)
	var receipts int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM sql_history_sharing_requests WHERE history_id=$1`, id).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("Deletion retained sharing receipts", err)
	}
	exec(`INSERT INTO sql_history_preferences(owner_id,capture_enabled,retain_days,generation) VALUES('alice',true,7,1)`)
	for _, next := range ids {
		exec(`UPDATE sql_query_history SET started_at=now()-interval '10 days' WHERE id=$1`, next)
	}
	if count, e := s.pruneSQLHistory(ctx, "alice", "sharing-retention"); e != nil || count != 30 {
		t.Fatal("Retention did not revoke shared history atomically", count, e)
	}
	if len(list("bob")["items"].([]any)) != 0 {
		t.Fatal("Deleted shared references still exposed")
	}
	call("alice", "GET", personal+unknown, "", "", nil, 200)
	call("alice", "GET", personal+pending, "", "", nil, 200)
}
