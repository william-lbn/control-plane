package control

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/functionedge"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

func TestFunctionEdgeJournalAndResourceMetering(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_function_edge_" + randomToken(8)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, config)
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
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('edge_actor','edge_actor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('edge_org','edge-org','Edge')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('edge_org','edge_actor','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_0000000000000003','edge_org','Edge','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_0000000000000004','prj_0000000000000003','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('edge_writer','prj_0000000000000003','br_0000000000000004','edge_writer','neonvm','edge_writer','edge_writer','suspended','cloud_admin','neondb',now(),now())`)
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES('fnc_0000000000000005','edge_org','prj_0000000000000003','br_0000000000000004','edge_writer','hello','neondb','app','fn_0000000000000005','suspended')`)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('edge_op','prj_0000000000000003','function','fnc_0000000000000005','deploy_function','succeeded','edge_actor','edge_request','{}')`)
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id,state,finished_at) VALUES('fdp_0000000000000002','fnc_0000000000000005','prj_0000000000000003','br_0000000000000004','nodejs24',repeat('a',64),100,'index.mjs','edge/artifact','edge-env','edge_actor','edge_op','completed',now())`)
	exec(`INSERT INTO function_instances(id,function_id,deployment_id,project_id,branch_id,generation,vm_name,service_name,bootstrap_secret_ref,vm_uid,service_uid,boot_id,state)
 VALUES('fni_0000000000000001','fnc_0000000000000005','fdp_0000000000000002','prj_0000000000000003','br_0000000000000004',1,'fn-0000000000000001','fn-0000000000000001','fn-0000000000000001-bootstrap','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',repeat('a',32),'ready')`)
	exec(`UPDATE function_definitions SET state='active',active_deployment_id='fdp_0000000000000002' WHERE id='fnc_0000000000000005'`)
	s := &server{db: db}
	scope := functions.Scope{InstanceID: "fni_0000000000000001", DeploymentID: "fdp_0000000000000002", ProjectID: "prj_0000000000000003", BranchID: "br_0000000000000004", Slug: "hello", Generation: 1}
	runtime := publishedFunctionRuntime{target: functionInstanceTarget{FunctionID: "fnc_0000000000000005", ProjectID: scope.ProjectID, BranchID: scope.BranchID, DeploymentID: scope.DeploymentID, Generation: 1, Slug: scope.Slug}, instance: functionInstanceRecord{Scope: scope}, bootID: strings.Repeat("a", 32), version: 1}
	r := httptest.NewRequest("POST", "http://broker.invalid", nil)
	r = r.WithContext(context.WithValue(ctx, requestIDKey, "edge_once"))
	event, err := s.beginFunctionEdgeEvent(ctx, r, runtime, 4, "POST", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err = db.QueryRow(ctx, `SELECT outcome FROM function_edge_invocations WHERE event_id=$1`, event.ID).Scan(&outcome); err != nil || outcome != "outcome_unknown" {
		t.Fatal("admission pretended to complete", err)
	}
	v := functionedge.Completion{EventID: event.ID, Dispatched: true, Status: 201, RequestBytes: 4, ResponseBytes: 11, DurationMS: 2, Outcome: "completed"}
	if !s.completeFunctionEdgeEvent(event, v) || s.completeFunctionEdgeEvent(event, v) {
		t.Fatal("completion missing or replay counted twice")
	}
	var calls, bytes int64
	if err = db.QueryRow(ctx, `SELECT count(*),sum(response_bytes)::bigint FROM function_edge_invocations WHERE function_id=$1`, runtime.target.FunctionID).Scan(&calls, &bytes); err != nil || calls != 1 || bytes != 11 {
		t.Fatal("trusted completion aggregation changed", err)
	}
	now := time.Now()
	sample := functions.ResourceMeter{Scope: scope, BootID: runtime.bootID, CPUUsec: 100, MemoryBytes: 4096, MemoryPeakBytes: 8192, ObservedAt: now, Source: "supervisor_cgroup_v2"}
	if err = s.recordFunctionResourceSample(ctx, runtime.target.FunctionID, sample); err != nil {
		t.Fatal(err)
	}
	if err = s.recordFunctionResourceSample(ctx, runtime.target.FunctionID, sample); err != nil {
		t.Fatal(err)
	}
	sample.CPUUsec = 90
	sample.ObservedAt = now.Add(time.Millisecond)
	if err = s.recordFunctionResourceSample(ctx, runtime.target.FunctionID, sample); err != nil {
		t.Fatal(err)
	}
	sample.CPUUsec = 150
	sample.ObservedAt = now.Add(2 * time.Millisecond)
	if err = s.recordFunctionResourceSample(ctx, runtime.target.FunctionID, sample); err != nil {
		t.Fatal(err)
	}
	var count, cpu int64
	if err = db.QueryRow(ctx, `SELECT count(*),max(cpu_usec) FROM function_resource_samples WHERE function_id=$1`, runtime.target.FunctionID).Scan(&count, &cpu); err != nil || count != 2 || cpu != 150 {
		t.Fatal("duplicate/regressed resource samples inflated usage", err)
	}
	foreign := sample
	foreign.BootID = strings.Repeat("b", 32)
	foreign.ObservedAt = now.Add(3 * time.Millisecond)
	if err = s.recordFunctionResourceSample(ctx, runtime.target.FunctionID, foreign); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM function_resource_samples WHERE function_id=$1`, runtime.target.FunctionID).Scan(&count); err != nil || count != 2 {
		t.Fatal("foreign boot resource report accepted", err)
	}
	foreign = sample
	foreign.Scope.BranchID = "br_0000000000000009"
	foreign.ObservedAt = now.Add(4 * time.Millisecond)
	if err = s.recordFunctionResourceSample(ctx, runtime.target.FunctionID, foreign); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM function_resource_samples WHERE function_id=$1`, runtime.target.FunctionID).Scan(&count); err != nil || count != 2 {
		t.Fatal("foreign branch resource report accepted", err)
	}
}
