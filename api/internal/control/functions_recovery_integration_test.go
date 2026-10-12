package control

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real disposable PostgreSQL lifecycle guards. Fixture retirement proves the
// metadata boundary only; actual VM/Runner absence is a separate live UI gate.
func TestFunctionRetainedFailedSourceRequiresCompletedStop(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_fn_recovery_" + randomToken(8)
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
	suffix := randomToken(8)
	function, project, branch, writer, deployment := "fnc_"+suffix, "prj_"+suffix, "br_"+suffix, "ep_"+suffix, "fdp_"+suffix
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('actor','actor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org','fn-recovery','Recovery')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'org','Recovery','rke2-lab',16,'ready','managed',now(),now())`, project)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now())`, branch, project)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',now(),now())`, writer, project, branch)
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'org',$2,$3,$4,'hello','neondb','app',$5,'degraded')`, function, project, branch, writer, "fn_"+suffix)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('deploy',$1,'function',$2,'deploy_function','failed','actor','recovery','{}')`, project, function)
	artifactKey, err := functionArtifactKey(project, branch, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	environmentName := "fdp-" + suffix + "-environment"
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id) VALUES($1,$2,$3,$4,'nodejs24',$5,100,'index.mjs',$6,$7,'actor','deploy')`, deployment, function, project, branch, strings.Repeat("a", 64), artifactKey, environmentName)
	exec(`UPDATE function_definitions SET target_deployment_id=$2 WHERE id=$1`, function, deployment)
	exec(`INSERT INTO function_deployment_backings(deployment_id,function_id,project_id,branch_id,environment_namespace,environment_secret_ref,environment_secret_uid,environment_mac,mac_key_version) VALUES($1,$2,$3,$4,'neon-ci',$5,'11111111-2222-3333-4444-555555555555',$6,'ci-key-v1')`, deployment, function, project, branch, environmentName, strings.Repeat("b", 64))
	exec(`INSERT INTO function_instances(id,function_id,deployment_id,project_id,branch_id,generation,vm_name,service_name,bootstrap_secret_ref,state) VALUES($1,$2,$3,$4,$5,1,$6,$6,$6||'-bootstrap','starting')`, "fni_"+suffix, function, deployment, project, branch, "fn-"+suffix)
	check := func(name string, want int) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			var count int
			if err := db.QueryRow(ctx, "SELECT count(*) FROM ("+functionPreviousSourceQuery+") q", function).Scan(&count); err != nil || count != want {
				t.Fatalf("retained source count=%d want=%d: %v", count, want, err)
			}
		})
	}
	check("failed_definition_does_not_authorize_replacement", 0)
	exec(`UPDATE function_definitions SET state='suspended',generation=2 WHERE id=$1`, function)
	check("suspended_intent_without_completed_stop_denied", 0)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('stop',$1,'function',$2,'suspend_function','running','actor','recovery',jsonb_build_object('generation',2,'branch_id',$3::text))`, project, function, branch)
	check("incomplete_stop_denied", 0)
	exec(`UPDATE operations SET state='succeeded' WHERE id='stop'`)
	check("completed_stop_with_live_candidate_denied", 0)
	exec(`UPDATE function_instances SET state='retired',retired_at=now() WHERE function_id=$1`, function)
	check("completed_owned_stop_and_retirement_reuses_original_source", 1)
	exec(`UPDATE operations SET payload=jsonb_set(payload,'{generation}','3') WHERE id='stop'`)
	check("foreign_stop_generation_denied", 0)
	exec(`UPDATE operations SET payload=jsonb_build_object('generation',2,'branch_id','br_ffffffffffffffff') WHERE id='stop'`)
	check("foreign_stop_branch_denied", 0)
	exec(`UPDATE operations SET payload=jsonb_build_object('generation',2,'branch_id',$1::text) WHERE id='stop'`, branch)
	exec(`UPDATE operations SET state='cancelled' WHERE id='deploy'`)
	check("cancelled_deployment_not_adopted_as_failed", 0)
	exec(`UPDATE function_deployments SET state='completed',finished_at=now() WHERE id=$1`, deployment)
	exec(`UPDATE function_definitions SET state='active',active_deployment_id=$2 WHERE id=$1`, function, deployment)
	check("completed_active_source_remains_reusable", 1)
}
