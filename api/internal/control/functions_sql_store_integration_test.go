package control

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

// Dedicated PostgreSQL receipt/lease verification. Fixture UUIDs are not proof
// of real Kubernetes objects; the separate Secret transport gate supplies that.
func TestFunctionSQLCredentialDurabilityAndLease(t *testing.T) {
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
	schema := "v2_fn_sql_" + randomToken(8)
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
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	suffix := randomToken(8)
	p := functionInstanceTarget{FunctionID: "fnc_" + suffix, ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, DeploymentID: "fdp_" + suffix, Generation: 1, Slug: "hello"}
	writer := "ep_" + suffix
	r := functionSQLReference{Namespace: "neon-ci", Name: "fn-" + suffix + "-sql-1", Generation: 1, WriterID: writer, UID: "11111111-2222-3333-4444-555555555555",
		MAC: strings.Repeat("a", 64), MACKeyVersion: "ci-v1", Scope: functionsql.Scope{FunctionID: p.FunctionID, ProjectID: p.ProjectID, BranchID: p.BranchID, Database: "neondb", Schema: "app", ProvisioningRole: "control_probe"}}
	exec(t, `INSERT INTO users(id,username,password_hash,role) VALUES('fn_sql_actor','fn_sql_actor','unused','owner')`)
	exec(t, `INSERT INTO organizations(id,name,slug) VALUES('fn_sql_org','Functions','fn-sql-org')`)
	exec(t, `INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'fn_sql_org','Functions','rke2-lab',16,'ready','managed',now(),now())`, p.ProjectID)
	exec(t, `INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now())`, p.BranchID, p.ProjectID)
	exec(t, `INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',now(),now())`, writer, p.ProjectID, p.BranchID)
	exec(t, `INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'fn_sql_org',$2,$3,$4,'hello','neondb','app',$5,'provisioning')`, p.FunctionID, p.ProjectID, p.BranchID, writer, r.Scope.Role())
	op, worker := "op_fn_sql_ci", "fn_sql_worker"
	exec(t, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'function',$3,'deploy_function','running','fn_sql_actor','fn_ci','{}',$4,now()+interval '1 hour')`, op, p.ProjectID, p.FunctionID, worker)
	key, _ := functionArtifactKey(p.ProjectID, p.BranchID, strings.Repeat("b", 64))
	exec(t, `INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id) VALUES($1,$2,$3,$4,'nodejs24',$5,100,'index.mjs',$6,$7,'fn_sql_actor',$8)`, p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID, strings.Repeat("b", 64), key, "fdp-"+suffix+"-environment", op)
	exec(t, `UPDATE function_definitions SET target_deployment_id=$2 WHERE id=$1`, p.FunctionID, p.DeploymentID)
	t.Run("missing_receipt_not_fabricated", func(t *testing.T) {
		if _, err := s.loadFunctionSQLCredential(ctx, op, worker, p); !errors.Is(err, errFunctionSQLSecret) {
			t.Fatal("missing receipt adopted")
		}
	})
	t.Run("exact_replay_restart_and_private_schema", func(t *testing.T) {
		if got, err := s.recordFunctionSQLCredential(ctx, op, worker, p, r); err != nil || got != r {
			t.Fatal("verified identity not recorded", err)
		}
		restarted := &server{db: db}
		if got, err := restarted.recordFunctionSQLCredential(ctx, op, worker, p, r); err != nil || got != r {
			t.Fatal("same identity replay failed", err)
		}
		if got, err := restarted.loadFunctionSQLCredential(ctx, op, worker, p); err != nil || got != r {
			t.Fatal("restart changed identity", err)
		}
		var count int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='function_sql_credentials' AND column_name IN ('password','verifier','database_url','dsn')`, schema).Scan(&count); err != nil || count != 0 {
			t.Fatal("receipt schema contains credential values")
		}
	})
	for name, change := range map[string]func(*functionSQLReference){
		"uid":         func(v *functionSQLReference) { v.UID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" },
		"mac":         func(v *functionSQLReference) { v.MAC = strings.Repeat("b", 64) },
		"key_version": func(v *functionSQLReference) { v.MACKeyVersion = "ci-v2" },
		"writer":      func(v *functionSQLReference) { v.WriterID = "ep_ffffffffffffffff" },
		"database":    func(v *functionSQLReference) { v.Scope.Database = "other" },
		"schema":      func(v *functionSQLReference) { v.Scope.Schema = "other" },
		"provisioner": func(v *functionSQLReference) { v.Scope.ProvisioningRole = "other" },
		"rotation":    func(v *functionSQLReference) { v.Generation = 2; v.Name = "fn-" + suffix + "-sql-2" },
	} {
		t.Run("reject_changed_"+name, func(t *testing.T) {
			changed := r
			change(&changed)
			if _, err := s.recordFunctionSQLCredential(ctx, op, worker, p, changed); err == nil {
				t.Fatal("immutable identity overwritten")
			}
		})
	}
	t.Run("database_direct_update_guard", func(t *testing.T) {
		if _, err := db.Exec(ctx, `UPDATE function_sql_credentials SET secret_uid='aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee' WHERE function_id=$1`, p.FunctionID); err == nil {
			t.Fatal("database immutable guard bypassed")
		}
	})
	t.Run("wrong_worker_and_expired_lease", func(t *testing.T) {
		if _, err := s.recordFunctionSQLCredential(ctx, op, "foreign", p, r); !errors.Is(err, errLeaseLost) {
			t.Fatal("foreign worker admitted")
		}
		exec(t, `UPDATE operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op)
		if _, err := s.loadFunctionSQLCredential(ctx, op, worker, p); !errors.Is(err, errLeaseLost) {
			t.Fatal("expired lease admitted")
		}
		exec(t, `UPDATE operations SET lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, op)
	})
	t.Run("parent_deletion_blocks_original_credential", func(t *testing.T) {
		exec(t, `UPDATE projects SET state='deleting' WHERE id=$1`, p.ProjectID)
		if _, err := s.loadFunctionSQLCredential(ctx, op, worker, p); err == nil {
			t.Fatal("deleting parent admitted")
		}
		exec(t, `UPDATE projects SET state='ready' WHERE id=$1`, p.ProjectID)
	})
	if got, err := s.loadFunctionSQLCredential(ctx, op, worker, p); err != nil || got != r {
		t.Fatal("negative tests changed original receipt", err)
	}
}
