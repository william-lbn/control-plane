package control

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

const snapshotChild = "br_2123456789abcdef"
const snapshotWriter = "ep_2123456789abcdef"
const snapshotSource = "fdp_3123456789abcdef"
const snapshotFn = "fnc_1123456789abcdef"

func snapshotExec(t *testing.T, f *storageEventFixture, sql string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
func newFunctionSnapshotFixture(t *testing.T, deployed bool) *storageEventFixture {
	t.Helper()
	f := newStorageEventFixture(t)
	t.Setenv("NEON_FUNCTION_BRANCH_COPY_ENABLED", "true")
	snapshotExec(t, f, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('snapshot_creation',$1,'branch',$2,'create_branch','queued','event_actor','snapshot-request','{}')`, f.p.ProjectID, snapshotChild)
	if !deployed {
		snapshotExec(t, f, `UPDATE function_definitions SET active_deployment_id=NULL WHERE id=$1`, snapshotFn)
		return f
	}
	key, _ := functionArtifactKey(f.p.ProjectID, f.p.BranchID, strings.Repeat("b", 64))
	snapshotExec(t, f, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('snapshot_deploy',$1,'function',$2,'deploy_function','succeeded','event_actor','snapshot-code','{}')`, f.p.ProjectID, snapshotFn)
	snapshotExec(t, f, `INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,environment_names,actor_id,operation_id,state,finished_at) VALUES($1,$2,$3,$4,'nodejs24',$5,100,'index.mjs',$6,'fdp-3123456789abcdef-environment',ARRAY['FROZEN'],'event_actor','snapshot_deploy','completed',now())`, snapshotSource, snapshotFn, f.p.ProjectID, f.p.BranchID, strings.Repeat("b", 64), key)
	snapshotExec(t, f, `INSERT INTO function_deployment_backings(deployment_id,function_id,project_id,branch_id,environment_namespace,environment_secret_ref,environment_secret_uid,environment_mac,mac_key_version) VALUES($1,$2,$3,$4,'neon','fdp-3123456789abcdef-environment','11111111-1111-4111-8111-111111111111',$5,'event-ci-v1')`, snapshotSource, snapshotFn, f.p.ProjectID, f.p.BranchID, strings.Repeat("c", 64))
	snapshotExec(t, f, `UPDATE function_definitions SET active_deployment_id=$2 WHERE id=$1`, snapshotFn, snapshotSource)
	return f
}
func captureFunctionSnapshot(t *testing.T, f *storageEventFixture) {
	t.Helper()
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err = f.s.snapshotFunctionBranchTx(f.ctx, tx, f.p.ProjectID, f.p.BranchID, snapshotChild, "snapshot_creation", "event_actor"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
}
func addFunctionSnapshotWriter(t *testing.T, f *storageEventFixture) {
	t.Helper()
	snapshotExec(t, f, `INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'suspended','cloud_admin','neondb',now(),now())`, snapshotWriter, f.p.ProjectID, snapshotChild)
}
func TestFunctionBranchSnapshotFrozenCodeDisabledTriggersAndChildOverride(t *testing.T) {
	f := newFunctionSnapshotFixture(t, true)
	storageTrigger := f.create(t)
	const cronTrigger = "ftr_3123456789abcdef01234567"
	snapshotExec(t, f, `INSERT INTO function_triggers(branch_id,id,project_id,org_id,function_id,actor_id,origin_branch_id,type,name,function_path,cron,enabled,next_run_at) VALUES($1,$2,$3,'event_org',$4,'event_actor',$1,'schedule','frozen cron','/original','* * * * *',true,now()+interval '1 minute')`, f.p.BranchID, cronTrigger, f.p.ProjectID, snapshotFn)
	captureFunctionSnapshot(t, f)
	if f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild) == nil {
		t.Fatal("materialized without original Writer")
	}
	// Source code and trigger changes after acceptance must not leak into child.
	snapshotExec(t, f, `UPDATE function_definitions SET version=version+1,active_deployment_id='fdp_1123456789abcdef' WHERE id=$1`, snapshotFn)
	snapshotExec(t, f, `UPDATE function_triggers SET name='later cron',function_path='/later',generation=generation+1 WHERE branch_id=$1 AND id=$2`, f.p.BranchID, cronTrigger)
	if _, err := f.db.Exec(f.ctx, `UPDATE function_branch_copy_plans SET trigger_snapshots='[]' WHERE branch_id=$1`, snapshotChild); err == nil {
		t.Fatal("accepted trigger snapshot mutable")
	}
	addFunctionSnapshotWriter(t, f)
	snapshotExec(t, f, `UPDATE branches SET state='error' WHERE id=$1`, snapshotChild)
	if f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild) == nil {
		t.Fatal("failed branch prepared before normal create recovery")
	}
	snapshotExec(t, f, `UPDATE branches SET state='ready' WHERE id=$1`, snapshotChild)
	if err := f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild); err != nil {
		t.Fatal(err)
	}
	if err := f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild); err != nil {
		t.Fatal("same planned identity replay", err)
	}
	var fn, op, did, origin, role, state string
	if err := f.db.QueryRow(f.ctx, `SELECT p.prepared_function_id,p.prepared_operation_id,f.target_deployment_id,c.source_deployment_id,f.sql_role,f.state FROM function_branch_copy_plans p JOIN function_definitions f ON f.id=p.prepared_function_id JOIN function_copy_sources c ON c.deployment_id=f.target_deployment_id WHERE p.branch_id=$1`, snapshotChild).Scan(&fn, &op, &did, &origin, &role, &state); err != nil || origin != snapshotSource || role != "fn_"+fn[4:] || state != "provisioning" {
		t.Fatal("frozen source/independent identity mismatch", err)
	}
	var ready, enabled, runs, vms, policy int
	if err := f.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM function_triggers WHERE branch_id=$1 AND state='ready'),(SELECT count(*) FROM function_triggers WHERE branch_id=$1 AND (enabled OR next_run_at IS NOT NULL)),(SELECT count(*) FROM function_trigger_invocations WHERE branch_id=$1),(SELECT count(*) FROM function_instances WHERE branch_id=$1),(SELECT count(*) FROM function_http_policies WHERE function_id=$2)`, snapshotChild, fn).Scan(&ready, &enabled, &runs, &vms, &policy); err != nil || ready != 2 || enabled+runs+vms+policy != 0 {
		t.Fatal("inherited execution/occurrences or missing triggers", err)
	}
	// Stable public trigger ID is kept; CAS child edit becomes a local override.
	original := f.call(t, "GET", "/"+cronTrigger, "", "", "", "event_viewer", snapshotChild, 200)
	if !strings.Contains(original.Body.String(), `"inherited":true`) || !strings.Contains(original.Body.String(), `"name":"frozen cron"`) {
		t.Fatal("source snapshot projection changed")
	}
	f.call(t, "PATCH", "/"+cronTrigger, "child-cron-edit-01", `"1"`, `{"type":"schedule","name":"child override","enabled":false}`, "event_actor", snapshotChild, 200)
	var parentName string
	if err := f.db.QueryRow(f.ctx, `SELECT name FROM function_triggers WHERE branch_id=$1 AND id=$2`, f.p.BranchID, cronTrigger).Scan(&parentName); err != nil || parentName != "later cron" {
		t.Fatal("child edit changed parent", err)
	}
	childView := f.call(t, "GET", "/"+cronTrigger, "", "", "", "event_actor", snapshotChild, 200)
	if !strings.Contains(childView.Body.String(), `"inherited":false`) {
		t.Fatal("child override still follows parent")
	}
	f.call(t, "DELETE", "/"+storageTrigger, "child-storage-delete", `"1"`, "", "event_actor", snapshotChild, 204)
	if err := f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild); err != nil {
		t.Fatal(err)
	}
	var tombstone int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_triggers WHERE branch_id=$1 AND id=$2 AND state='deleted'`, snapshotChild, storageTrigger).Scan(&tombstone); err != nil || tombstone != 1 {
		t.Fatal("child tombstone was resurrected", err)
	}
	// Plan read is viewer metadata and never needs a Kube or SQL connector.
	req := httptest.NewRequest("GET", "/api/v1/projects/"+f.p.ProjectID+"/branches/"+snapshotChild+"/function-copy-plans", nil)
	req.Header.Set("Cookie", "neon_v2_session=event_viewer-session")
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, req)
	var view struct {
		Items []map[string]any `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || len(view.Items) != 1 || view.Items[0]["operation_id"] != op || view.Items[0]["source_deployment_id"] != snapshotSource {
		t.Fatal("plan view lost original receipt", w.Code, w.Body.String())
	}
}
func TestFunctionBranchSnapshotDefinitionOnlyRevocationQuotaAndMissingBacking(t *testing.T) {
	t.Run("definition_only", func(t *testing.T) {
		f := newFunctionSnapshotFixture(t, false)
		captureFunctionSnapshot(t, f)
		addFunctionSnapshotWriter(t, f)
		snapshotExec(t, f, `UPDATE users SET disabled=true WHERE id='event_actor'`)
		if f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild) == nil {
			t.Fatal("revoked actor copied a definition")
		}
		snapshotExec(t, f, `UPDATE users SET disabled=false WHERE id='event_actor'`)
		if err := f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_branch_copy_plans p JOIN function_definitions f ON f.id=p.prepared_function_id JOIN operations o ON o.id=p.prepared_operation_id WHERE p.branch_id=$1 AND p.source_deployment_id IS NULL AND p.deployment_id IS NULL AND f.active_deployment_id IS NULL AND f.target_deployment_id IS NULL AND o.state='succeeded' AND o.action='create_function'`, snapshotChild).Scan(&count); err != nil || count != 1 {
			t.Fatal("invented code/SQL for empty definition", err)
		}
	})
	t.Run("quota_preserves_ready_branch_and_original_plan", func(t *testing.T) {
		f := newFunctionSnapshotFixture(t, true)
		captureFunctionSnapshot(t, f)
		addFunctionSnapshotWriter(t, f)
		for n := 1; n <= 7; n++ {
			id := fmt.Sprintf("fnc_%016x", n)
			snapshotExec(t, f, `INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'event_org',$2,$3,'event_writer',$4,'neondb','app',$5,'provisioning')`, id, f.p.ProjectID, f.p.BranchID, fmt.Sprintf("quota%d", n), "fn_"+id[4:])
		}
		if f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild) == nil {
			t.Fatal("ignored definition budget")
		}
		var planned string
		var ready bool
		var children int
		if err := f.db.QueryRow(f.ctx, `SELECT p.function_id,b.state='ready',(SELECT count(*) FROM function_definitions WHERE branch_id=$1) FROM function_branch_copy_plans p JOIN branches b ON b.id=p.branch_id WHERE p.branch_id=$1 AND p.prepared_function_id IS NULL`, snapshotChild).Scan(&planned, &ready, &children); err != nil || !ready || children != 0 {
			t.Fatal("quota failure partially materialized or failed SQL branch", err)
		}
		snapshotExec(t, f, `UPDATE function_definitions SET state='deleted',deleted_at=now() WHERE id='fnc_0000000000000001'`)
		if err := f.s.prepareFunctionBranchPlans(f.ctx, f.p.ProjectID, snapshotChild); err != nil {
			t.Fatal(err)
		}
		var actual string
		if err := f.db.QueryRow(f.ctx, `SELECT prepared_function_id FROM function_branch_copy_plans WHERE branch_id=$1`, snapshotChild).Scan(&actual); err != nil || actual != planned {
			t.Fatal("quota retry changed original identity", err)
		}
	})
	t.Run("missing_source_backing", func(t *testing.T) {
		f := newFunctionSnapshotFixture(t, true)
		snapshotExec(t, f, `DELETE FROM function_deployment_backings WHERE deployment_id=$1`, snapshotSource)
		tx, err := f.db.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(f.ctx)
		if f.s.snapshotFunctionBranchTx(f.ctx, tx, f.p.ProjectID, f.p.BranchID, snapshotChild, "snapshot_creation", "event_actor") == nil {
			t.Fatal("missing published environment silently omitted")
		}
	})
}
