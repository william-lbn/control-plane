package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCatalogMutationOriginalResultAtomicAndNoExternalReplay(t *testing.T) {
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
	schema := "v2_catalog_result_" + randomToken(6)
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
	const project = "prj_0123456789abcdef"
	const branch = "br_0123456789abcdef"
	const endpoint = "ep_0123456789abcdef"
	const actor = "catalog_result_admin"
	const worker = "original-catalog-worker"
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('catalog_result_org','catalog-result-org','Catalog result')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('catalog_result_org')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('catalog_result_org',$1,'admin')`, actor)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES($1,'catalog_result_org','Catalog','lab',16,'ready','managed',$2,now(),now())`, project, strings.Repeat("a", 32))
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'main',$3,'ready',now())`, branch, project, strings.Repeat("b", 32))
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','postgres','read_write',now(),now())`, endpoint, project, branch)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{4}, 32)} // nil Kube and no Proxy: result recovery must never touch either.
	actions := []string{"create_role", "rotate_role_password", "delete_role", "create_database", "update_database_owner", "delete_database"}
	for index, action := range actions {
		t.Run(action, func(t *testing.T) {
			roleAction := strings.Contains(action, "role")
			table := "branch_databases"
			initialState := "creating"
			if strings.HasPrefix(action, "delete_") {
				initialState = "deleting"
			} else if action == "rotate_role_password" || action == "update_database_owner" {
				initialState = "updating"
			}
			p := catalogPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint, Name: fmt.Sprintf("receipt_object_%d", index), ExpectedVersion: 7}
			if roleAction {
				table = "branch_roles"
				p.CredentialRef = "original-result-credential"
				p.MutationCredentialUID = "11111111-1111-4111-8111-111111111111"
				ref := p.CredentialRef
				if action != "create_role" {
					p.PreviousCredentialRef = "previous-result-credential"
					ref = p.PreviousCredentialRef
				}
				if action == "delete_role" {
					p.CredentialRef = ""
					p.MutationCredentialUID = ""
				}
				exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,$2,$3,$4,7)`, branch, p.Name, initialState, ref)
			} else {
				p.Owner = "new_owner"
				owner := p.Owner
				if action == "update_database_owner" {
					p.PreviousOwner = "previous_owner"
					p.DatabaseOID = 12345
					owner = p.PreviousOwner
				}
				exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state,version) VALUES($1,$2,$3,$4,7)`, branch, p.Name, owner, initialState)
			}
			operation := fmt.Sprintf("op_%024x", index+1)
			raw, _ := json.Marshal(p)
			exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'branch_catalog',$3,$4,'running',$5,$1,$6,$7,now()+interval '5 minutes')`, operation, project, branch, action, actor, raw, worker)
			for ordinal, name := range []string{"apply_postgres_catalog", "persist_all_compute_specs", "publish_all_proxy_credentials", "verify_catalog_and_commit"} {
				exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, operation, ordinal, name)
			}
			commit := func(value catalogPayload, lease string) error {
				tx, e := db.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(ctx)
				if e = commitCatalogMutationResultTx(ctx, tx, operation, lease, action, value); e != nil {
					return e
				}
				return tx.Commit(ctx)
			}
			if err = commit(p, "foreign-worker"); err == nil {
				t.Fatal("foreign worker committed catalog result")
			}
			wrong := p
			wrong.ExpectedVersion++
			if err = commit(wrong, worker); err == nil {
				t.Fatal("changed original payload accepted")
			}
			if index == 0 {
				exec(`UPDATE projects SET state='deleting' WHERE id=$1`, project)
				if err = commit(p, worker); err == nil {
					t.Fatal("retiring parent accepted fresh catalog result")
				}
				exec(`UPDATE projects SET state='ready' WHERE id=$1`, project)
				exec(`UPDATE branch_roles SET credential_ref='replacement-credential' WHERE branch_id=$1 AND name=$2`, branch, p.Name)
				if err = commit(p, worker); err == nil {
					t.Fatal("different credential generation committed")
				}
				exec(`UPDATE branch_roles SET credential_ref=$3 WHERE branch_id=$1 AND name=$2`, branch, p.Name, p.CredentialRef)
			}
			exec(`CREATE FUNCTION reject_catalog_result_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_catalog_result_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_catalog_result_audit()`)
			if err = commit(p, worker); err == nil {
				t.Fatal("audit failure committed catalog metadata")
			}
			exec(`DROP TRIGGER reject_catalog_result_audit ON audit_events; DROP FUNCTION reject_catalog_result_audit()`)
			var version int64
			var count int
			if err = db.QueryRow(ctx, "SELECT version FROM "+table+" WHERE branch_id=$1 AND name=$2", branch, p.Name).Scan(&version); err != nil || version != 7 {
				t.Fatalf("metadata did not roll back %d %v", version, err)
			}
			if err = db.QueryRow(ctx, `SELECT count(*) FROM catalog_mutation_results WHERE operation_id=$1`, operation).Scan(&count); err != nil || count != 0 {
				t.Fatal("result did not roll back", err)
			}
			if err = commit(p, worker); err != nil {
				t.Fatal(err)
			}
			if err = commit(p, worker); err != nil {
				t.Fatal("same original result not idempotent", err)
			}
			var result map[string]any
			var resultRaw []byte
			if err = db.QueryRow(ctx, `SELECT result FROM catalog_mutation_results WHERE operation_id=$1`, operation).Scan(&resultRaw); err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(resultRaw, &result) != nil || result["version_token"] != "8" || strings.Contains(string(resultRaw), "credential") {
				t.Fatal("unsafe or wrong result", string(resultRaw))
			}
			if _, err = db.Exec(ctx, `UPDATE catalog_mutation_results SET action='delete_role' WHERE operation_id=$1`, operation); err == nil {
				t.Fatal("committed result was mutable")
			}
			if _, err = db.Exec(ctx, `DELETE FROM catalog_mutation_results WHERE operation_id=$1`, operation); err == nil {
				t.Fatal("committed result was deletable")
			}
			// Simulate a later authorized generation and a missing outer settlement.
			// The old result must settle without inspecting or rewriting this state.
			exec("UPDATE "+table+" SET state='ready',version=17 WHERE branch_id=$1 AND name=$2", branch, p.Name)
			if roleAction {
				exec(`UPDATE branch_roles SET credential_ref='later-authorized-credential' WHERE branch_id=$1 AND name=$2`, branch, p.Name)
			} else {
				exec(`UPDATE branch_databases SET owner_name='later_authorized_owner' WHERE branch_id=$1 AND name=$2`, branch, p.Name)
			}
			exec(`UPDATE endpoints SET deleted_at=now() WHERE id=$1`, endpoint)
			if err = s.reconcileCatalog(ctx, operation, "foreign-worker", action, p); err == nil {
				t.Fatal("foreign recovery lease accepted")
			}
			if err = s.reconcileCatalog(ctx, operation, worker, action, p); err != nil {
				t.Fatal("original result recovery touched an external dependency or later intent", err)
			}
			exec(`UPDATE endpoints SET deleted_at=NULL WHERE id=$1`, endpoint)
			if err = db.QueryRow(ctx, "SELECT version FROM "+table+" WHERE branch_id=$1 AND name=$2", branch, p.Name).Scan(&version); err != nil || version != 17 {
				t.Fatal("old result advanced later generation", err)
			}
			if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE request_id=$1 AND outcome='succeeded'`, operation).Scan(&count); err != nil || count != 1 {
				t.Fatal("original result duplicated audit", err, count)
			}
			if err = db.QueryRow(ctx, `SELECT count(*) FROM operation_steps WHERE operation_id=$1 AND state='succeeded'`, operation).Scan(&count); err != nil || count != 4 {
				t.Fatal("original steps not recovered", err, count)
			}
			if action == "rotate_role_password" {
				key := "catalog-original-replay-key"
				target := "/api/v1/projects/" + project + "/branches/" + branch + "/roles/" + p.Name
				hash := s.createRequestHash("PATCH:"+target, record{"name": "", "password": "protected-original-password"})
				exec(`INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, actor, keyHash(key), hash, operation)
				exec(`UPDATE endpoints SET deleted_at=now() WHERE id=$1`, endpoint)
				request := httptest.NewRequest("PATCH", target, strings.NewReader(`{"password":"protected-original-password"}`))
				request.SetPathValue("project", project)
				request.SetPathValue("branch", branch)
				request.SetPathValue("role", p.Name)
				request.Header.Set("Idempotency-Key", key)
				request = request.WithContext(context.WithValue(request.Context(), userKey, user{ID: actor}))
				response := httptest.NewRecorder()
				s.catalogMutation(response, request)
				if response.Code != 202 {
					t.Fatalf("original HTTP receipt required current Writer: %d %s", response.Code, response.Body.String())
				}
				exec(`UPDATE endpoints SET deleted_at=NULL WHERE id=$1`, endpoint)
			}
			exec(`UPDATE operations SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL,finished_at=now() WHERE id=$1`, operation)
		})
	}
}
