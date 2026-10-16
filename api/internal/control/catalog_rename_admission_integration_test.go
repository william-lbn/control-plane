package control

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
	"time"
)

func TestCatalogRenameAdmissionAuthorizationConflictAuditAndMetadataReplay(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_catalog_rename_" + randomToken(6)
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	p := renamePayloadForTest()
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('rename_admin','rename_admin','unused','owner'),('rename_viewer','rename_viewer','unused','viewer'),('rename_foreign','rename_foreign','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('rename_org','rename-org','Rename')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('rename_org','rename_admin','admin'),('rename_org','rename_viewer','viewer')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'rename_org','Rename','lab',16,'ready','managed',now(),now())`, p.ProjectID)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now())`, p.BranchID, p.ProjectID)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',now(),now())`, p.EndpointID, p.ProjectID, p.BranchID)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES($1,$2,'ready',$3)`, p.BranchID, p.Name, p.CredentialRef)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES($1,'owned_database',$2,'ready')`, p.BranchID, p.Name)
	admit := func(value catalogRenamePayload, actor string, want int) string {
		t.Helper()
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		op, e := admitCatalogRenameTx(ctx, tx, value, actor, "rename-request", keyHash("rename-original-key"), "request-hash")
		if want != 0 {
			var fault *catalogRenameFault
			if !errors.As(e, &fault) || fault.Status != want {
				t.Fatalf("admission rejected incorrectly: %v, want %d", e, want)
			}
			return ""
		}
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		return op
	}
	admit(p, "rename_viewer", 403)
	admit(p, "rename_foreign", 403)
	exec(`UPDATE users SET disabled=true WHERE id='rename_admin'`)
	admit(p, "rename_admin", 403)
	exec(`UPDATE users SET disabled=false WHERE id='rename_admin'`)
	stale := p
	stale.ExpectedVersion = 2
	admit(stale, "rename_admin", 412)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES($1,$2,'deleted','retained-target')`, p.BranchID, p.NewName)
	admit(p, "rename_admin", 409)
	exec(`DELETE FROM branch_roles WHERE branch_id=$1 AND name=$2`, p.BranchID, p.NewName)
	extra := p
	extra.ConfigUIDs = map[string]string{p.EndpointID: p.ConfigUIDs[p.EndpointID], "ep_1123456789abcdef": "31234567-89ab-cdef-0123-456789abcdef"}
	admit(extra, "rename_admin", 409)
	// Audit/intent/idempotency must roll back together, including the source's
	// updating state. The injected failure exists only in this isolated schema.
	exec(`CREATE FUNCTION reject_rename_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_rename_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_rename_audit()`)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admitCatalogRenameTx(ctx, tx, p, "rename_admin", "request", "key", "hash")
	if err == nil {
		t.Fatal("audit failure did not abort intent")
	}
	_ = tx.Rollback(ctx)
	var state string
	var operations int
	if err = db.QueryRow(ctx, `SELECT state FROM branch_roles WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name).Scan(&state); err != nil || state != "ready" {
		t.Fatal("failed audit left partial source state", err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM operations`).Scan(&operations); err != nil || operations != 0 {
		t.Fatal("failed audit left operation", err)
	}
	exec(`DROP TRIGGER reject_rename_audit ON audit_events`)
	op := admit(p, "rename_admin", 0)
	if op == "" {
		t.Fatal("durable rename operation absent")
	}
	admit(p, "rename_admin", 409)
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = commitCatalogRenameTx(ctx, tx, p); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = commitCatalogRenameTx(ctx, tx, p); err != nil {
		t.Fatal("lost metadata commit not recovered", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var version int64
	var credential, owner string
	if err = db.QueryRow(ctx, `SELECT version,credential_ref FROM branch_roles WHERE branch_id=$1 AND name=$2`, p.BranchID, p.NewName).Scan(&version, &credential); err != nil || version != 2 || credential != p.CredentialRef {
		t.Fatal("rename changed credential or replay version", err)
	}
	if err = db.QueryRow(ctx, `SELECT owner_name,version FROM branch_databases WHERE branch_id=$1 AND name='owned_database'`, p.BranchID).Scan(&owner, &version); err != nil || owner != p.NewName || version != 2 {
		t.Fatal("owner references not atomically renamed", err)
	}
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES($1,$2,'ready','independent-role')`, p.BranchID, p.Name)
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = commitCatalogRenameTx(ctx, tx, p); err == nil {
		t.Fatal("recreated original metadata name adopted")
	}
	_ = tx.Rollback(ctx)
}
