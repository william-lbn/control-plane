package control

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

func TestCatalogBackendRenamePostgresAtomicScopeAndImmutableHistory(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	p := catalogRenamePayload{catalogPayload: bindingPayload("original_database", 1), Kind: "database", NewName: "renamed_database", ObjectOID: 1234}
	const function = "fnc_0123456789abcdef"
	const operation = "op_catalog_backend_original"
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'branch_catalog',$3,'rename_database','running','binding_admin',$1,'{}','binding_worker',now()+interval '5 minutes')`, operation, p.ProjectID, p.BranchID)
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'binding_org',$2,$3,$4,'backendrename',$5,'app','fn_0123456789abcdef','suspended')`, function, p.ProjectID, p.BranchID, p.EndpointID, p.Name)
	old := functionSQLReference{Namespace: "neon-ci", Name: "fn-0123456789abcdef-sql-1", UID: "11111111-1111-4111-8111-111111111111", MAC: strings.Repeat("a", 64), MACKeyVersion: "ci-v1", WriterID: p.EndpointID, Generation: 1, Scope: functionsql.Scope{FunctionID: function, ProjectID: p.ProjectID, BranchID: p.BranchID, Database: p.Name, Schema: "app", ProvisioningRole: "control_probe"}}
	exec(`INSERT INTO function_sql_credentials(function_id,project_id,branch_id,credential_generation,writer_id,database_name,sql_schema,provisioning_role,namespace,secret_name,secret_uid,content_mac,mac_key_version,created_operation_id) VALUES($1,$2,$3,1,$4,$5,'app','control_probe','neon-ci',$6,$7,$8,'ci-v1',$9)`, function, p.ProjectID, p.BranchID, p.EndpointID, p.Name, old.Name, old.UID, old.MAC, operation)
	for table, hasSecret := range map[string]bool{"managed_auth_instances": true, "data_api_instances": true, "object_storage_instances": false} {
		q := `INSERT INTO ` + table + `(branch_id,project_id,endpoint_id,generation,state,spec`
		values := ` VALUES($1,$2,$3,4,'disabled',jsonb_build_object('database',$4::text,'preserved_setting',true)`
		if hasSecret {
			q += `,secret_ref`
			values += `,'original-private-connection'`
		}
		exec(q+`)`+values+`)`, p.BranchID, p.ProjectID, p.EndpointID, p.Name)
	}
	plan, err := readCatalogBackendRenamePlan(ctx, s.db, p)
	if err != nil || len(plan.Items) != 4 {
		t.Fatal("original dependency inventory", err)
	}
	p.Backend = plan
	for i := range plan.Items {
		if plan.Items[i].Kind == "auth" || plan.Items[i].Kind == "data_api" {
			plan.Items[i].SecretUID = old.UID
		}
	}
	raw, _ := json.Marshal(p)
	exec(`UPDATE operations SET payload=$2 WHERE id=$1`, operation, raw)
	if _, err = s.db.Exec(ctx, `UPDATE function_definitions SET generation=generation+1 WHERE id=$1`, function); err == nil {
		t.Fatal("competing Function mutation ignored active original name migration")
	}
	partial, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = partial.Exec(ctx, `SELECT set_config('neon_control.catalog_backend_rename_operation',$1,true)`, operation); err != nil {
		t.Fatal(err)
	}
	if _, err = partial.Exec(ctx, `UPDATE function_definitions SET database_name=$2,sql_credential_generation=2,version=version+1 WHERE id=$1`, function, p.NewName); err != nil {
		t.Fatal(err)
	}
	if err = partial.Commit(ctx); err == nil {
		t.Fatal("new Function database scope committed without its immutable credential")
	}
	next := old
	next.Generation = 2
	next.Name = "fn-0123456789abcdef-sql-2"
	next.Scope.Database = p.NewName
	next.UID = "22222222-2222-4222-8222-222222222222"
	next.MAC = strings.Repeat("b", 64)
	commit := func(value catalogBackendRenameBackings, finish bool) error {
		tx, e := s.db.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		e = commitCatalogBackendRenameTx(ctx, tx, operation, p, value)
		if e != nil {
			return e
		}
		if !finish {
			return nil
		}
		return tx.Commit(ctx)
	}
	wrong := next
	wrong.Scope.Database = "another_database"
	if err = commit(catalogBackendRenameBackings{function: wrong}, true); err == nil {
		t.Fatal("different signed destination adopted")
	}
	if err = commit(catalogBackendRenameBackings{function: next}, false); err != nil {
		t.Fatal(err)
	}
	var database string
	var generation int64
	if err = s.db.QueryRow(ctx, `SELECT database_name,sql_credential_generation FROM function_definitions WHERE id=$1`, function).Scan(&database, &generation); err != nil || database != p.Name || generation != 1 {
		t.Fatal("rollback lost original scope", err)
	}
	if _, err = s.db.Exec(ctx, `UPDATE function_definitions SET database_name=$2,sql_credential_generation=2,version=version+1 WHERE id=$1`, function, p.NewName); err == nil {
		t.Fatal("unapproved direct scope rewrite accepted")
	}
	exec(`UPDATE managed_auth_instances SET state='active' WHERE branch_id=$1`, p.BranchID)
	if err = checkCatalogBackendRenamePlan(ctx, s.db, p); err == nil {
		t.Fatal("active consumer migrated")
	}
	exec(`UPDATE managed_auth_instances SET state='disabled' WHERE branch_id=$1`, p.BranchID)
	if err = commit(catalogBackendRenameBackings{function: next}, true); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(ctx, `SELECT database_name,sql_credential_generation FROM function_definitions WHERE id=$1`, function).Scan(&database, &generation); err != nil || database != p.NewName || generation != 2 {
		t.Fatal("new signed scope not current", err)
	}
	var history int
	if err = s.db.QueryRow(ctx, `SELECT count(*) FROM function_sql_credentials WHERE function_id=$1 AND ((credential_generation=1 AND database_name=$2 AND secret_uid=$3) OR (credential_generation=2 AND database_name=$4 AND secret_uid=$5))`, function, p.Name, old.UID, p.NewName, next.UID).Scan(&history); err != nil || history != 2 {
		t.Fatal("original credential history lost", err)
	}
	for table := range map[string]bool{"managed_auth_instances": true, "data_api_instances": true, "object_storage_instances": false} {
		var unchanged bool
		if err = s.db.QueryRow(ctx, `SELECT generation=4 AND spec->>'database'=$2 AND spec->>'preserved_setting'='true' AND state='disabled' FROM `+table+` WHERE branch_id=$1`, p.BranchID, p.NewName).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("service data generation or unrelated config changed", table, err)
		}
	}
	if _, err = s.db.Exec(ctx, `UPDATE function_sql_credentials SET database_name=$2 WHERE function_id=$1 AND credential_generation=1`, function, p.NewName); err == nil {
		t.Fatal("historical signed scope overwritten")
	}
}

func TestCatalogBackendRenamePostgresDependencySnapshotIsExact(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	p := catalogRenamePayload{catalogPayload: bindingPayload("original_database", 1), Kind: "database", NewName: "renamed_database", ObjectOID: 1234}
	exec(`INSERT INTO object_storage_instances(branch_id,project_id,endpoint_id,generation,state,spec) VALUES($1,$2,$3,1,'disabled',jsonb_build_object('database',$4::text))`, p.BranchID, p.ProjectID, p.EndpointID, p.Name)
	var err error
	p.Backend, err = readCatalogBackendRenamePlan(ctx, s.db, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = checkCatalogBackendRenamePlan(ctx, s.db, p); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE object_storage_instances SET spec=spec||'{"changed":true}' WHERE branch_id=$1`, p.BranchID)
	if err = checkCatalogBackendRenamePlan(ctx, s.db, p); err == nil {
		t.Fatal("same-generation spec drift ignored")
	}
	exec(`UPDATE object_storage_instances SET spec=spec-'changed' WHERE branch_id=$1`, p.BranchID)
	if err = checkCatalogBackendRenamePlan(ctx, s.db, p); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE object_storage_instances SET state='provisioning' WHERE branch_id=$1`, p.BranchID)
	if _, err = readCatalogBackendRenamePlan(ctx, s.db, p); err == nil {
		t.Fatal("unfinished service accepted")
	}
}
