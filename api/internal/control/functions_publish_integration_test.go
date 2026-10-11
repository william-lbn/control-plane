package control

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

// These are actual metadata transactions, not actual Kube/guest/SQL readiness
// proofs. Fixture UUIDs and signed-status-shaped values never enter lab metadata.
func TestFunctionPublicationAtomicReplayAndReadiness(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := randomToken(8)
	schema := "v2_fn_publish_" + suffix
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
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	p := functionInstanceTarget{"fnc_" + suffix, "prj_" + suffix, "br_" + suffix, "fdp_" + suffix, 1, "hello"}
	endpoint := "ep_" + suffix
	op, worker := "op_fn_publish_ci", "fn_publish_worker"
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('fn_pub_actor','fn_pub_actor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('fn_pub_org','fn-pub','Publication CI')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'fn_pub_org','Functions','rke2-lab',16,'ready','managed',now(),now())`, p.ProjectID)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now())`, p.BranchID, p.ProjectID)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',now(),now())`, endpoint, p.ProjectID, p.BranchID)
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'fn_pub_org',$2,$3,$4,'hello','neondb','app',$5,'provisioning')`, p.FunctionID, p.ProjectID, p.BranchID, endpoint, "fn_"+suffix)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'function',$3,'deploy_function','running','fn_pub_actor','fn_ci','{}',$4,now()+interval '1 hour')`, op, p.ProjectID, p.FunctionID, worker)
	digest := strings.Repeat("a", 64)
	key, _ := functionArtifactKey(p.ProjectID, p.BranchID, digest)
	env := functionEnvironmentReference{Namespace: "neon-ci", ProjectID: p.ProjectID, BranchID: p.BranchID, DeploymentID: p.DeploymentID, Name: "fdp-" + suffix + "-environment", UID: "11111111-2222-3333-4444-555555555555", MAC: strings.Repeat("b", 64)}
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id) VALUES($1,$2,$3,$4,'nodejs24',$5,100,'index.mjs',$6,$7,'fn_pub_actor',$8)`, p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID, digest, key, env.Name, op)
	exec(`UPDATE function_definitions SET target_deployment_id=$2 WHERE id=$1`, p.FunctionID, p.DeploymentID)
	backing := functionDeploymentBacking{functionArtifactReference{p.ProjectID, p.BranchID, digest, 100, "index.mjs", key}, env, "ci-v1"}
	if _, err = s.recordFunctionDeploymentBacking(ctx, op, worker, p, backing); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.reserveFunctionInstance(ctx, op, worker, p)
	if err != nil {
		t.Fatal(err)
	}
	vmUID, serviceUID, bootstrapUID := "22222222-3333-4444-5555-666666666666", "33333333-4444-5555-6666-777777777777", "44444444-5555-6666-7777-888888888888"
	if _, err = s.recordFunctionInstanceResources(ctx, op, worker, p, candidate.Scope.InstanceID, vmUID, serviceUID); err != nil {
		t.Fatal(err)
	}
	observation := functionReadyObservation{Status: functions.InstanceStatus{Scope: candidate.Scope, BootID: strings.Repeat("c", 32), Runtime: functions.Runtime, NodeReady: true, LastActivity: time.Now()}, VMUID: vmUID, ServiceUID: serviceUID, BootstrapUID: bootstrapUID, ObservedAt: time.Now()}
	t.Run("missing_bootstrap_receipt_does_not_publish", func(t *testing.T) {
		if _, e := s.publishFunctionInstance(ctx, op, worker, p, observation); !errors.Is(e, errFunctionPublication) {
			t.Fatal("missing receipt admitted")
		}
	})
	ref := functionBootstrapReference{Namespace: "neon-ci", Name: candidate.SecretName, UID: bootstrapUID, MAC: strings.Repeat("d", 64), MACKeyVersion: "ci-v1", FunctionID: p.FunctionID, WriterID: endpoint, BundleDigest: digest, Scope: candidate.Scope}
	if _, err = s.recordFunctionBootstrap(ctx, op, worker, p, ref); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name   string
		change func(*functionReadyObservation)
	}{
		{"foreign_vm", func(o *functionReadyObservation) { o.VMUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" }},
		{"foreign_service", func(o *functionReadyObservation) { o.ServiceUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" }},
		{"foreign_bootstrap", func(o *functionReadyObservation) { o.BootstrapUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" }},
		{"foreign_branch", func(o *functionReadyObservation) { o.Status.Scope.BranchID = "br_aaaaaaaaaaaaaaaa" }},
		{"wrong_generation", func(o *functionReadyObservation) { o.Status.Scope.Generation++ }},
		{"wrong_boot", func(o *functionReadyObservation) { o.Status.BootID = "unknown" }},
		{"node_not_ready", func(o *functionReadyObservation) { o.Status.NodeReady = false }},
		{"draining", func(o *functionReadyObservation) { o.Status.Draining = true }},
		{"untrusted_counters", func(o *functionReadyObservation) { o.Status.BackgroundCountersTrusted = true }},
		{"expired_observation", func(o *functionReadyObservation) {
			o.ObservedAt = time.Now().Add(-time.Minute)
			o.Status.LastActivity = o.ObservedAt
		}},
		{"future_observation", func(o *functionReadyObservation) { o.ObservedAt = time.Now().Add(time.Minute) }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			o := observation
			entry.change(&o)
			if _, e := s.publishFunctionInstance(ctx, op, worker, p, o); !errors.Is(e, errFunctionPublication) {
				t.Fatal("invalid readiness published", e)
			}
		})
	}
	t.Run("stale_worker", func(t *testing.T) {
		if _, e := s.publishFunctionInstance(ctx, op, "old-worker", p, observation); !errors.Is(e, errLeaseLost) {
			t.Fatal("old Worker published")
		}
	})
	t.Run("lease_expires_during_real_parent_lock_wait", func(t *testing.T) {
		blocker, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer blocker.Rollback(ctx)
		if _, e = blocker.Exec(ctx, `SELECT id FROM function_definitions WHERE id=$1 FOR UPDATE`, p.FunctionID); e != nil {
			t.Fatal(e)
		}
		exec(`UPDATE operations SET lease_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, op)
		current := observation
		current.ObservedAt = time.Now()
		finished := make(chan error, 1)
		go func() { _, e := s.publishFunctionInstance(ctx, op, worker, p, current); finished <- e }()
		waiting := false
		for i := 0; i < 40; i++ {
			if e = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%FROM function_definitions f%')`, schema).Scan(&waiting); e != nil {
				t.Fatal(e)
			}
			if waiting {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("publication did not enter a real parent row-lock wait")
		}
		time.Sleep(2100 * time.Millisecond)
		if e = blocker.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		if e = <-finished; !errors.Is(e, errLeaseLost) {
			t.Fatal("expired lease published after waiting", e)
		}
		var candidateState, deploymentState, target string
		if e = db.QueryRow(ctx, `SELECT i.state,d.state,f.target_deployment_id FROM function_instances i JOIN function_deployments d ON d.id=i.deployment_id JOIN function_definitions f ON f.id=i.function_id WHERE i.id=$1`, candidate.Scope.InstanceID).Scan(&candidateState, &deploymentState, &target); e != nil || candidateState != "starting" || deploymentState != "building" || target != p.DeploymentID {
			t.Fatal("expired lease left partial publication")
		}
		exec(`UPDATE operations SET lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, op)
	})
	oldDeployment, oldInstance := "fdp_aaaaaaaaaaaaaaaa", "fni_aaaaaaaaaaaaaaaa"
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('op_fn_pub_old',$1,'function',$2,'deploy_function','succeeded','fn_pub_actor','old','{}')`, p.ProjectID, p.FunctionID)
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,state,actor_id,operation_id,finished_at) VALUES($1,$2,$3,$4,'nodejs24',$5,100,'index.mjs',$6,'fdp-aaaaaaaaaaaaaaaa-environment','completed','fn_pub_actor','op_fn_pub_old',now())`, oldDeployment, p.FunctionID, p.ProjectID, p.BranchID, digest, key)
	exec(`INSERT INTO function_instances(id,function_id,deployment_id,project_id,branch_id,generation,vm_name,service_name,bootstrap_secret_ref,vm_uid,service_uid,boot_id,state) VALUES($1,$2,$3,$4,$5,1,'fn-aaaaaaaaaaaaaaaa','fn-aaaaaaaaaaaaaaaa','fn-aaaaaaaaaaaaaaaa-bootstrap',$6,$7,$8,'ready')`, oldInstance, p.FunctionID, oldDeployment, p.ProjectID, p.BranchID, vmUID, serviceUID, strings.Repeat("e", 32))
	exec(`UPDATE function_definitions SET active_deployment_id=$2,state='active' WHERE id=$1`, p.FunctionID, oldDeployment)
	for _, state := range []string{"ready", "draining", "failed"} {
		t.Run("old_"+state+"_retains_pointer_and_budget", func(t *testing.T) {
			exec(`UPDATE function_instances SET state=$2 WHERE id=$1`, oldInstance, state)
			if _, e := s.publishFunctionInstance(ctx, op, worker, p, observation); !errors.Is(e, errFunctionPublication) {
				t.Fatal("old nonretired instance silently replaced")
			}
			var active, candidateState, deploymentState string
			if e := db.QueryRow(ctx, `SELECT f.active_deployment_id,i.state,d.state FROM function_definitions f JOIN function_instances i ON i.id=$2 JOIN function_deployments d ON d.id=$3 WHERE f.id=$1`, p.FunctionID, candidate.Scope.InstanceID, p.DeploymentID).Scan(&active, &candidateState, &deploymentState); e != nil || active != oldDeployment || candidateState != "starting" || deploymentState != "building" {
				t.Fatal("failed admission mutated publication")
			}
		})
	}
	// Only a metadata fixture retirement: no external VM absence is asserted.
	exec(`UPDATE function_instances SET state='retired',retired_at=now() WHERE id=$1`, oldInstance)
	observation.ObservedAt = time.Now()
	t.Run("atomic_publication_and_original_replay", func(t *testing.T) {
		replay, e := s.publishFunctionInstance(ctx, op, worker, p, observation)
		if e != nil || replay {
			t.Fatal("first publication failed", e)
		}
		var active, target, state, deploymentState, boot string
		if e = db.QueryRow(ctx, `SELECT f.active_deployment_id,COALESCE(f.target_deployment_id,''),i.state,d.state,i.boot_id FROM function_definitions f JOIN function_instances i ON i.id=$2 JOIN function_deployments d ON d.id=$3 WHERE f.id=$1`, p.FunctionID, candidate.Scope.InstanceID, p.DeploymentID).Scan(&active, &target, &state, &deploymentState, &boot); e != nil || active != p.DeploymentID || target != "" || state != "ready" || deploymentState != "completed" || boot != observation.Status.BootID {
			t.Fatal("partial atomic publication")
		}
		restarted := &server{db: db}
		oldObservation := observation
		oldObservation.ObservedAt = time.Now().Add(-time.Hour)
		oldObservation.Status.LastActivity = oldObservation.ObservedAt
		replay, e = restarted.publishFunctionInstance(ctx, op, worker, p, oldObservation)
		if e != nil || !replay {
			t.Fatal("unknown commit/restart did not replay the original publication", e)
		}
		replaced := observation
		replaced.Status.BootID = strings.Repeat("f", 32)
		if _, e = restarted.publishFunctionInstance(ctx, op, worker, p, replaced); !errors.Is(e, errFunctionPublication) {
			t.Fatal("new boot silently adopted")
		}
	})
	t.Run("concurrent_replays_keep_one_published_identity", func(t *testing.T) {
		finished := make(chan error, 8)
		for i := 0; i < 8; i++ {
			go func() {
				replay, e := (&server{db: db}).publishFunctionInstance(ctx, op, worker, p, observation)
				if e == nil && !replay {
					e = errors.New("replay became new publication")
				}
				finished <- e
			}()
		}
		for i := 0; i < 8; i++ {
			if e := <-finished; e != nil {
				t.Fatal(e)
			}
		}
		var live int
		if e := db.QueryRow(ctx, `SELECT count(*) FROM function_instances WHERE function_id=$1 AND state='ready' AND boot_id=$2`, p.FunctionID, observation.Status.BootID).Scan(&live); e != nil || live != 1 {
			t.Fatal("replay changed current instance count")
		}
	})
	t.Run("completed_operation_cannot_reenter_publication", func(t *testing.T) {
		exec(`UPDATE operations SET state='succeeded' WHERE id=$1`, op)
		if _, e := s.publishFunctionInstance(ctx, op, worker, p, observation); !errors.Is(e, errLeaseLost) {
			t.Fatal("terminal Operation reentered publication")
		}
		exec(`UPDATE operations SET state='running' WHERE id=$1`, op)
	})
	t.Run("new_generation_closes_old_replay", func(t *testing.T) {
		exec(`UPDATE function_definitions SET generation=generation+1 WHERE id=$1`, p.FunctionID)
		if _, e := s.publishFunctionInstance(ctx, op, worker, p, observation); !errors.Is(e, errFunctionPublication) {
			t.Fatal("obsolete publication replay admitted")
		}
	})
}
