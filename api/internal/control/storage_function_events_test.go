package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type storageEventFixture struct {
	s        *server
	db       *pgxpool.Pool
	native   *pgx.Conn
	ctx      context.Context
	p        objectStoragePayload
	timeline string
	trigger  string
}

func newStorageEventFixture(t *testing.T) *storageEventFixture {
	t.Helper()
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || dsn == "" || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "v2_object_events_" + randomToken(5)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 12
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const project = "prj_1123456789abcdef"
	const branch = "br_1123456789abcdef"
	const fn = "fnc_1123456789abcdef"
	const timeline = "1123456789abcdef1123456789abcdef"
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('event_actor','event_actor','unused','owner'),('event_viewer','event_viewer','unused','viewer')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('event_org','event-org','Events')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('event_org','event_actor','admin'),('event_org','event_viewer','viewer')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'event_org','Events','lab',16,'ready','managed',now(),now())`, project)
	exec(`INSERT INTO branches(id,project_id,name,state,timeline_id,created_at) VALUES($1,$2,'main','ready',$3,now()),('br_2123456789abcdef',$2,'child','ready','2123456789abcdef2123456789abcdef',now())`, branch, project, timeline)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('event_writer',$1,$2,'event_writer','neonvm','event_writer','event_writer','suspended','cloud_admin','neondb',now(),now())`, project, branch)
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'event_org',$2,$3,'event_writer','onupload','neondb','app','fn_1123456789abcdef','suspended')`, fn, project, branch)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('event_deploy',$1,'function',$2,'deploy_function','queued','event_actor','event_deploy_ci','{}')`, project, fn)
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id,state,finished_at) VALUES('fdp_1123456789abcdef',$1,$2,$3,'nodejs24',$4,100,'index.mjs','functions/events','env-events','event_actor','event_deploy','completed',now())`, fn, project, branch, strings.Repeat("a", 64))
	exec(`UPDATE function_definitions SET active_deployment_id='fdp_1123456789abcdef' WHERE id=$1`, fn)
	exec(`INSERT INTO function_instances(id,function_id,deployment_id,project_id,branch_id,generation,vm_name,service_name,bootstrap_secret_ref,state,retired_at) VALUES('fni_1123456789abcdef',$1,'fdp_1123456789abcdef',$2,$3,1,'fn-1123456789abcdef','fn-1123456789abcdef','fn-1123456789abcdef-bootstrap','retired',now())`, fn, project, branch)
	exec(`INSERT INTO function_http_policies(function_id,public_enabled,idle_enabled,idle_suspended,actor_id) VALUES($1,true,true,true,'event_actor')`, fn)
	for _, actor := range []string{"event_actor", "event_viewer"} {
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	nativeName := "object_events_" + randomToken(5)
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+nativeName); err != nil {
		t.Fatal(err)
	}
	nativeCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	nativeCfg.Database = nativeName
	native, err := pgx.ConnectConfig(ctx, nativeCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { native.Close(context.Background()) })
	p := objectStoragePayload{ProjectID: project, BranchID: branch, EndpointID: "event_writer", Generation: 1, Spec: ObjectStorageSpec{Database: nativeName}}
	tx, err := native.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyObjectStorageSQLTx(ctx, tx, p); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = native.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('event-bucket','private')`); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(p.Spec)
	exec(`INSERT INTO object_storage_instances(branch_id,project_id,endpoint_id,generation,state,spec) VALUES($1,$2,$3,1,'active',$4)`, branch, project, p.EndpointID, spec)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{7}, 32), storage: &storageBlobs{}, functions: &functionsPlatform{key: bytes.Repeat([]byte{5}, 32)}}
	s.functions.input.KeyVersion = "event-ci-v1"
	t.Setenv("NEON_FUNCTION_OBJECT_EVENTS_ENABLED", "true")
	t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", "http://functions.invalid:8792")
	return &storageEventFixture{s: s, db: db, native: native, ctx: ctx, p: p, timeline: timeline}
}
func (f *storageEventFixture) call(t *testing.T, method, suffix, key, version, body, actor, branch string, want int) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/projects/" + f.p.ProjectID + "/branches/" + branch + "/functions-triggers" + suffix
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Cookie", "neon_v2_session="+actor+"-session; neon_v2_csrf="+actor+"-csrf")
	r.Header.Set("X-CSRF-Token", actor+"-csrf")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	if version != "" {
		r.Header.Set("If-Match", version)
	}
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: %d want %d %s", method, suffix, w.Code, want, w.Body.String())
	}
	return w
}
func (f *storageEventFixture) create(t *testing.T) string {
	t.Helper()
	input := `{"type":"storage_object_created","name":"new uploads","function_slug":"onupload","function_path":"/uploaded","storage_object_created":{"bucket_name":"event-bucket","prefix":"uploads/"},"enabled":true,"acknowledge_at_least_once":true}`
	f.call(t, "POST", "", "event-create-viewer", "", input, "event_viewer", f.p.BranchID, 403)
	f.call(t, "POST", "", "event-create-noack", "", strings.Replace(input, `"acknowledge_at_least_once":true`, `"acknowledge_at_least_once":false`, 1), "event_actor", f.p.BranchID, 422)
	r := f.call(t, "POST", "", "event-create-original", "", input, "event_actor", f.p.BranchID, 201)
	replay := f.call(t, "POST", "", "event-create-original", "", input, "event_actor", f.p.BranchID, 201)
	if replay.Body.String() != r.Body.String() {
		t.Fatal("event trigger replay changed")
	}
	var result struct {
		Trigger functionTriggerView `json:"trigger"`
	}
	if json.Unmarshal(r.Body.Bytes(), &result) != nil || result.Trigger.Storage == nil || result.Trigger.Next != nil {
		t.Fatal("invalid storage trigger representation")
	}
	f.trigger = result.Trigger.ID
	return f.trigger
}
func (f *storageEventFixture) publish(t *testing.T, key string, commit bool) {
	t.Helper()
	tx, err := f.native.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err = storageDirectoryLock(f.ctx, tx); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	if _, err = tx.Exec(f.ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('event-bucket',$1,$2,$3,1,'text/plain') ON CONFLICT(bucket,key) DO UPDATE SET blob_key=$2,sha256=$3`, key, f.p.ProjectID+"/blob_event", sha); err != nil {
		t.Fatal(err)
	}
	if err = f.s.publishStorageFunctionEvent(f.ctx, tx, f.p, "event-bucket", key, sha, 1); err != nil {
		t.Fatal(err)
	}
	if commit {
		if err = tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
}
func (f *storageEventFixture) export(t *testing.T) {
	t.Helper()
	var generation int64
	if err := f.db.QueryRow(f.ctx, `SELECT generation FROM storage_event_branches WHERE branch_id=$1`, f.p.BranchID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := f.s.exportStorageFunctionEventsConn(f.ctx, f.native, storageEventBranch{Payload: f.p, Timeline: f.timeline, Generation: generation}); err != nil {
		t.Fatal(err)
	}
}

func TestStorageFunctionOutboxAtomicityIsolationAndCopy(t *testing.T) {
	f := newStorageEventFixture(t)
	f.create(t)
	// Reproduce an active pre-upgrade native catalog, in this disposable DB.
	if _, err := f.native.Exec(f.ctx, `DROP TABLE neon_storage.object_events`); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "uploads/rollback.txt", false)
	var count int
	if f.native.QueryRow(f.ctx, `SELECT count(*) FROM neon_storage.objects`).Scan(&count) != nil || count != 0 {
		t.Fatal("rolled-back object remained")
	}
	var hasOutbox bool
	if f.native.QueryRow(f.ctx, `SELECT to_regclass('neon_storage.object_events') IS NOT NULL`).Scan(&hasOutbox) != nil || hasOutbox {
		t.Fatal("first publication rollback left its lazy outbox")
	}
	f.export(t)
	var dirty bool
	if f.db.QueryRow(f.ctx, `SELECT dirty FROM storage_event_branches WHERE branch_id=$1`, f.p.BranchID).Scan(&dirty) != nil || dirty {
		t.Fatal("rolled-back first publication kept waking an empty native catalog")
	}
	if f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_trigger_invocations`).Scan(&count) != nil || count != 0 {
		t.Fatal("rolled-back event dispatched")
	}
	f.publish(t, "outside.txt", true)
	f.publish(t, "uploads/first.txt", true)
	f.export(t)
	if f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_trigger_invocations`).Scan(&count) != nil || count != 1 {
		t.Fatal("exact prefix occurrence missing")
	}
	var id string
	if f.db.QueryRow(f.ctx, `SELECT id FROM function_trigger_invocations`).Scan(&id) != nil {
		t.Fatal("missing original occurrence")
	}
	if _, err := f.native.Exec(f.ctx, `UPDATE neon_storage.object_events SET state='pending',exported_at=NULL WHERE branch_id=$1`, f.p.BranchID); err != nil {
		t.Fatal(err)
	}
	f.export(t)
	if f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_trigger_invocations WHERE id=$1`, id).Scan(&count) != nil || count != 1 {
		t.Fatal("outbox replay duplicated occurrence")
	}
	ctx := f.s.storagePublicationContext(f.ctx, f.p)
	if _, err := copyStorageObject(ctx, f.native, f.p.ProjectID, "event-bucket", "uploads/first.txt", "event-bucket", "uploads/copied.txt", "", "", ""); err != nil {
		t.Fatal(err)
	}
	f.export(t)
	if f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_trigger_invocations`).Scan(&count) != nil || count != 2 {
		t.Fatal("copy did not publish exactly one event")
	}
	// Same catalog bytes are visible in a clone, but original-branch envelopes
	// cannot be adopted by the new branch even when IDs/paths are copied.
	clone := f.p
	clone.BranchID = "br_2123456789abcdef"
	if err := f.s.exportStorageFunctionEventsConn(f.ctx, f.native, storageEventBranch{Payload: clone, Timeline: "2123456789abcdef2123456789abcdef", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_trigger_invocations WHERE branch_id=$1`, clone.BranchID).Scan(&count) != nil || count != 0 {
		t.Fatal("clone replayed parent events")
	}
	// A database owner can edit its catalog. It cannot mint a platform signature.
	bad := storageEventEnvelope{ID: "soe_" + randomToken(16), Project: f.p.ProjectID, Branch: f.p.BranchID, Timeline: f.timeline, KeyVersion: "event-ci-v1", Created: time.Now().UTC(), Data: storageCreatedData{Bucket: "event-bucket", Key: "uploads/forged.txt", SHA256: strings.Repeat("a", 64), Size: 1}, Targets: []storageEventTarget{{Trigger: f.trigger, Function: "fnc_1123456789abcdef", Actor: "event_actor", Generation: 1, Name: "new uploads", Slug: "onupload", Path: "/uploaded"}}}
	raw, _ := json.Marshal(bad)
	if _, err := f.native.Exec(f.ctx, `INSERT INTO neon_storage.object_events(id,project_id,branch_id,timeline_id,envelope,mac) VALUES($1,$2,$3,$4,$5,$6)`, bad.ID, bad.Project, bad.Branch, bad.Timeline, raw, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	f.export(t)
	var state string
	if f.native.QueryRow(f.ctx, `SELECT state FROM neon_storage.object_events WHERE id=$1`, bad.ID).Scan(&state) != nil || state != "rejected" {
		t.Fatal("unsigned event was not rejected")
	}
	if f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_trigger_invocations`).Scan(&count) != nil || count != 2 {
		t.Fatal("forged event dispatched")
	}
}

func TestFunctionObjectDeliveryRedriveAndImmutableReceipts(t *testing.T) {
	f := newStorageEventFixture(t)
	id := f.create(t)
	f.publish(t, "uploads/failed.txt", true)
	f.export(t)
	var original string
	if f.db.QueryRow(f.ctx, `SELECT id FROM function_trigger_invocations WHERE trigger_id=$1`, id).Scan(&original) != nil {
		t.Fatal("missing original")
	}
	for attempt := 1; attempt <= 3; attempt++ {
		run, err := f.s.claimFunctionTrigger(f.ctx)
		if err != nil || run == nil || run.ID != original || run.Attempts != attempt {
			t.Fatal("invalid original attempt", err)
		}
		p := functionInstanceTarget{FunctionID: run.Function, ProjectID: run.Project, BranchID: run.Branch, DeploymentID: "fdp_1123456789abcdef", Generation: 1, Slug: "onupload"}
		req := httptest.NewRequest("POST", "http://event.test/", nil)
		req = req.WithContext(context.WithValue(f.ctx, requestIDKey, original+"-"+string(rune('0'+attempt))))
		event, err := f.s.beginFunctionEvent(f.ctx, req, p, "fni_1123456789abcdef", "object_storage", "POST")
		if err != nil {
			t.Fatal(err)
		}
		if err = f.s.attachFunctionTriggerAttempt(f.ctx, *run, event.ID); err != nil {
			t.Fatal(err)
		}
		f.s.finishFunctionEvent(event, 503, 0, "completed")
		f.s.finishFunctionTrigger(*run, "failed", "application_response_failed", 503, 0, true)
		if attempt < 3 {
			if _, err = f.db.Exec(f.ctx, `UPDATE function_trigger_invocations SET next_attempt_at=now() WHERE id=$1`, original); err != nil {
				t.Fatal(err)
			}
		}
	}
	var old []byte
	if f.db.QueryRow(f.ctx, `SELECT to_jsonb(i) FROM function_trigger_invocations i WHERE id=$1`, original).Scan(&old) != nil {
		t.Fatal("original failure missing")
	}
	route := "/" + id + "/invocations/" + original + "/retry"
	input := `{"acknowledge_duplicate_side_effects":true}`
	f.call(t, "POST", route, "redrive-viewer-0001", "\""+original+"\"", input, "event_viewer", f.p.BranchID, 403)
	f.call(t, "POST", route, "redrive-noack-00001", "\""+original+"\"", `{"acknowledge_duplicate_side_effects":false}`, "event_actor", f.p.BranchID, 422)
	f.call(t, "POST", route, "redrive-cross-00001", "\""+original+"\"", input, "event_actor", "br_2123456789abcdef", 404)
	accepted := f.call(t, "POST", route, "redrive-original-01", "\""+original+"\"", input, "event_actor", f.p.BranchID, 202)
	replay := f.call(t, "POST", route, "redrive-original-01", "\""+original+"\"", input, "event_actor", f.p.BranchID, 202)
	if replay.Body.String() != accepted.Body.String() {
		t.Fatal("lost reply created replacement delivery")
	}
	f.call(t, "POST", route, "redrive-second-001", "\""+original+"\"", input, "event_actor", f.p.BranchID, 409)
	run, err := f.s.claimFunctionTrigger(f.ctx)
	if err != nil || run == nil || run.Parent != original || run.Snapshot.LogicalID != original || run.ID == original {
		t.Fatal("redrive lost original logical identity", err)
	}
	f.s.finishFunctionTrigger(*run, "succeeded", "", 200, 0, false)
	f.call(t, "POST", route, "redrive-delivered-1", "\""+original+"\"", input, "event_actor", f.p.BranchID, 409)
	var after []byte
	if f.db.QueryRow(f.ctx, `SELECT to_jsonb(i) FROM function_trigger_invocations i WHERE id=$1`, original).Scan(&after) != nil || !bytes.Equal(old, after) {
		t.Fatal("original terminal failure was rewritten")
	}
	var attempts int
	if f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_trigger_delivery_attempts WHERE invocation_id=$1`, original).Scan(&attempts) != nil || attempts != 3 {
		t.Fatal("per-attempt journal missing")
	}
}
