package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFunctionSchedulesDurabilityIsolationAndDispatchRecovery(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_trigger_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 12
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
	reject := func(q string, args ...any) {
		t.Helper()
		_, e := db.Exec(ctx, q, args...)
		var p *pgconn.PgError
		if !errors.As(e, &p) || p.Code != "23514" {
			t.Fatal("database invariant failed", e)
		}
	}
	const project = "prj_0123456789abcdef"
	const branch = "br_0123456789abcdef"
	const fn = "fnc_0123456789abcdef"
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('schedule_actor','schedule_actor','unused','owner'),('schedule_viewer','schedule_viewer','unused','viewer')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('schedule_org','schedule-org','Schedules')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('schedule_org','schedule_actor','admin'),('schedule_org','schedule_viewer','viewer')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'schedule_org','Schedule','lab',16,'ready','managed',now(),now())`, project)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now()),('br_abcdef0123456789',$2,'child','ready',now())`, branch, project)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('schedule_writer',$1,$2,'schedule_writer','neonvm','schedule_writer','schedule_writer','suspended','cloud_admin','neondb',now(),now())`, project, branch)
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'schedule_org',$2,$3,'schedule_writer','hello','neondb','app','fn_0123456789abcdef','suspended')`, fn, project, branch)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('schedule_op',$1,'function',$2,'deploy_function','queued','schedule_actor','schedule_ci','{}')`, project, fn)
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id,state,finished_at) VALUES('fdp_0123456789abcdef',$1,$2,$3,'nodejs24',$4,100,'index.mjs','functions/schedule','env-schedule','schedule_actor','schedule_op','completed',now())`, fn, project, branch, strings.Repeat("a", 64))
	exec(`UPDATE function_definitions SET active_deployment_id='fdp_0123456789abcdef' WHERE id=$1`, fn)
	exec(`INSERT INTO function_http_policies(function_id,public_enabled,idle_enabled,idle_suspended,actor_id) VALUES($1,true,true,true,'schedule_actor')`, fn)
	for _, actor := range []string{"schedule_actor", "schedule_viewer"} {
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{7}, 32), storage: &storageBlobs{}, functions: &functionsPlatform{key: bytes.Repeat([]byte{5}, 32)}}
	t.Setenv("NEON_FUNCTION_SCHEDULES_ENABLED", "true")
	t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", "http://functions.invalid:8792")
	if _, err = s.awaitFunctionIdentity(ctx, branch, "hello", "fnc_ffffffffffffffff"); err != errFunctionInstanceConflict {
		t.Fatal("queued trigger adopted replacement slug", err)
	}
	originalIdentity, err := s.functionHTTPIdentity(ctx, branch, "hello")
	if err != nil {
		t.Fatal(err)
	}
	originalIdentity.ID = "fnc_ffffffffffffffff"
	if s.automaticFunctionIntent(ctx, originalIdentity, true) || s.automaticFunctionIntent(ctx, originalIdentity, false) {
		t.Fatal("automatic intent crossed original Function identity")
	}
	mux := s.routes()
	call := func(method, id, key, version, body, actor, br string, want int) *httptest.ResponseRecorder {
		t.Helper()
		path := "/api/v1/projects/" + project + "/branches/" + br + "/functions-triggers"
		if id != "" {
			path += "/" + id
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Cookie", "neon_v2_session="+actor+"-session; neon_v2_csrf="+actor+"-csrf")
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if version != "" {
			r.Header.Set("If-Match", version)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s status %d expected %d: %s", method, id, w.Code, want, w.Body.String())
		}
		return w
	}
	input := `{"type":"schedule","name":"minute audit","function_slug":"hello","function_path":"/scheduled","schedule":{"cron":"* * * * *"},"enabled":false}`
	call("POST", "", "schedule-viewer-0001", "", input, "schedule_viewer", branch, 403)
	first := call("POST", "", "schedule-create-0001", "", input, "schedule_actor", branch, 201)
	var result struct {
		Trigger functionTriggerView `json:"trigger"`
	}
	if json.Unmarshal(first.Body.Bytes(), &result) != nil || !functionTriggerID.MatchString(result.Trigger.ID) || result.Trigger.Version != "1" || result.Trigger.Next != nil {
		t.Fatal("invalid trigger representation")
	}
	id := result.Trigger.ID
	replay := call("POST", "", "schedule-create-0001", "", input, "schedule_actor", branch, 201)
	if replay.Body.String() != first.Body.String() {
		t.Fatal("durable replay changed")
	}
	call("POST", "", "schedule-create-0001", "", strings.Replace(input, "minute audit", "other", 1), "schedule_actor", branch, 409)
	call("POST", "", "schedule-duplicate-01", "", input, "schedule_actor", branch, 409)
	call("PATCH", id, "schedule-stale-00001", `"2"`, `{"type":"schedule","enabled":true}`, "schedule_actor", branch, 412)
	call("PATCH", id, "schedule-unquoted-01", "1", `{"type":"schedule","enabled":true}`, "schedule_actor", branch, 422)
	call("PATCH", id, "schedule-unknown-001", `"1"`, `{"type":"schedule","schedule":{"cron":"* * * * *","timezone":"UTC"}}`, "schedule_actor", branch, 422)
	call("PATCH", id, "schedule-badpath-001", `"1"`, `{"type":"schedule","function_path":"/bad?x=1"}`, "schedule_actor", branch, 422)
	call("PATCH", id, "schedule-null-000001", `"1"`, `{"type":"schedule","enabled":null}`, "schedule_actor", branch, 422)
	call("GET", id, "", "", "", "schedule_actor", "br_abcdef0123456789", 404)
	call("HEAD", id, "", "", "", "schedule_actor", branch, 200)
	call("PATCH", id, "schedule-enable-001", `"1"`, `{"type":"schedule","enabled":true}`, "schedule_actor", branch, 200)
	var at time.Time
	var version int64
	if db.QueryRow(ctx, `SELECT next_run_at,generation FROM function_triggers WHERE id=$1`, id).Scan(&at, &version) != nil || version != 2 || at.Location() != time.Local && at.Location() != time.UTC || at.Second() != 0 {
		t.Fatal("database UTC future minute missing")
	}
	exec(`UPDATE function_triggers SET next_run_at=date_trunc('minute',clock_timestamp()) WHERE id=$1`, id)
	// Two independent enqueue callers serialize without duplicate occurrences.
	var group sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() { defer group.Done(); errs <- s.enqueueFunctionSchedules(ctx) }()
	}
	group.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var count int
	var original string
	if db.QueryRow(ctx, `SELECT count(*),min(id) FROM function_trigger_invocations WHERE trigger_id=$1`, id).Scan(&count, &original) != nil || count != 1 {
		t.Fatal("occurrence duplicated")
	}
	call("PATCH", id, "schedule-pause-0001", `"2"`, `{"type":"schedule","enabled":false,"name":"new name"}`, "schedule_actor", branch, 200)
	var snap string
	if db.QueryRow(ctx, `SELECT snapshot->>'name' FROM function_trigger_invocations WHERE id=$1`, original).Scan(&snap) != nil || snap != "minute audit" {
		t.Fatal("queued snapshot rewritten")
	}
	reject(`UPDATE function_trigger_invocations SET snapshot='{}' WHERE id=$1`, original)
	run, e := s.claimFunctionTrigger(ctx)
	if e != nil || run == nil || run.ID != original || run.Attempts != 1 {
		t.Fatal("durable claim failed", e)
	}
	// Known pre-dispatch failure may retry; an uncertain dispatched call cannot.
	s.finishFunctionTrigger(*run, "failed", "target_not_ready", 0, 0, true)
	exec(`UPDATE function_trigger_invocations SET next_attempt_at=clock_timestamp() WHERE id=$1`, original)
	run, e = s.claimFunctionTrigger(ctx)
	if e != nil || run == nil || run.Attempts != 2 {
		t.Fatal("bounded pre-dispatch retry failed", e)
	}
	exec(`UPDATE function_trigger_invocations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, original)
	other, e := s.claimFunctionTrigger(ctx)
	if e != nil || other != nil {
		t.Fatal("expired dispatch replayed", e)
	}
	var state string
	if db.QueryRow(ctx, `SELECT state FROM function_trigger_invocations WHERE id=$1`, original).Scan(&state) != nil || state != "outcome_unknown" {
		t.Fatal("lost owner not unknown")
	}
	s.finishFunctionTrigger(*run, "succeeded", "", 200, 0, false)
	reject(`UPDATE function_trigger_invocations SET state='queued',finished_at=NULL WHERE id=$1`, original)
	call("PATCH", id, "schedule-enable-002", `"3"`, `{"type":"schedule","enabled":true}`, "schedule_actor", branch, 200)
	exec(`UPDATE function_triggers SET next_run_at=date_trunc('minute',clock_timestamp())-interval '5 minutes' WHERE id=$1`, id)
	if e = s.enqueueFunctionSchedules(ctx); e != nil {
		t.Fatal(e)
	}
	if db.QueryRow(ctx, `SELECT count(*) FROM function_trigger_invocations WHERE trigger_id=$1 AND state='skipped' AND error_code='schedule_misfire'`, id).Scan(&count) != nil || count != 1 {
		t.Fatal("stale catchup not skipped")
	}
	// One future occurrence with a revoked creator fails closed without touching
	// Kubernetes or a Function guest, even though its HTTP policy still exists.
	exec(`UPDATE function_triggers SET next_run_at=date_trunc('minute',clock_timestamp())-interval '1 minute' WHERE id=$1`, id)
	if e = s.enqueueFunctionSchedules(ctx); e != nil {
		t.Fatal(e)
	}
	run, e = s.claimFunctionTrigger(ctx)
	if e != nil || run == nil {
		t.Fatal("revocation occurrence missing", e)
	}
	exec(`UPDATE organization_members SET role='viewer' WHERE user_id='schedule_actor'`)
	s.dispatchFunctionTrigger(ctx, *run)
	if db.QueryRow(ctx, `SELECT state FROM function_trigger_invocations WHERE id=$1`, run.ID).Scan(&state) != nil || state != "skipped" {
		t.Fatal("revoked actor admitted")
	}
	exec(`UPDATE organization_members SET role='admin' WHERE user_id='schedule_actor'`)
	call("GET", id+"/invocations?limit=1", "", "", "", "schedule_actor", branch, 200)
	call("DELETE", id, "schedule-delete-001", `"4"`, "", "schedule_actor", branch, 204)
	call("DELETE", id, "schedule-delete-001", `"4"`, "", "schedule_actor", branch, 204)
	call("GET", id, "", "", "", "schedule_actor", branch, 404)
	call("GET", id+"/invocations", "", "", "", "schedule_actor", branch, 200)
	// A replay of old creation is a receipt, never resurrection of the plan.
	call("POST", "", "schedule-create-0001", "", input, "schedule_actor", branch, 201)
	if db.QueryRow(ctx, `SELECT count(*) FROM function_triggers WHERE id=$1 AND state='ready'`, id).Scan(&count) != nil || count != 0 {
		t.Fatal("replay resurrected plan")
	}
	// Admission must work with only its transaction connection available. This
	// detects a nested pool borrow while branch/configuration locks are held.
	oneConfig := cfg.Copy()
	oneConfig.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, oneConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	s.db = one
	singleCtx, done := context.WithTimeout(ctx, 4*time.Second)
	defer done()
	r := httptest.NewRequest("POST", "/triggers", strings.NewReader(strings.Replace(input, `"enabled":false`, `"enabled":true`, 1)))
	r = r.WithContext(context.WithValue(singleCtx, userKey, user{ID: "schedule_actor"}))
	r.SetPathValue("project", project)
	r.SetPathValue("branch", branch)
	r.Header.Set("Idempotency-Key", "schedule-single-pool-001")
	w := httptest.NewRecorder()
	s.functionTriggers(w, r)
	s.db = db
	if w.Code != 201 {
		t.Fatalf("single-connection admission failed: %d %s", w.Code, w.Body.String())
	}
}
