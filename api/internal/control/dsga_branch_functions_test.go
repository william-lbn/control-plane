package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

const dsgaSnapshotFunction = "fnc_4123456789abcdef"
const dsgaSnapshotDeployment = "fdp_4123456789abcdef"

// 本夹具只准备元数据库合同，显式模拟已有原 Operation 的成功事实；不
// 启动 VM、不提供真实 Pageserver 证明，也不宣称 Functions 已在 Guest 执行。
type dsgaBranchFunctionFixture struct {
	*dsgaProjectFixture
	parent          createPayload
	plan            dsgaCreationPlan
	parentOperation string
	requestContext  context.Context
}

func newDSGABranchFunctionFixture(t *testing.T, major uint32) *dsgaBranchFunctionFixture {
	t.Helper()
	f := newDSGAProjectFixture(t)
	setDSGACatalog(t, dsgaRetainedReleaseCatalogFixture(t))
	f.body.PostgresVersion = int(major)
	if response := f.request("creation_admin", "function-parent-original", f.body); response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body.String())
	}
	op, _, _ := f.reservation("function-parent-original")
	var raw []byte
	if err := f.s.db.QueryRow(f.ctx, "SELECT payload FROM operations WHERE id=$1", op).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		createPayload
		Plan dsgaCreationPlan `json:"dsga_creation"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE operations SET state='running',lease_owner='function-parent',lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1", op)
	binding, err := f.s.loadDSGACreationBinding(f.ctx, op, "function-parent")
	if err != nil {
		t.Fatal(err)
	}
	if err = binding.bind(f.ctx, "fixture-functions-parent-vm", ""); err != nil {
		t.Fatal(err)
	}
	if err = binding.bind(f.ctx, "fixture-functions-parent-vm", "fixture-functions-parent-config"); err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE projects SET state='ready' WHERE id=$1", envelope.ProjectID)
	f.exec("UPDATE branches SET state='ready' WHERE id=$1", envelope.BranchID)
	f.exec("UPDATE endpoints SET state='active' WHERE id=$1", envelope.EndpointID)
	f.exec("UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", op)
	f.s.functions = &functionsPlatform{key: bytes.Repeat([]byte{0x51}, 32)}
	f.s.functions.input.KeyVersion = "dsga-branch-ci-v1"
	f.s.storage = &storageBlobs{}
	t.Setenv("NEON_FUNCTION_BRANCH_COPY_ENABLED", "true")
	f.exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state)
 VALUES($1,'org_creation',$2,$3,$4,'frozensource','neondb','app','fn_4123456789abcdef','suspended')`, dsgaSnapshotFunction, envelope.ProjectID, envelope.BranchID, envelope.EndpointID)
	f.exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
 VALUES('dsga_snapshot_source_deploy',$1,'function',$2,'deploy_function','succeeded','creation_admin','source-code','{}')`, envelope.ProjectID, dsgaSnapshotFunction)
	key, err := functionArtifactKey(envelope.ProjectID, envelope.BranchID, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,environment_names,actor_id,operation_id,state,finished_at)
 VALUES($1,$2,$3,$4,'nodejs24',$5,100,'index.mjs',$6,'fdp-4123456789abcdef-environment',ARRAY['FROZEN'],'creation_admin','dsga_snapshot_source_deploy','completed',now())`,
		dsgaSnapshotDeployment, dsgaSnapshotFunction, envelope.ProjectID, envelope.BranchID, strings.Repeat("b", 64), key)
	f.exec(`INSERT INTO function_deployment_backings(deployment_id,function_id,project_id,branch_id,environment_namespace,environment_secret_ref,environment_secret_uid,environment_mac,mac_key_version)
 VALUES($1,$2,$3,$4,'neon','fdp-4123456789abcdef-environment','11111111-1111-4111-8111-111111111111',$5,'dsga-branch-ci-v1')`,
		dsgaSnapshotDeployment, dsgaSnapshotFunction, envelope.ProjectID, envelope.BranchID, strings.Repeat("c", 64))
	f.exec("UPDATE function_definitions SET active_deployment_id=$2 WHERE id=$1", dsgaSnapshotFunction, dsgaSnapshotDeployment)
	f.exec(`INSERT INTO function_triggers(branch_id,id,project_id,org_id,function_id,actor_id,origin_branch_id,type,name,function_path,cron,enabled,next_run_at)
 VALUES($1,'ftr_4123456789abcdef01234567',$2,'org_creation',$3,'creation_admin',$1,'schedule','original cron','/original','* * * * *',true,now()+interval '1 minute')`, envelope.BranchID, envelope.ProjectID, dsgaSnapshotFunction)
	auth := dsgaCreationAuth{Actor: "creation_admin", SessionHash: digest("creation_admin-session"), CSRFHash: digest("creation_admin-csrf")}
	return &dsgaBranchFunctionFixture{f, envelope.createPayload, envelope.Plan, op, context.WithValue(f.ctx, dsgaCreationAuthKey{}, auth)}
}

func (f *dsgaBranchFunctionFixture) admit(key string, body branchCreateRequest, source string) (createPayload, string, error) {
	return f.s.admitDSGAStorageBranch(f.requestContext, f.parent.ProjectID, "org_creation", "creation_admin", f.parent.TenantID, f.parent.TimelineID, "0/200", source,
		body, &f.plan, key, "branch-functions-fixture-hash", "branch-functions-request")
}

func (f *dsgaBranchFunctionFixture) confirmBranch(p createPayload, op string) {
	f.t.Helper()
	f.exec("UPDATE operations SET state='running',lease_owner='function-fork',lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1", op)
	request, _ := json.Marshal(dsgaTimelineRequest{p.TimelineID, f.plan.PostgresVersion, p.ParentTimelineID, p.ParentLSN})
	f.exec(`INSERT INTO dsga_creation_timelines(operation_id,tenant_id,timeline_id,request) VALUES($1,$2,$3,$4)`, op, p.TenantID, p.TimelineID, request)
	f.exec("UPDATE dsga_creation_timelines SET confirmed_at=clock_timestamp() WHERE operation_id=$1", op)
	f.exec("UPDATE branches SET state='ready' WHERE id=$1", p.BranchID)
	f.exec("UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", op)
}

func (f *dsgaBranchFunctionFixture) requestWriter(p createPayload) (string, string) {
	f.t.Helper()
	data, _ := json.Marshal(endpointCreateRequest{BranchID: p.BranchID, Type: "read_write", Password: f.body.Password})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+p.ProjectID+"/endpoints", bytes.NewReader(data))
	r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "creation_admin-session"})
	r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "creation_admin-csrf"})
	r.Header.Set("X-CSRF-Token", "creation_admin-csrf")
	r.Header.Set("Idempotency-Key", "branch-functions-child-writer")
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	if w.Code != 202 {
		f.t.Fatal(w.Code, w.Body.String())
	}
	var op, writer string
	if err := f.s.db.QueryRow(f.ctx, `SELECT o.id,o.resource_id FROM idempotency_keys k JOIN operations o ON o.id=k.operation_id WHERE k.actor_id='creation_admin' AND k.key_hash=$1`, keyHash("branch-functions-child-writer")).Scan(&op, &writer); err != nil {
		f.t.Fatal(err)
	}
	return op, writer
}

func TestDSGABranchFunctionConsentAndOmitAtomicity(t *testing.T) {
	for _, major := range []uint32{17, 18} {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			f := newDSGABranchFunctionFixture(t, major)
			body := branchCreateRequest{Name: "Inherited Functions", ParentBranchID: f.parent.BranchID, FunctionCopyPolicy: "inherit_paused"}
			for _, historical := range []bool{false, true} {
				source := ""
				if historical {
					source = "lsn"
					body.AcknowledgeFunctions = true
				}
				_, _, err := f.admit("missing-consent-"+fmt.Sprint(historical), body, source)
				var fault dsgaBranchFunctionError
				if !errors.As(err, &fault) || fault.status != 422 || fault.code != "function_copy_acknowledgment_required" {
					t.Fatal("missing consent accepted", err)
				}
			}
			var branches, plans int
			if err := f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM branches WHERE project_id=$1),(SELECT count(*) FROM function_branch_copy_plans WHERE project_id=$1)`, f.parent.ProjectID).Scan(&branches, &plans); err != nil || branches != 1 || plans != 0 {
				t.Fatal("failed consent wrote a child", err, branches, plans)
			}
			body.FunctionCopyPolicy = ""
			p, _, err := f.admit("default-remains-omit", body, "")
			if err != nil || p.BranchFunctionCopyPolicy != "omit" {
				t.Fatal("DSGA default changed", err)
			}
			if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM function_branch_copy_plans WHERE branch_id=$1`, p.BranchID).Scan(&plans); err != nil || plans != 0 {
				t.Fatal("default copied Functions", err)
			}
			body.FunctionCopyPolicy = "inherit_paused"
			t.Setenv("NEON_FUNCTION_BRANCH_COPY_ENABLED", "false")
			if _, _, err = f.admit("operator-disabled-copy", body, ""); err == nil {
				t.Fatal("disabled copy admitted")
			}
		})
	}
}

func TestDSGABranchFunctionPublicAdmissionRejectsBeforeStorage(t *testing.T) {
	f := newDSGABranchFunctionFixture(t, 17)
	for _, test := range []struct {
		name, actor, policy string
		compute, enabled    bool
		status              int
		code                string
	}{
		{"unknown_policy", "creation_admin", "copy_latest", false, true, 422, "invalid_function_copy_policy"},
		{"disabled_copy", "creation_admin", "inherit_paused", false, false, 503, "function_branch_copy_disabled"},
		{"viewer", "creation_viewer", "inherit_paused", false, true, 403, ""},
		{"joint_compute", "creation_admin", "inherit_paused", true, true, 422, "dsga_branch_compute_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("NEON_FUNCTION_BRANCH_COPY_ENABLED", fmt.Sprint(test.enabled))
			body := branchCreateRequest{Name: "Public admission", ParentBranchID: f.parent.BranchID,
				CreateEndpoint: &test.compute, FunctionCopyPolicy: test.policy, AcknowledgeFunctions: true}
			data, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.parent.ProjectID+"/branches", bytes.NewReader(data))
			r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: test.actor + "-session"})
			r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: test.actor + "-csrf"})
			r.Header.Set("X-CSRF-Token", test.actor+"-csrf")
			r.Header.Set("Idempotency-Key", "function-branch-"+test.name)
			response := httptest.NewRecorder()
			f.s.routes().ServeHTTP(response, r)
			if response.Code != test.status || (test.code != "" && !strings.Contains(response.Body.String(), test.code)) {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
	var branches, plans int
	if err := f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM branches WHERE project_id=$1),
 (SELECT count(*) FROM function_branch_copy_plans WHERE project_id=$1)`, f.parent.ProjectID).Scan(&branches, &plans); err != nil || branches != 1 || plans != 0 {
		t.Fatal("rejected public request wrote a child", err, branches, plans)
	}
}

func TestDSGABranchFunctionFrozenPlanAndOriginalWriter(t *testing.T) {
	for _, major := range []uint32{17, 18} {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			f := newDSGABranchFunctionFixture(t, major)
			body := branchCreateRequest{Name: "Frozen Functions", ParentBranchID: f.parent.BranchID, FunctionCopyPolicy: "inherit_paused", AcknowledgeFunctions: true, AcknowledgeCurrentCode: true}
			p, op, err := f.admit("frozen-functions-original", body, "lsn")
			if err != nil {
				t.Fatal(err)
			}
			if p.EndpointID != "" || p.BranchFunctionCopyPolicy != "inherit_paused" {
				t.Fatal("storage branch ran Compute")
			}
			for _, values := range [][3]string{{op, p.ProjectID, "foreign-source"}, {op, "foreign-project", f.parent.BranchID}, {f.parentOperation, p.ProjectID, f.parent.BranchID}} {
				var valid bool
				if err := f.s.db.QueryRow(f.ctx, `SELECT dsga_function_branch_source_valid($1,$2,$3,$4,'creation_admin',false)`, values[0], values[1], p.BranchID, values[2]).Scan(&valid); err != nil || valid {
					t.Fatal("foreign snapshot origin accepted", err)
				}
			}
			tx, err := f.s.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = f.s.snapshotFunctionBranchTx(f.ctx, tx, p.ProjectID, f.parent.BranchID, p.BranchID, f.parentOperation, "creation_admin")
			_ = tx.Rollback(f.ctx)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
				t.Fatal("non-branch original accepted", err)
			}
			if err := f.s.prepareFunctionBranchPlans(f.ctx, p.ProjectID, p.BranchID); err == nil {
				t.Fatal("materialized before branch and Writer ready")
			}
			f.confirmBranch(p, op)
			f.exec("UPDATE function_definitions SET active_deployment_id=NULL,version=version+1 WHERE id=$1", dsgaSnapshotFunction)
			f.exec("UPDATE function_triggers SET name='later cron',generation=generation+1 WHERE function_id=$1", dsgaSnapshotFunction)
			writerOp, writer := f.requestWriter(p)
			f.exec("UPDATE endpoints SET state='suspended' WHERE id=$1", writer)
			if err := f.s.prepareFunctionBranchPlans(f.ctx, p.ProjectID, p.BranchID); err == nil {
				t.Fatal("unconfirmed DSGA Writer accepted")
			}
			// 原创建 Worker 仍在 provisioning 阶段；不能先伪装 suspended
			// 再让它绑定，新路径和原生命周期必须保持同一状态语义。
			f.exec("UPDATE endpoints SET state='provisioning' WHERE id=$1", writer)
			f.exec("UPDATE operations SET state='running',lease_owner='function-writer',lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1", writerOp)
			binding, err := f.s.loadDSGACreationBinding(f.ctx, writerOp, "function-writer")
			if err != nil {
				t.Fatal(err)
			}
			if err = binding.bind(f.ctx, "fixture-functions-child-vm", ""); err != nil {
				t.Fatal(err)
			}
			if err = binding.bind(f.ctx, "fixture-functions-child-vm", "fixture-functions-child-config"); err != nil {
				t.Fatal(err)
			}
			f.exec("UPDATE operation_steps SET state='succeeded' WHERE operation_id=$1", writerOp)
			f.exec("UPDATE endpoints SET state='active' WHERE id=$1", writer)
			f.exec("UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", writerOp)
			for range 2 {
				if err = f.s.prepareFunctionBranchPlans(f.ctx, p.ProjectID, p.BranchID); err != nil {
					t.Fatal("original metadata materialization", err)
				}
			}
			var function, deployment, role string
			var sourceVersion int64
			if err = f.s.db.QueryRow(f.ctx, `SELECT plan.prepared_function_id,c.source_deployment_id,c.source_version,f.sql_role FROM function_branch_copy_plans plan JOIN function_definitions f ON f.id=plan.prepared_function_id JOIN function_copy_sources c ON c.deployment_id=plan.deployment_id WHERE plan.branch_id=$1`, p.BranchID).Scan(&function, &deployment, &sourceVersion, &role); err != nil || deployment != dsgaSnapshotDeployment || sourceVersion != 1 || role == "fn_4123456789abcdef" {
				t.Fatal("borrowed latest code or parent SQL identity", err)
			}
			var triggers, enabled, instances int
			var name string
			if err = f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM function_triggers WHERE branch_id=$1),(SELECT count(*) FROM function_triggers WHERE branch_id=$1 AND (enabled OR next_run_at IS NOT NULL)),(SELECT count(*) FROM function_instances WHERE branch_id=$1),(SELECT name FROM function_triggers WHERE branch_id=$1 LIMIT 1)`, p.BranchID).Scan(&triggers, &enabled, &instances, &name); err != nil || triggers != 1 || enabled+instances != 0 || name != "original cron" {
				t.Fatal("copied runnable state or latest trigger", err, triggers, enabled, instances, name)
			}
		})
	}
}
