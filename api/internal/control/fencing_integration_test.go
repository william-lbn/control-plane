package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Run against the same isolated schema as TestMigrateFreshSchema. Its rows are
// deliberately retained so the recovery test can be audited after execution.
func TestOperationLeaseFenceIntegration(t *testing.T) {
	url, schema := os.Getenv("NEON_V2_TEST_DATABASE_URL"), os.Getenv("NEON_V2_TEST_SCHEMA")
	if url == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	if !regexp.MustCompile(`^v2_migration_[a-z0-9_]+$`).MatchString(schema) {
		t.Fatal("NEON_V2_TEST_SCHEMA must be a dedicated v2_migration_* schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	projectID, opID := newID("prj_fence_"), newID("op_fence_")
	_, err = pool.Exec(ctx, `INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at)
		VALUES($1,'local',$1,'test',16,'provisioning','managed',now(),now())`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,
		lease_owner,lease_expires_at) VALUES($1,$2,'project',$2,'create_project','running','test','test','worker_a',now()+interval '45 seconds')`, opID, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state)
		VALUES($1,0,'create_managed_tenant','queued')`, opID); err != nil {
		t.Fatal(err)
	}
	s := &server{db: pool}
	if err = s.setCreateStep(ctx, opID, "worker_a", 0, "running", ""); err != nil {
		t.Fatalf("current owner denied: %v", err)
	}
	if err = s.setCreateStep(ctx, opID, "worker_b", 0, "succeeded", ""); !errors.Is(err, errLeaseLost) {
		t.Fatalf("different worker should be fenced: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE operations SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, opID); err != nil {
		t.Fatal(err)
	}
	if err = s.setCreateStep(ctx, opID, "worker_a", 0, "succeeded", ""); !errors.Is(err, errLeaseLost) {
		t.Fatalf("expired owner should be fenced: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE operations SET lease_owner='worker_b',lease_expires_at=now()+interval '45 seconds'
		WHERE id=$1`, opID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = assertCreateLeaseTx(ctx, tx, opID, "worker_a"); !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale final transaction should be fenced: %v", err)
	}
	if err = assertCreateLeaseTx(ctx, tx, opID, "worker_b"); err != nil {
		t.Fatalf("current final transaction denied: %v", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.setCreateStep(ctx, opID, "worker_b", 0, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts int
	if err = pool.QueryRow(ctx, `SELECT state,attempts FROM operation_steps WHERE operation_id=$1 AND ordinal=0`, opID).
		Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" || attempts != 1 {
		t.Fatalf("unexpected fenced step result: state=%s attempts=%d", state, attempts)
	}
	t.Logf("fence evidence: schema=%s project=%s operation=%s", schema, projectID, opID)
}
