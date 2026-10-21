package control

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFunctionBranchCopyAdmissionSourceIsolationAndDurableReplay(t *testing.T) {
	f := newStorageEventFixture(t)
	t.Setenv("NEON_FUNCTION_BRANCH_COPY_ENABLED", "true")
	const child = "br_2123456789abcdef"
	const writer = "ep_2123456789abcdef"
	const source = "fdp_3123456789abcdef"
	sourceFn := "fnc_1123456789abcdef"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'suspended','cloud_admin','neondb',now(),now())`, writer, f.p.ProjectID, child)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,finished_at) VALUES('copy_source_original',$1,'function',$2,'deploy_function','succeeded','event_actor','copy-source-ci','{}',now())`, f.p.ProjectID, sourceFn)
	key, _ := functionArtifactKey(f.p.ProjectID, f.p.BranchID, strings.Repeat("b", 64))
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id,state,finished_at) VALUES($1,$2,$3,$4,'nodejs24',$5,100,'index.mjs',$6,'fdp-3123456789abcdef-environment','event_actor','copy_source_original','completed',now())`, source, sourceFn, f.p.ProjectID, f.p.BranchID, strings.Repeat("b", 64), key)
	exec(`INSERT INTO function_deployment_backings(deployment_id,function_id,project_id,branch_id,environment_namespace,environment_secret_ref,environment_secret_uid,environment_mac,mac_key_version) VALUES($1,$2,$3,$4,'neon','fdp-3123456789abcdef-environment','11111111-1111-4111-8111-111111111111',$5,'event-ci-v1')`, source, sourceFn, f.p.ProjectID, f.p.BranchID, strings.Repeat("c", 64))
	input := functionCopyInput{SourceBranch: f.p.BranchID, SourceSlug: "onupload", SourceDeployment: source, Slug: "copied", Acknowledge: true}
	call := func(in functionCopyInput, key, version, actor string, want int) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(in)
		r := httptest.NewRequest("POST", "/api/v1/projects/"+f.p.ProjectID+"/branches/"+child+"/functions-copy", strings.NewReader(string(body)))
		r.Header.Set("Cookie", "neon_v2_session="+actor+"-session; neon_v2_csrf="+actor+"-csrf")
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", version)
		w := httptest.NewRecorder()
		f.s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("copy status %d want %d: %s", w.Code, want, w.Body.String())
		}
		return w
	}
	call(input, "copy-viewer-00001", `"1"`, "event_viewer", 403)
	call(input, "copy-no-version01", "", "event_actor", 428)
	noack := input
	noack.Acknowledge = false
	call(noack, "copy-no-consent01", `"1"`, "event_actor", 422)
	same := input
	same.SourceBranch = child
	call(same, "copy-samebranch01", `"1"`, "event_actor", 422)
	foreign := input
	foreign.SourceBranch = "br_4123456789abcdef"
	call(foreign, "copy-foreign-0001", `"1"`, "event_actor", 409)
	legacy := input
	legacy.SourceDeployment = "fdp_1123456789abcdef"
	call(legacy, "copy-missing-ref1", `"1"`, "event_actor", 404)
	accepted := call(input, "copy-original-001", `"1"`, "event_actor", 202)
	replay := call(input, "copy-original-001", `"1"`, "event_actor", 202)
	if accepted.Body.String() != replay.Body.String() {
		t.Fatal("lost copy admission reply changed original receipt")
	}
	var response struct {
		Function  string `json:"function_id"`
		Operation struct {
			ID string `json:"id"`
		} `json:"operation"`
	}
	if json.Unmarshal(accepted.Body.Bytes(), &response) != nil || !functionDefinitionID.MatchString(response.Function) {
		t.Fatal("copy definition receipt missing")
	}
	var target functionInstanceTarget
	var role string
	var instances, policies, captures, triggers int
	target.FunctionID, target.ProjectID, target.BranchID = response.Function, f.p.ProjectID, child
	if err := f.db.QueryRow(f.ctx, `SELECT target_deployment_id,generation,slug,sql_role FROM function_definitions WHERE id=$1`, response.Function).Scan(&target.DeploymentID, &target.Generation, &target.Slug, &role); err != nil {
		t.Fatal(err)
	}
	if role != "fn_"+response.Function[4:] || role == "fn_1123456789abcdef" {
		t.Fatal("destination reused source SQL role")
	}
	if err := f.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM function_instances WHERE function_id=$1),(SELECT count(*) FROM function_http_policies WHERE function_id=$1),(SELECT count(*) FROM function_output_policies WHERE function_id=$1),(SELECT count(*) FROM function_triggers WHERE function_id=$1)`, response.Function).Scan(&instances, &policies, &captures, &triggers); err != nil || instances+policies+captures+triggers != 0 {
		t.Fatal("copy admission started execution or inherited authorization", err)
	}
	origin, err := f.s.loadFunctionCopySource(f.ctx, target)
	if err != nil || origin.Deployment != source || origin.Function != sourceFn || origin.Branch != f.p.BranchID {
		t.Fatal("immutable copy source unavailable", err)
	}
	exec(`UPDATE function_definitions SET version=version+1 WHERE id=$1`, sourceFn)
	call(input, "copy-new-stale001", `"1"`, "event_actor", 412)
	if call(input, "copy-original-001", `"1"`, "event_actor", 202).Body.String() != accepted.Body.String() {
		t.Fatal("parent update changed accepted copy")
	}
	changed := input
	changed.Slug = "different"
	call(changed, "copy-original-001", `"1"`, "event_actor", 409)
	if _, err := f.db.Exec(f.ctx, `UPDATE function_copy_sources SET source_version=source_version+1 WHERE deployment_id=$1`, target.DeploymentID); err == nil {
		t.Fatal("copy provenance was mutable")
	}
	exec(`UPDATE users SET disabled=true WHERE id='event_actor'`)
	if f.s.authorizeFunctionCopyActor(f.ctx, f.p.ProjectID, origin.Actor) == nil {
		t.Fatal("revoked source actor authorized backing access")
	}
	exec(`UPDATE users SET disabled=false WHERE id='event_actor'`)
	exec(`UPDATE operations SET state='running',lease_owner='copy-ci-worker',lease_expires_at=$2 WHERE id=$1`, response.Operation.ID, time.Now().Add(time.Minute))
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := lockFunctionCopyLease(f.ctx, tx, response.Operation.ID, "copy-ci-worker", target)
	tx.Rollback(f.ctx)
	if err != nil || committed {
		t.Fatal("valid pending copy scope/lease rejected", err)
	}
	wrong := target
	wrong.BranchID = f.p.BranchID
	tx, err = f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockFunctionCopyLease(f.ctx, tx, response.Operation.ID, "copy-ci-worker", wrong)
	tx.Rollback(f.ctx)
	if err == nil {
		t.Fatal("copy lease crossed original branch")
	}
	t.Setenv("NEON_FUNCTION_BRANCH_COPY_ENABLED", "false")
	call(input, "copy-operator-off1", `"2"`, "event_actor", 503)
}
