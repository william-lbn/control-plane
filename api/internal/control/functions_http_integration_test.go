package control

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestFunctionHTTPPolicyAndDistributedAdmission(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, e := pgxpool.ParseConfig(dsn)
	if dsn == "" || e != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "v2_function_http_" + randomToken(6)
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 10
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e = db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('http_actor','http_actor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('http_org','http-org','HTTP')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('http_org','http_actor','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_0123456789abcdef','http_org','HTTP','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_0123456789abcdef','prj_0123456789abcdef','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('http_writer','prj_0123456789abcdef','br_0123456789abcdef','http_writer','neonvm','http_writer','http_writer','suspended','cloud_admin','neondb',now(),now())`)
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES('fnc_0123456789abcdef','http_org','prj_0123456789abcdef','br_0123456789abcdef','http_writer','hello','neondb','app','fn_0123456789abcdef','suspended')`)
	s := &server{db: db, storage: &storageBlobs{}, functions: &functionsPlatform{key: bytes.Repeat([]byte{3}, 32)}}
	t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", "http://functions.invalid:8792")
	call := func(key, version string, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("PATCH", "/api/v1/projects/prj_0123456789abcdef/branches/br_0123456789abcdef/functions/hello/http-policy", bytes.NewBufferString(body))
		r = r.WithContext(context.WithValue(ctx, userKey, user{ID: "http_actor"}))
		r.SetPathValue("project", "prj_0123456789abcdef")
		r.SetPathValue("branch", "br_0123456789abcdef")
		r.SetPathValue("slug", "hello")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", version)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.functionHTTPPolicy(w, r)
		if w.Code != want {
			t.Fatalf("status %d wanted %d: %s", w.Code, want, w.Body.String())
		}
		return w
	}
	policy := `{"public_enabled":true,"idle_enabled":true,"idle_timeout_seconds":30,"acknowledge_public_access_and_background_eviction":true}`
	first := call("policy-original-0001", "1", policy, 200)
	replay := call("policy-original-0001", "1", policy, 200)
	var a, b map[string]any
	json.Unmarshal(first.Body.Bytes(), &a)
	json.Unmarshal(replay.Body.Bytes(), &b)
	if a["version_token"] != "2" || b["version_token"] != "2" {
		t.Fatal("policy replay changed version")
	}
	call("policy-original-0001", "2", policy, 409)
	call("policy-stale-0000002", "1", policy, 412)
	call("policy-no-ack-000003", "2", `{"public_enabled":true,"idle_enabled":true,"idle_timeout_seconds":30}`, 422)
	p, e := s.functionHTTPIdentity(ctx, "br_0123456789abcdef", "hello")
	if e != nil || !p.Idle || p.IdleSuspended {
		t.Fatal("public policy identity or manual stop marker", e)
	}
	read1, ok := s.functionAdmissionLock(ctx, p.ID, false)
	if !ok {
		t.Fatal("first shared admission")
	}
	read2, ok := s.functionAdmissionLock(ctx, p.ID, false)
	if !ok {
		t.Fatal("second API shared admission")
	}
	if f, ok := s.functionAdmissionLock(ctx, p.ID, true); ok {
		f()
		t.Fatal("idle retirement crossed active invocation")
	}
	read1()
	read2()
	exclusive, ok := s.functionAdmissionLock(ctx, p.ID, true)
	if !ok {
		t.Fatal("idle retirement did not acquire after streams ended")
	}
	if f, ok := s.functionAdmissionLock(ctx, p.ID, false); ok {
		f()
		t.Fatal("invocation crossed idle retirement")
	}
	exclusive()
	exec(`UPDATE users SET disabled=true WHERE id='http_actor'`)
	if _, e = s.functionHTTPIdentity(ctx, p.Branch, p.Slug); e == nil {
		t.Fatal("disabled policy owner still exposes function")
	}
}
