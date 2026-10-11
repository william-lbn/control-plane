package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Actual PostgreSQL locks, concurrent sessions and durable restart recovery are
// tested here. No Kubernetes/guest peer is simulated as physical retirement.
func TestFunctionsInstanceReservationLeaseAndOwnership(t *testing.T) {
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
	suffix := randomToken(8)
	schema := "v2_fn_instances_" + suffix
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["application_name"] = schema
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := &server{db: db}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	project, branch, endpoint := "prj_"+suffix, "br_"+suffix, "ep_"+suffix
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('fn_store_actor','fn_store_actor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('fn_store_org','fn-store','Functions store CI')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'fn_store_org','Functions','rke2-lab',16,'ready','managed',now(),now())`, project)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now())`, branch, project)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',now(),now())`, endpoint, project, branch)
	var targets []functionInstanceTarget
	for i := 1; i <= 3; i++ {
		p := functionInstanceTarget{fmt.Sprintf("fnc_%016x", i), project, branch, fmt.Sprintf("fdp_%016x", i), 1, fmt.Sprintf("hello%d", i)}
		operation := fmt.Sprintf("op_fn_store_%d", i)
		exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'fn_store_org',$2,$3,$4,$5,'neondb','app',$6,'provisioning')`, p.FunctionID, project, branch, endpoint, p.Slug, fmt.Sprintf("fn_%016x", i))
		exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'function',$3,'deploy_function','running','fn_store_actor','fn_ci','{}','fn_store_worker',now()+interval '1 hour')`, operation, project, p.FunctionID)
		exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id) VALUES($1,$2,$3,$4,'nodejs24',repeat('a',64),100,'index.mjs',$1,$5,'fn_store_actor',$6)`, p.DeploymentID, p.FunctionID, project, branch, fmt.Sprintf("fn-env-%d", i), operation)
		exec(`UPDATE function_definitions SET target_deployment_id=$2 WHERE id=$1`, p.FunctionID, p.DeploymentID)
		targets = append(targets, p)
	}
	op, worker, p := "op_fn_store_1", "fn_store_worker", targets[0]
	var original functionInstanceRecord
	t.Run("durable_reservation_and_restart", func(t *testing.T) {
		original, err = s.reserveFunctionInstance(ctx, op, worker, p)
		if err != nil || original.Scope.Validate() != nil || original.State != "provisioning" || original.VMUID != nil || original.SecretName != original.VMName+"-bootstrap" {
			t.Fatal("first reservation rejected", err)
		}
		restarted := &server{db: db}
		replayed, e := restarted.reserveFunctionInstance(ctx, op, worker, p)
		if e != nil || replayed.Scope != original.Scope || replayed.VMName != original.VMName {
			t.Fatal("restart allocated a different candidate", e)
		}
		var state string
		if db.QueryRow(ctx, `SELECT state FROM function_deployments WHERE id=$1`, p.DeploymentID).Scan(&state) != nil || state != "building" {
			t.Fatal("deployment phase not durable")
		}
	})
	if original.Scope.InstanceID == "" {
		t.Fatal("reservation prerequisite failed")
	}
	t.Run("concurrent_replay_one_identity", func(t *testing.T) {
		var wg sync.WaitGroup
		failures := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				item, e := s.reserveFunctionInstance(ctx, op, worker, p)
				if e != nil || item.Scope != original.Scope {
					failures <- errors.New("concurrent replay changed candidate identity")
				}
			}()
		}
		wg.Wait()
		close(failures)
		for e := range failures {
			t.Fatal(e)
		}
		var count int
		if db.QueryRow(ctx, `SELECT count(*) FROM function_instances WHERE function_id=$1`, p.FunctionID).Scan(&count) != nil || count != 1 {
			t.Fatal("duplicate durable candidate")
		}
	})
	t.Run("stale_owner_and_wrong_scope", func(t *testing.T) {
		if _, e := s.reserveFunctionInstance(ctx, op, "stale_worker", p); !errors.Is(e, errLeaseLost) {
			t.Fatal("stale Worker admitted", e)
		}
		wrong := p
		wrong.ProjectID = "prj_ffffffffffffffff"
		if _, e := s.reserveFunctionInstance(ctx, op, worker, wrong); !errors.Is(e, errLeaseLost) {
			t.Fatal("foreign project Operation admitted", e)
		}
		wrong = p
		wrong.BranchID = "br_ffffffffffffffff"
		if _, e := s.reserveFunctionInstance(ctx, op, worker, wrong); !errors.Is(e, errFunctionInstanceConflict) {
			t.Fatal("foreign branch candidate admitted", e)
		}
	})
	t.Run("partial_resource_observation_and_no_uid_replacement", func(t *testing.T) {
		service := "11111111-1111-1111-1111-111111111111"
		vm := "22222222-2222-2222-2222-222222222222"
		item, e := s.recordFunctionInstanceResources(ctx, op, worker, p, original.Scope.InstanceID, "", service)
		if e != nil || item.ServiceUID == nil || *item.ServiceUID != service || item.VMUID != nil || item.State != "provisioning" {
			t.Fatal("partial observation not durable", e)
		}
		item, e = (&server{db: db}).recordFunctionInstanceResources(ctx, op, worker, p, original.Scope.InstanceID, vm, service)
		if e != nil || item.VMUID == nil || *item.VMUID != vm || item.State != "starting" {
			t.Fatal("ownership recovery after restart failed", e)
		}
		if _, e = s.recordFunctionInstanceResources(ctx, op, worker, p, original.Scope.InstanceID, "33333333-3333-3333-3333-333333333333", ""); !errors.Is(e, errFunctionInstanceConflict) {
			t.Fatal("replacement VM UID adopted", e)
		}
		if _, e = s.recordFunctionInstanceResources(ctx, op, worker, p, original.Scope.InstanceID, "", "not-a-uid"); !errors.Is(e, errFunctionInstanceConflict) {
			t.Fatal("invalid UID admitted", e)
		}
		if _, e = s.recordFunctionInstanceResources(ctx, op, "stale_worker", p, original.Scope.InstanceID, vm, service); !errors.Is(e, errLeaseLost) {
			t.Fatal("stale ownership write admitted", e)
		}
	})
	t.Run("lease_expires_while_waiting_on_definition_lock", func(t *testing.T) {
		blocker, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer blocker.Rollback(ctx)
		if _, e = blocker.Exec(ctx, `SELECT id FROM function_definitions WHERE id=$1 FOR UPDATE`, p.FunctionID); e != nil {
			t.Fatal(e)
		}
		exec(`UPDATE operations SET lease_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, op)
		finished := make(chan error, 1)
		go func() { _, e := s.reserveFunctionInstance(ctx, op, worker, p); finished <- e }()
		waiting := false
		for i := 0; i < 40; i++ {
			if admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%FROM function_definitions f%')`, schema).Scan(&waiting) != nil {
				t.Fatal("lock observation failed")
			}
			if waiting {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("test did not observe a real candidate row-lock wait")
		}
		time.Sleep(2100 * time.Millisecond)
		if blocker.Commit(ctx) != nil {
			t.Fatal("blocker release failed")
		}
		if e = <-finished; !errors.Is(e, errLeaseLost) {
			t.Fatal("transaction-start clock admitted expired lease", e)
		}
		exec(`UPDATE operations SET lease_expires_at=now()+interval '1 hour' WHERE id=$1`, op)
	})
	t.Run("failed_candidate_holds_capacity", func(t *testing.T) {
		second, e := s.reserveFunctionInstance(ctx, "op_fn_store_2", worker, targets[1])
		if e != nil {
			t.Fatal(e)
		}
		exec(`UPDATE function_instances SET state='failed' WHERE id=$1`, second.Scope.InstanceID)
		if _, e = s.reserveFunctionInstance(ctx, "op_fn_store_3", worker, targets[2]); !errors.Is(e, errFunctionInstanceCapacity) {
			t.Fatal("failed physical candidate released budget", e)
		}
		if _, e = s.reserveFunctionInstance(ctx, "op_fn_store_2", worker, targets[1]); !errors.Is(e, errFunctionInstanceConflict) {
			t.Fatal("failed instance silently replaced", e)
		}
	})
	t.Run("generation_advance_stops_original_candidate", func(t *testing.T) {
		exec(`UPDATE function_definitions SET generation=generation+1 WHERE id=$1`, p.FunctionID)
		if _, e := s.reserveFunctionInstance(ctx, op, worker, p); !errors.Is(e, errFunctionInstanceConflict) {
			t.Fatal("old generation admitted", e)
		}
		if _, e := s.recordFunctionInstanceResources(ctx, op, worker, p, original.Scope.InstanceID, "22222222-2222-2222-2222-222222222222", ""); !errors.Is(e, errFunctionInstanceConflict) {
			t.Fatal("old generation ownership write admitted", e)
		}
	})
}
