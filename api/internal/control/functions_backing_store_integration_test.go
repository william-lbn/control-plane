package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"testing"
	"time"
)

// Real PostgreSQL durability and constraints only. Disposable UIDs below are
// fixture data, never claimed as proof of a real Kubernetes resource.
func TestFunctionDeploymentBackingPersistenceAndLease(t *testing.T) {
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
	schema := "v2_fn_backing_" + randomToken(8)
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
	s := &server{db: db}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	suffix := randomToken(8)
	p := functionInstanceTarget{"fnc_" + suffix, "prj_" + suffix, "br_" + suffix, "fdp_" + suffix, 1, "hello"}
	env := functionEnvironmentReference{Namespace: "neon-ci", ProjectID: p.ProjectID, BranchID: p.BranchID, DeploymentID: p.DeploymentID, Name: "fdp-" + suffix + "-environment", UID: "11111111-2222-3333-4444-555555555555", MAC: strings.Repeat("b", 64)}
	key, _ := functionArtifactKey(p.ProjectID, p.BranchID, strings.Repeat("a", 64))
	backing := functionDeploymentBacking{functionArtifactReference{p.ProjectID, p.BranchID, strings.Repeat("a", 64), 100, "index.mjs", key}, env, "ci-key-v1"}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('fn_back_actor','fn_back_actor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('fn_back_org','fn-back','Backing CI')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'fn_back_org','Functions','rke2-lab',16,'ready','managed',now(),now())`, p.ProjectID)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now())`, p.BranchID, p.ProjectID)
	endpoint := "ep_" + suffix
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',now(),now())`, endpoint, p.ProjectID, p.BranchID)
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'fn_back_org',$2,$3,$4,'hello','neondb','app',$5,'provisioning')`, p.FunctionID, p.ProjectID, p.BranchID, endpoint, "fn_"+suffix)
	op, worker := "op_fn_back_ci", "fn_back_worker"
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'function',$3,'deploy_function','running','fn_back_actor','fn_ci','{}',$4,now()+interval '1 hour')`, op, p.ProjectID, p.FunctionID, worker)
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id) VALUES($1,$2,$3,$4,'nodejs24',$5,100,'index.mjs',$6,$7,'fn_back_actor',$8)`, p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID, backing.Artifact.Digest, key, env.Name, op)
	exec(`UPDATE function_definitions SET target_deployment_id=$2 WHERE id=$1`, p.FunctionID, p.DeploymentID)
	t.Run("missing_receipt_not_fabricated", func(t *testing.T) {
		if _, err := s.loadFunctionDeploymentBacking(ctx, op, worker, p); !errors.Is(err, errFunctionBackingReceipt) {
			t.Fatal("unverified backing loaded")
		}
	})
	t.Run("durable_exact_replay_and_restart", func(t *testing.T) {
		actual, err := s.recordFunctionDeploymentBacking(ctx, op, worker, p, backing)
		if err != nil || actual != backing {
			t.Fatal("receipt not recorded", err)
		}
		restarted := &server{db: db}
		again, err := restarted.recordFunctionDeploymentBacking(ctx, op, worker, p, backing)
		if err != nil || again != backing {
			t.Fatal("restart changed receipt", err)
		}
		loaded, err := restarted.loadFunctionDeploymentBacking(ctx, op, worker, p)
		if err != nil || loaded != backing {
			t.Fatal("persisted receipt changed", err)
		}
		public, _ := json.Marshal(loaded)
		if string(public) != "{}" {
			t.Fatal("private receipt serialized")
		}
	})
	t.Run("changed_uid_mac_key_version_and_scope_rejected", func(t *testing.T) {
		for _, change := range []func(*functionDeploymentBacking){
			func(b *functionDeploymentBacking) { b.Environment.UID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" },
			func(b *functionDeploymentBacking) { b.Environment.MAC = strings.Repeat("c", 64) },
			func(b *functionDeploymentBacking) { b.MACKeyVersion = "ci-key-v2" },
			func(b *functionDeploymentBacking) { b.Environment.Namespace = "foreign" },
			func(b *functionDeploymentBacking) {
				b.Artifact.Digest = strings.Repeat("d", 64)
				b.Artifact.Key, _ = functionArtifactKey(p.ProjectID, p.BranchID, b.Artifact.Digest)
			},
			func(b *functionDeploymentBacking) { b.Environment.BranchID = "br_ffffffffffffffff" },
		} {
			candidate := backing
			change(&candidate)
			if _, err := s.recordFunctionDeploymentBacking(ctx, op, worker, p, candidate); !errors.Is(err, errFunctionBackingReceipt) {
				t.Fatal("changed receipt adopted")
			}
		}
		current, err := s.loadFunctionDeploymentBacking(ctx, op, worker, p)
		if err != nil || current != backing {
			t.Fatal("original receipt altered")
		}
	})
	t.Run("database_immutability_and_scope_guards", func(t *testing.T) {
		if _, err := db.Exec(ctx, `UPDATE function_deployment_backings SET mac_key_version='other' WHERE deployment_id=$1`, p.DeploymentID); err == nil {
			t.Fatal("direct receipt mutation allowed")
		}
		if _, err := db.Exec(ctx, `UPDATE function_deployment_backings SET created_at=now()+interval '1 second' WHERE deployment_id=$1`, p.DeploymentID); err == nil {
			t.Fatal("receipt history rewritten")
		}
	})
	t.Run("stale_lease_and_parent_retirement_rejected", func(t *testing.T) {
		if _, err := s.recordFunctionDeploymentBacking(ctx, op, "old_worker", p, backing); !errors.Is(err, errLeaseLost) {
			t.Fatal("old worker admitted")
		}
		exec(`UPDATE operations SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, op)
		if _, err := s.loadFunctionDeploymentBacking(ctx, op, worker, p); !errors.Is(err, errLeaseLost) {
			t.Fatal("expired lease read admitted")
		}
		exec(`UPDATE operations SET lease_expires_at=now()+interval '1 hour' WHERE id=$1`, op)
		exec(`UPDATE projects SET state='deleting' WHERE id=$1`, p.ProjectID)
		if _, err := s.recordFunctionDeploymentBacking(ctx, op, worker, p, backing); !errors.Is(err, errFunctionInstanceConflict) {
			t.Fatal("retired parent admitted")
		}
	})
}
