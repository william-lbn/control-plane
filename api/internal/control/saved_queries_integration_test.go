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

func TestSavedQueriesIntegration(t *testing.T) {
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
	base := "/api/v1/projects/prj_saved/saved-queries"
	sql := "SELECT 'private-template';\nSELECT 2;"
	spec := savedQuerySpec{Name: "draft", SQL: sql}
	v, w := call("alice", "POST", base, "create-key-1", "", spec, 201)
	if strings.Contains(w.Body.String(), "private-template") {
		t.Fatal("SQL in mutation reply")
	}
	id := v["query"].(map[string]any)["id"].(string)
	_, w = call("alice", "POST", base, "create-key-1", "", spec, 200)
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("creation replay missing")
	}
	call("alice", "POST", base, "create-key-1", "", savedQuerySpec{Name: "changed", SQL: sql}, 409)
	v, _ = call("alice", "GET", base+"/"+id, "", "", nil, 200)
	if v["sql"] != sql {
		t.Fatal("saved draft changed")
	}
	call("bob", "GET", base+"/"+id, "", "", nil, 404)
	call("viewer", "GET", base, "", "", nil, 403)
	call("outsider", "GET", base, "", "", nil, 404)
	var sealed []byte
	if db.QueryRow(ctx, `SELECT sealed_sql FROM saved_queries WHERE id=$1`, id).Scan(&sealed) != nil || bytes.Contains(sealed, []byte("private-template")) {
		t.Fatal("plaintext SQL in metadata")
	}
	var replay []byte
	if db.QueryRow(ctx, `SELECT response FROM saved_query_requests WHERE owner_id='alice'`).Scan(&replay) != nil || bytes.Contains(replay, []byte("private-template")) {
		t.Fatal("plaintext SQL in replay")
	}
	s.backendKeys.Active = "v2"
	_, w = call("alice", "POST", base, "create-key-1", "", spec, 200)
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("pepper activation broke durable replay")
	}
	call("alice", "PATCH", base+"/"+id, "update-key-1", "", spec, 428)
	call("alice", "PATCH", base+"/"+id, "update-key-2", `"9"`, spec, 412)
	updated := savedQuerySpec{Name: "renamed", SQL: "SELECT 3;"}
	call("alice", "PATCH", base+"/"+id, "update-key-3", `"1"`, updated, 200)
	call("alice", "PATCH", base+"/"+id, "update-key-3", `"1"`, updated, 200)
	call("bob", "DELETE", base+"/"+id, "delete-key-bob", `"2"`, nil, 404)
	v, _ = call("alice", "GET", base+"/"+id, "", "", nil, 200)
	if v["sql"] != "SELECT 3;" || v["generation"] != float64(2) {
		t.Fatal("versioned update missing")
	}
	call("alice", "DELETE", base+"/"+id, "delete-key-1", `"2"`, nil, 200)
	call("alice", "DELETE", base+"/"+id, "delete-key-1", `"2"`, nil, 200)
	call("alice", "GET", base+"/"+id, "", "", nil, 404)
	var audit int
	if db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_type='saved_query'`).Scan(&audit) != nil || audit != 3 {
		t.Fatal("replays duplicated audit side effects")
	}
}
