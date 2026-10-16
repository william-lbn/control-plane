package control

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSQLHistoryNamesPrivacyConcurrencyAuditAndLifecycle(t *testing.T) {
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
	schema := "v2_history_names_" + randomToken(6)
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
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, actor := range []string{"alice", "bob", "viewer", "outsider"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-token"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_history_names','history-names','History names')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_history_names','alice','editor'),('org_history_names','bob','admin'),('org_history_names','viewer','viewer')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_history_names','org_history_names','History names','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_history_names','prj_history_names','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_history_names','prj_history_names','br_history_names','ep-history-names','VirtualMachine','history-names','history-names','suspended','history_role','history_db',now(),now())`)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{4}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{3}, 32), "v2": bytes.Repeat([]byte{5}, 32)}}}
	t.Setenv("NEON_V2_SQL_HISTORY_ENABLED", "true")
	admit := func(owner, outcome string) string {
		t.Helper()
		r := httptest.NewRequest("POST", "/query", nil)
		r.SetPathValue("project", "prj_history_names")
		r.SetPathValue("endpoint", "ep_history_names")
		r = r.WithContext(context.WithValue(ctx, userKey, user{ID: owner}))
		h, e := s.beginSQLHistory(r, record{"branch_id": "br_history_names"}, "single", "history_role", "history_db", "SELECT 'private literal'")
		if e != nil || h == nil {
			t.Fatal(e)
		}
		if outcome != "admitted" {
			h.finish(httptest.NewRecorder(), outcome, "")
		}
		return h.id
	}
	id, foreign, unknown, pending := admit("alice", "completed"), admit("bob", "completed"), admit("alice", "outcome_unknown"), admit("alice", "admitted")
	base := "/api/v1/projects/prj_history_names/query-history/"
	target := base + id + "/name"
	schemas := loadOpenAPI(t)["components"].(map[string]any)["schemas"].(map[string]any)
	request := func(actor, method, path, key, tag string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-token"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", tag)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	call := func(actor, method, path, key, tag string, body any, want int) record {
		t.Helper()
		w := request(actor, method, path, key, tag, body)
		if w.Code != want {
			t.Fatalf("%s expected %d got %d: %s", method, want, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" && want == 200 {
			t.Fatal("name response cacheable")
		}
		var out record
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if want == 200 && (strings.HasSuffix(path, "/name") || path == base+id) {
			name := "SQLHistoryDetail"
			if strings.HasSuffix(path, "/name") {
				name = "SQLHistoryName"
				if method == "PUT" {
					name = "SQLHistoryNameReceipt"
				}
			}
			shape := schemas[name].(map[string]any)
			if shape["additionalProperties"] != false {
				t.Fatal("name response contract must be closed")
			}
			properties := shape["properties"].(map[string]any)
			for field := range out {
				if properties[field] == nil {
					t.Fatalf("actual response field missing from %s: %s", name, field)
				}
			}
			for _, field := range shape["required"].([]any) {
				if _, ok := out[field.(string)]; !ok {
					t.Fatalf("required response field missing: %s", field)
				}
			}
		}
		return out
	}
	value := call("alice", "GET", target, "", "", nil, 200)
	if value["name"] != nil || value["version_token"] != "0" {
		t.Fatal("unexpected initial name")
	}
	for _, handler := range []http.HandlerFunc{s.sqlHistoryName, s.sqlHistory} {
		r := httptest.NewRequest("GET", target, nil)
		r.SetPathValue("project", "prj_history_names")
		r.SetPathValue("history", id)
		r = r.WithContext(context.WithValue(ctx, userKey, user{ID: "alice", APIKey: true}))
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != 403 {
			t.Fatal("API key reached private personal history")
		}
	}
	body := record{"name": "Private 查询 annotation"}
	call("bob", "GET", target, "", "", nil, 404)
	call("alice", "GET", base+foreign+"/name", "", "", nil, 404)
	call("viewer", "PUT", target, "name-viewer", `"0"`, body, 403)
	call("outsider", "GET", target, "", "", nil, 404)
	call("alice", "PUT", target, "name-no-version", "", body, 428)
	call("alice", "PUT", target, "name-bad-version", `"00"`, body, 422)
	call("alice", "PUT", target, "name-bad-payload", `"0"`, record{"name": "bad\nname"}, 422)
	call("alice", "PUT", base+unknown+"/name", "name-unknown", `"0"`, body, 409)
	call("alice", "PUT", base+pending+"/name", "name-pending", `"0"`, body, 409)
	exec(`ALTER TABLE audit_events ADD CONSTRAINT reject_name_audit CHECK(action<>'name_sql_history')`)
	call("alice", "PUT", target, "name-audit-failure", `"0"`, body, 503)
	if v := call("alice", "GET", target, "", "", nil, 200); v["version_token"] != "0" || v["name"] != nil {
		t.Fatal("failed audit published a name")
	}
	exec(`ALTER TABLE audit_events DROP CONSTRAINT reject_name_audit`)
	first := request("alice", "PUT", target, "name-audit-failure", `"0"`, body)
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	replay := request("alice", "PUT", target, "name-audit-failure", `"0"`, body)
	if replay.Code != 200 || first.Body.String() != replay.Body.String() || first.Header().Get("ETag") != `"1"` {
		t.Fatal("stable original receipt lost")
	}
	if strings.Contains(first.Body.String(), body["name"].(string)) {
		t.Fatal("plaintext name in receipt")
	}
	call("alice", "PUT", target, "name-audit-failure", `"0"`, record{"name": "changed"}, 409)
	call("alice", "PUT", target, "name-stale-version", `"0"`, body, 412)
	var raw []byte
	var keyVersion, receiptText string
	if err = db.QueryRow(ctx, `SELECT sealed_name,name_key_version,(SELECT response::text FROM sql_history_name_requests WHERE history_id=$1 LIMIT 1) FROM sql_query_history WHERE id=$1`, id).Scan(&raw, &keyVersion, &receiptText); err != nil || keyVersion != "v1" || bytes.Contains(raw, []byte("Private")) || strings.Contains(receiptText, "annotation") {
		t.Fatal("private name not encrypted or receipt leaked", err)
	}
	if v := call("alice", "GET", target, "", "", nil, 200); v["name"] != body["name"] {
		t.Fatal("encrypted name unavailable")
	}
	details := call("alice", "GET", base+id, "", "", nil, 200)
	if details["name"] != body["name"] || details["name_key_version"] != nil || details["sealed_name"] != nil {
		t.Fatal("personal detail projection leaked envelope")
	}
	// Shared SQL visibility does not grant access to its owner's private labels.
	call("alice", "PUT", base+id+"/sharing", "name-share-source", `"0"`, record{"shared": true, "acknowledge_sensitive_sql_sharing": true}, 200)
	shared := call("bob", "GET", "/api/v1/projects/prj_history_names/shared-query-history/"+id, "", "", nil, 200)
	if _, ok := shared["name"]; ok {
		t.Fatal("private name copied into shared history")
	}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, name := range []string{"Concurrent A", "Concurrent B"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			results <- request("alice", "PUT", target, "name-concurrent-"+n, `"1"`, record{"name": n}).Code
		}(name)
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[200] != 1 || counts[412] != 1 {
		t.Fatal("generation race admitted stale update", counts)
	}
	s.backendKeys.Active = "v2"
	call("alice", "GET", target, "", "", nil, 200)
	old := s.backendKeys.decoded["v1"]
	delete(s.backendKeys.decoded, "v1")
	call("alice", "GET", target, "", "", nil, 503)
	s.backendKeys.decoded["v1"] = old
	cleared := call("alice", "PUT", target, "name-clear-current", `"2"`, record{"name": ""}, 200)
	if cleared["has_name"] != false || cleared["version_token"] != "3" {
		t.Fatal("clear did not preserve generation")
	}
	for n := 3; n < 60; n++ {
		call("alice", "PUT", target, "name-rate-"+strconv.Itoa(n), `"`+strconv.Itoa(n)+`"`, record{"name": "Bounded label"}, 200)
	}
	call("alice", "PUT", target, "name-rate-reject", `"60"`, body, 429)
	call("alice", "PUT", target, "name-audit-failure", `"0"`, body, 200)
	if current := call("alice", "GET", target, "", "", nil, 200); current["version_token"] != "60" || current["name"] != "Bounded label" {
		t.Fatal("old receipt reapplied an obsolete annotation")
	}
	// A database-level composite FK forbids misbound owner receipts.
	if _, err = db.Exec(ctx, `INSERT INTO sql_history_name_requests(owner_id,history_id,key_hash,request_hash,response) VALUES('bob',$1,'foreign','foreign','{}')`, id); err == nil {
		t.Fatal("foreign owner receipt admitted")
	}
	call("alice", "DELETE", base+id+"?acknowledge_history_deletion=true", "", "", nil, 204)
	var count int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM sql_history_name_requests WHERE history_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted annotation receipts retained", err)
	}
	call("alice", "GET", target, "", "", nil, 404)
}
