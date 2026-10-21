package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCatalogRenameOriginalResultAtomicAndNoExternalReplay(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_rename_result_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgQuote(schema)); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgQuote(schema)+" CASCADE")
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
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	base := renamePayloadForTest()
	const actor = "rename_result_admin"
	const worker = "original-rename-worker"
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('rename_result_org','rename-result-org','Rename result')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('rename_result_org',$1,'admin')`, actor)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES($1,'rename_result_org','Rename','lab',16,'ready','managed',$2,now(),now())`, base.ProjectID, strings.Repeat("a", 32))
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'main',$3,'ready',now())`, base.BranchID, base.ProjectID, strings.Repeat("b", 32))
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','postgres','read_write',now(),now())`, base.EndpointID, base.ProjectID, base.BranchID)
	s := &server{db: db} // no Kube client or Proxy; original committed recovery must not access either.
	for index, kind := range []string{"role", "database"} {
		t.Run(kind, func(t *testing.T) {
			p := base
			p.Kind = kind
			p.Name = "old_" + kind
			p.NewName = "new_" + kind
			table, reference := "branch_roles", "credential_ref"
			expected := p.CredentialRef
			if kind == "database" {
				table, reference = "branch_databases", "owner_name"
				p.CredentialRef = ""
				p.CredentialUID = ""
				p.Owner = "managed_owner"
				expected = p.Owner
			}
			exec("INSERT INTO "+table+"(branch_id,name,state,"+reference+") VALUES($1,$2,'updating',$3)", p.BranchID, p.Name, expected)
			if kind == "role" {
				exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES($1,'follower_database',$2,'ready')`, p.BranchID, p.Name)
			}
			operation := fmt.Sprintf("op_%024x", index+30)
			raw, _ := json.Marshal(p)
			exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'branch_catalog',$3,$4,'running',$5,$1,$6,$7,now()+interval '5 minutes')`, operation, p.ProjectID, p.BranchID, "rename_"+kind, actor, raw, worker)
			for ordinal, name := range []string{"verify_original_compute_backings", "rename_postgres_original_oid", "persist_all_compute_catalogs", "publish_proxy_credentials", "verify_oid_and_commit_metadata"} {
				exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, operation, ordinal, name)
			}
			commit := func(value catalogRenamePayload, lease string) error {
				tx, e := db.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(ctx)
				if e = commitCatalogRenameResultTx(ctx, tx, operation, lease, value); e != nil {
					return e
				}
				return tx.Commit(ctx)
			}
			if err = commit(p, "foreign-worker"); err == nil {
				t.Fatal("foreign lease committed rename")
			}
			wrong := p
			wrong.ObjectOID++
			if err = commit(wrong, worker); err == nil {
				t.Fatal("changed original OID accepted")
			}
			wrong = p
			wrong.ConfigUIDs = map[string]string{p.EndpointID: "99999999-9999-4999-8999-999999999999"}
			if err = commit(wrong, worker); err == nil {
				t.Fatal("changed original backing binding accepted")
			}
			exec(`UPDATE users SET disabled=true WHERE id=$1`, actor)
			if err = commit(p, worker); err == nil {
				t.Fatal("revoked actor accepted fresh rename result")
			}
			exec(`UPDATE users SET disabled=false WHERE id=$1`, actor)
			exec(`UPDATE projects SET state='deleting' WHERE id=$1`, p.ProjectID)
			if err = commit(p, worker); err == nil {
				t.Fatal("retiring parent accepted fresh rename result")
			}
			exec(`UPDATE projects SET state='ready' WHERE id=$1`, p.ProjectID)
			// A legacy final-looking row alone cannot manufacture an original result.
			exec("UPDATE "+table+" SET name=$3,state='ready',version=2 WHERE branch_id=$1 AND name=$2", p.BranchID, p.Name, p.NewName)
			if err = commit(p, worker); err == nil {
				t.Fatal("inferred legacy result from current target name")
			}
			exec("UPDATE "+table+" SET name=$3,state='updating',version=1 WHERE branch_id=$1 AND name=$2", p.BranchID, p.NewName, p.Name)
			exec(`CREATE FUNCTION reject_rename_result_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_rename_result_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_rename_result_audit()`)
			if err = commit(p, worker); err == nil {
				t.Fatal("audit failure committed rename metadata")
			}
			exec(`DROP TRIGGER reject_rename_result_audit ON audit_events; DROP FUNCTION reject_rename_result_audit()`)
			var version int64
			var current string
			var count int
			if err = db.QueryRow(ctx, "SELECT version FROM "+table+" WHERE branch_id=$1 AND name=$2", p.BranchID, p.Name).Scan(&version); err != nil || version != 1 {
				t.Fatal("rename metadata did not roll back", err, version)
			}
			if kind == "role" {
				if err = db.QueryRow(ctx, `SELECT owner_name FROM branch_databases WHERE branch_id=$1 AND name='follower_database'`, p.BranchID).Scan(&current); err != nil || current != p.Name {
					t.Fatal("follower owner did not roll back", err, current)
				}
			}
			if err = db.QueryRow(ctx, `SELECT count(*) FROM catalog_rename_results WHERE operation_id=$1`, operation).Scan(&count); err != nil || count != 0 {
				t.Fatal("original result did not roll back", err)
			}
			if err = commit(p, worker); err != nil {
				t.Fatal(err)
			}
			if err = commit(p, worker); err != nil {
				t.Fatal("original committed result not idempotent", err)
			}
			var resultRaw []byte
			var result map[string]any
			if err = db.QueryRow(ctx, `SELECT result FROM catalog_rename_results WHERE operation_id=$1`, operation).Scan(&resultRaw); err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(resultRaw, &result) != nil || result["version_token"] != "2" || result["name"] != p.NewName || strings.Contains(string(resultRaw), "credential") {
				t.Fatal("unsafe original result", string(resultRaw))
			}
			if _, err = db.Exec(ctx, `UPDATE catalog_rename_results SET kind='role' WHERE operation_id=$1`, operation); err == nil {
				t.Fatal("rename result mutable")
			}
			if _, err = db.Exec(ctx, `DELETE FROM catalog_rename_results WHERE operation_id=$1`, operation); err == nil {
				t.Fatal("rename result deletable")
			}
			// Later rename/rotation, reused old name and a retired original Writer must
			// not cause recovery to reapply the old password or original metadata.
			exec("UPDATE "+table+" SET name=$3,version=9,"+reference+"='later_authorized_identity' WHERE branch_id=$1 AND name=$2", p.BranchID, p.NewName, "later_"+kind)
			exec("INSERT INTO "+table+"(branch_id,name,state,"+reference+") VALUES($1,$2,'ready','independent_identity')", p.BranchID, p.Name)
			exec(`UPDATE endpoints SET deleted_at=now() WHERE id=$1`, p.EndpointID)
			exec(`UPDATE users SET disabled=true WHERE id=$1`, actor)
			if err = s.reconcileCatalogRename(ctx, operation, "foreign-worker", p); err == nil {
				t.Fatal("foreign lease recovered original rename")
			}
			if err = s.reconcileCatalogRename(ctx, operation, worker, p); err != nil {
				t.Fatal("original result recovery touched current SQL/backings or later intent", err)
			}
			if err = db.QueryRow(ctx, "SELECT version,"+reference+" FROM "+table+" WHERE branch_id=$1 AND name=$2", p.BranchID, "later_"+kind).Scan(&version, &current); err != nil || version != 9 || current != "later_authorized_identity" {
				t.Fatal("old result changed later generation", err)
			}
			if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE request_id=$1 AND outcome='succeeded'`, operation).Scan(&count); err != nil || count != 1 {
				t.Fatal("duplicate rename success audit", err, count)
			}
			if err = db.QueryRow(ctx, `SELECT count(*) FROM operation_steps WHERE operation_id=$1 AND state='succeeded'`, operation).Scan(&count); err != nil || count != 5 {
				t.Fatal("original five rename steps not settled", err, count)
			}
			exec(`UPDATE endpoints SET deleted_at=NULL WHERE id=$1`, p.EndpointID)
			exec(`UPDATE users SET disabled=false WHERE id=$1`, actor)
			exec(`UPDATE operations SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL,finished_at=now() WHERE id=$1`, operation)
		})
	}
}
