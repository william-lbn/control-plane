package control

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func catalogBindingFixture(t *testing.T) (context.Context, *server, func(string, ...any)) {
	t.Helper()
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "v2_catalog_binding_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgQuote(schema)); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.Exec(context.Background(), "DROP SCHEMA "+pgQuote(schema)+" CASCADE")
		admin.Close()
	})
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('binding_admin','binding_admin','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('binding_org','binding-org','Catalog binding')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('binding_org')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('binding_org','binding_admin','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES('prj_0123456789abcdef','binding_org','Catalog','lab',16,'ready','managed',$1,now(),now())`, strings.Repeat("a", 32))
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES('br_0123456789abcdef','prj_0123456789abcdef','main',$1,'ready',now())`, strings.Repeat("b", 32))
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES('ep_0123456789abcdef','prj_0123456789abcdef','br_0123456789abcdef','binding','neonvm','binding','binding','active','cloud_admin','postgres','read_write',now(),now())`)
	return ctx, &server{db: db}, exec
}
func bindingPayload(name string, version int64) catalogPayload {
	return catalogPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", Name: name, ExpectedVersion: version, NativeIdentity: &catalogMutationIdentity{Scope: catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("b", 32)}}}
}
func bindingOperation(t *testing.T, exec func(string, ...any), action string, p catalogPayload) catalogPayload {
	t.Helper()
	p.operationID = newID("op_")
	p.workerID = "binding_worker"
	raw, _ := json.Marshal(p)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'branch_catalog',$3,$4,'running','binding_admin',$1,$5,$6,now()+interval '5 minutes')`, p.operationID, p.ProjectID, p.BranchID, action, raw, p.workerID)
	for i := 0; i < 4; i++ {
		exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, p.operationID, i, "binding_step")
	}
	return p
}

func TestCatalogObjectBindingCreateRotateDeleteReuseAndOriginalRecovery(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	p := bindingPayload("bound_role", 1)
	p.CredentialRef = "original-credential"
	p.MutationCredentialUID = "11111111-1111-4111-8111-111111111111"
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,$2,'creating',$3,1)`, p.BranchID, p.Name, p.CredentialRef)
	p = bindingOperation(t, exec, "create_role", p)
	commit := func(action string, value catalogPayload) error {
		tx, e := s.db.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if e = commitCatalogMutationResultTx(ctx, tx, value.operationID, value.workerID, action, value); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if err := commit("create_role", p); err == nil {
		t.Fatal("created identity inferred without original OID acknowledgment")
	}
	if err := s.recordCatalogCreateObservation(ctx, p, "create_role", 12345); err != nil {
		t.Fatal(err)
	}
	if err := s.recordCatalogCreateObservation(ctx, p, "create_role", 12346); err == nil {
		t.Fatal("original OID overwritten")
	}
	exec(`CREATE FUNCTION reject_binding_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_binding_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_binding_audit()`)
	if err := commit("create_role", p); err == nil {
		t.Fatal("audit failure left committed identity")
	}
	var count int
	s.db.QueryRow(ctx, `SELECT count(*) FROM catalog_object_bindings`).Scan(&count)
	if count != 0 {
		t.Fatal("binding survived rollback")
	}
	exec(`DROP TRIGGER reject_binding_audit ON audit_events; DROP FUNCTION reject_binding_audit()`)
	if err := commit("create_role", p); err != nil {
		t.Fatal(err)
	}
	load := func(name string) catalogObjectRef {
		t.Helper()
		tx, e := s.db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		v, e := loadCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "role", name, p.NativeIdentity.Scope)
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		return v
	}
	original := load(p.Name)
	if original.OID != 12345 || original.Version != 2 || original.CredentialUID != p.MutationCredentialUID {
		t.Fatal("wrong create generation")
	}
	rotate := bindingPayload(p.Name, 2)
	rotate.PreviousCredentialRef = p.CredentialRef
	rotate.CredentialRef = "rotated-credential"
	rotate.MutationCredentialUID = "22222222-2222-4222-8222-222222222222"
	rotate.NativeIdentity.Object = original
	exec(`UPDATE branch_roles SET state='updating' WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name)
	exec(`UPDATE operations SET state='succeeded',lease_owner=NULL WHERE id=$1`, p.operationID)
	rotate = bindingOperation(t, exec, "rotate_role_password", rotate)
	wrong := rotate
	proof := *rotate.NativeIdentity
	proof.Object.CredentialUID = "33333333-3333-4333-8333-333333333333"
	wrong.NativeIdentity = &proof
	if err := commit("rotate_role_password", wrong); err == nil {
		t.Fatal("forged previous credential generation accepted")
	}
	if err := commit("rotate_role_password", rotate); err != nil {
		t.Fatal(err)
	}
	current := load(p.Name)
	if current.IdentityID != original.IdentityID || current.OID != original.OID || current.Version != 3 || current.CredentialUID != rotate.MutationCredentialUID {
		t.Fatal("rotation broke native lineage")
	}
	exec(`UPDATE operations SET state='succeeded',lease_owner=NULL WHERE id=$1`, rotate.operationID)
	deletion := bindingPayload(p.Name, 3)
	deletion.PreviousCredentialRef = rotate.CredentialRef
	deletion.NativeIdentity.Object = current
	exec(`UPDATE branch_roles SET state='deleting' WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name)
	deletion = bindingOperation(t, exec, "delete_role", deletion)
	if err := commit("delete_role", deletion); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE operations SET state='succeeded',lease_owner=NULL WHERE id=$1`, deletion.operationID)
	replacement := bindingPayload(p.Name, 5)
	replacement.CredentialRef = "replacement-credential"
	replacement.MutationCredentialUID = "44444444-4444-4444-8444-444444444444"
	exec(`UPDATE branch_roles SET state='creating',version=5,credential_ref=$3 WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name, replacement.CredentialRef)
	replacement = bindingOperation(t, exec, "create_role", replacement)
	if err := s.recordCatalogCreateObservation(ctx, replacement, "create_role", 23456); err != nil {
		t.Fatal(err)
	}
	if err := commit("create_role", replacement); err != nil {
		t.Fatal(err)
	}
	latest := load(p.Name)
	if latest.IdentityID == original.IdentityID || latest.OID != 23456 || latest.Version != 6 {
		t.Fatal("name reuse adopted retired identity")
	}
	// Nil Kube/Proxy proves old committed recovery does not read new credentials.
	exec(`UPDATE operations SET state='succeeded',lease_owner=NULL WHERE id=$1`, replacement.operationID)
	exec(`UPDATE operations SET state='running',lease_owner=$2,lease_expires_at=now()+interval '5 minutes' WHERE id=$1`, p.operationID, p.workerID)
	if err := s.reconcileCatalog(ctx, p.operationID, p.workerID, "create_role", p); err != nil {
		t.Fatal("old result touched later generation", err)
	}
	if got := load(p.Name); got != latest {
		t.Fatal("old result rewrote current native generation")
	}
	for _, q := range []string{`UPDATE catalog_object_bindings SET object_oid=99`, `DELETE FROM catalog_object_bindings`, `UPDATE catalog_create_observations SET object_oid=99`, `DELETE FROM catalog_create_observations`} {
		if _, err := s.db.Exec(ctx, q); err == nil {
			t.Fatal("immutable native evidence changed", q)
		}
	}
}

func TestCatalogObjectBindingInitialObservationAndOwnerRenameFollower(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	p := bindingPayload("original_owner", 2)
	const uid = "11111111-1111-4111-8111-111111111111"
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,$2,'ready','baseline-credential',2)`, p.BranchID, p.Name)
	expectation := catalogIdentityExpectation{Scope: p.NativeIdentity.Scope, Kind: "role", Name: p.Name, OID: 33333}
	raw, _ := json.Marshal(expectation)
	exec(`INSERT INTO catalog_identity_baselines(id,project_id,branch_id,kind,name,object_oid,intent_version,tenant_id,timeline_id,expectation,marker_sha256,credential_ref,credential_uid,captured_by) VALUES('baseline-original',$1,$2,'role',$3,33333,2,$4,$5,$6,$7,'baseline-credential',$8,'binding_admin')`, p.ProjectID, p.BranchID, p.Name, p.NativeIdentity.Scope.TenantID, p.NativeIdentity.Scope.TimelineID, raw, strings.Repeat("c", 64), uid)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ref, err := loadCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "role", p.Name, p.NativeIdentity.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if ref.OID != 33333 || ref.CredentialUID != uid {
		t.Fatal("baseline generation differs")
	}
	wrongScope := p.NativeIdentity.Scope
	wrongScope.TimelineID = strings.Repeat("c", 32)
	if _, err = loadCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "role", p.Name, wrongScope); err == nil {
		t.Fatal("foreign timeline used current binding")
	}
	dbref := catalogObjectRef{ID: "db_binding", IdentityID: "db_identity", OID: 44444, Version: 4, OwnerOID: ref.OID}
	if err = insertCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "database", "owned_database", "ready", "initial_observation", "owned-db-baseline", p.NativeIdentity.Scope, dbref); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO branch_databases(branch_id,name,owner_name,state,version) VALUES($1,'owned_database',$2,'ready',4)`, p.BranchID, p.Name); err != nil {
		t.Fatal(err)
	}
	p.NativeIdentity.Object = ref
	p.CredentialRef = ref.CredentialRef
	rename := catalogRenamePayload{catalogPayload: p, Kind: "role", NewName: "renamed_owner", ObjectOID: ref.OID, CredentialUID: uid}
	if err = commitCatalogRenameBindingTx(ctx, tx, "original-rename", rename); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE branch_roles SET state='updating' WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name); err != nil {
		t.Fatal(err)
	}
	if err = commitCatalogRenameTx(ctx, tx, rename); err != nil {
		t.Fatal(err)
	}
	current, err := loadCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "role", rename.NewName, p.NativeIdentity.Scope)
	if err != nil || current.IdentityID != ref.IdentityID || current.Version != 3 || current.CredentialUID != uid {
		t.Fatal("rename lost original native identity", err)
	}
	follower, err := loadCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "database", "owned_database", p.NativeIdentity.Scope)
	if err != nil || follower.IdentityID != dbref.IdentityID || follower.Version != 5 || follower.OwnerOID != ref.OID {
		t.Fatal("database follower identity was not carried atomically", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,'unobserved','ready','unobserved-credential',2)`, p.BranchID)
	tx, err = s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = loadCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "role", "unobserved", p.NativeIdentity.Scope); err == nil {
		t.Fatal("name-only legacy identity silently adopted")
	}
}

func TestCatalogObjectBindingNativeSameNameReplacementAndSCRAMRename(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	p := bindingPayload("bound_native_"+randomToken(6), 2)
	old := p.Name + "_retained"
	renamed := p.Name + "_renamed"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := conn.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, p.NativeIdentity.Scope.TenantID, p.NativeIdentity.Scope.TimelineID)
	verifier, err := scramVerifier("ci-original-bound-native-password")
	if err != nil {
		t.Fatal(err)
	}
	other, err := scramVerifier("ci-external-current-native-password")
	if err != nil {
		t.Fatal(err)
	}
	exec("CREATE ROLE " + pgQuote(p.Name) + " NOLOGIN CONNECTION LIMIT 7 PASSWORD " + pgLiteral(other))
	exec("COMMENT ON ROLE " + pgQuote(p.Name) + " IS " + pgLiteral("neon-control/branch/"+p.BranchID))
	defer func() {
		for _, name := range []string{p.Name, old, renamed} {
			conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgQuote(name))
		}
	}()
	if err = conn.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname=$1`, p.Name).Scan(&p.NativeIdentity.Object.OID); err != nil {
		t.Fatal(err)
	}
	if err = verifyCatalogNativeBinding(ctx, conn, p, "rotate_role_password", 0, false); err != nil {
		t.Fatal(err)
	}
	if err = applyBoundRolePasswordSQL(ctx, conn, p, "rotate_role_password", verifier, 0); err != nil {
		t.Fatal(err)
	}
	var rotated string
	var canLogin bool
	var connectionLimit int
	if err = conn.QueryRow(ctx, `SELECT rolpassword,rolcanlogin,rolconnlimit FROM pg_authid WHERE oid=$1`, p.NativeIdentity.Object.OID).Scan(&rotated, &canLogin, &connectionLimit); err != nil || rotated != verifier || canLogin || connectionLimit != 7 {
		t.Fatal("password-only rotation rewrote unrelated flags", err)
	}
	exec("ALTER ROLE " + pgQuote(p.Name) + " PASSWORD " + pgLiteral(other))
	rename := catalogRenamePayload{catalogPayload: p, Kind: "role", NewName: renamed, ObjectOID: p.NativeIdentity.Object.OID}
	rename.CredentialRef = "original-credential"
	if err = applyCatalogRenameSQL(ctx, conn, rename, verifier); err != nil {
		t.Fatal(err)
	}
	var actual string
	var login bool
	var limit int
	if err = conn.QueryRow(ctx, `SELECT rolpassword,rolcanlogin,rolconnlimit FROM pg_authid WHERE oid=$1`, p.NativeIdentity.Object.OID).Scan(&actual, &login, &limit); err != nil || actual != other || login || limit != 7 {
		t.Fatal("rename rewrote unrelated current credential or options", err)
	}
	exec("ALTER ROLE " + pgQuote(renamed) + " RENAME TO " + pgQuote(old))
	exec("CREATE ROLE " + pgQuote(p.Name) + " NOLOGIN CONNECTION LIMIT 9 PASSWORD " + pgLiteral(other))
	exec("COMMENT ON ROLE " + pgQuote(p.Name) + " IS " + pgLiteral("neon-control/branch/"+p.BranchID))
	for _, action := range []string{"rotate_role_password", "delete_role"} {
		if err = verifyCatalogNativeBinding(ctx, conn, p, action, 0, false); err == nil {
			t.Fatal("same-name copied-marker replacement accepted", action)
		}
	}
	if err = verifyCatalogNativeBinding(ctx, conn, p, "create_role", 0, false); err == nil {
		t.Fatal("unknown original CREATE outcome adopted")
	}
	if err = applyCatalogRenameSQL(ctx, conn, rename, verifier); err == nil {
		t.Fatal("rename adopted replacement")
	}
	if err = applyBoundRolePasswordSQL(ctx, conn, p, "rotate_role_password", verifier, 0); err == nil {
		t.Fatal("rotation reset replacement password")
	}
	if err = applyBoundRoleDeletionSQL(ctx, conn, p); err == nil {
		t.Fatal("deletion removed replacement")
	}
	exec("DROP ROLE " + pgQuote(p.Name))
	if err = verifyCatalogNativeBinding(ctx, conn, p, "delete_role", 0, true); err == nil {
		t.Fatal("renamed original object treated as deleted")
	}
}

func TestCatalogObjectBindingNativeDatabaseAndOwnerReplacement(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	suffix := randomToken(6)
	p := bindingPayload("bound_database_"+suffix, 2)
	p.PreviousOwner = "bound_owner_" + suffix
	p.Owner = "bound_target_" + suffix
	oldDB, oldOwner := p.Name+"_retained", p.Owner+"_retained"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := conn.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	marker := "neon-control/branch/" + p.BranchID
	exec(`SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, p.NativeIdentity.Scope.TenantID, p.NativeIdentity.Scope.TimelineID)
	for _, name := range []string{p.PreviousOwner, p.Owner} {
		exec("CREATE ROLE " + pgQuote(name) + " NOLOGIN")
		exec("COMMENT ON ROLE " + pgQuote(name) + " IS " + pgLiteral(marker))
	}
	defer func() {
		for _, name := range []string{p.Name, oldDB} {
			conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgQuote(name))
		}
		for _, name := range []string{p.PreviousOwner, p.Owner, oldOwner} {
			conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgQuote(name))
		}
	}()
	previous, target := catalogObjectRef{}, catalogObjectRef{}
	if err = conn.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname=$1`, p.PreviousOwner).Scan(&previous.OID); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname=$1`, p.Owner).Scan(&target.OID); err != nil {
		t.Fatal(err)
	}
	p.NativeIdentity.PreviousOwner, p.NativeIdentity.Owner = &previous, &target
	exec("CREATE DATABASE " + pgQuote(p.Name) + " OWNER " + pgQuote(p.PreviousOwner) + " CONNECTION LIMIT 17")
	exec("COMMENT ON DATABASE " + pgQuote(p.Name) + " IS " + pgLiteral(marker))
	if err = conn.QueryRow(ctx, `SELECT oid FROM pg_database WHERE datname=$1`, p.Name).Scan(&p.DatabaseOID); err != nil {
		t.Fatal(err)
	}
	p.NativeIdentity.Object.OID = p.DatabaseOID
	p.NativeIdentity.Object.OwnerOID = previous.OID
	dataCfg := cfg.Copy()
	dataCfg.Database = p.Name
	data, err := pgx.ConnectConfig(ctx, dataCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = data.Exec(ctx, `CREATE TABLE retained_binding_data(value text);INSERT INTO retained_binding_data VALUES('retained-native-binding-data')`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	data.Close(ctx)
	if err = applyDatabaseOwnerSQL(ctx, conn, p); err != nil {
		t.Fatal(err)
	}
	if err = applyDatabaseOwnerSQL(ctx, conn, p); err != nil {
		t.Fatal("same original Owner result not repeatable", err)
	}
	var oid, owner uint32
	var limit int
	if err = conn.QueryRow(ctx, `SELECT oid,datdba,datconnlimit FROM pg_database WHERE datname=$1`, p.Name).Scan(&oid, &owner, &limit); err != nil || oid != p.DatabaseOID || owner != target.OID || limit != 17 {
		t.Fatal("Owner operation changed database identity/options", err)
	}
	exec("ALTER ROLE " + pgQuote(p.Owner) + " RENAME TO " + pgQuote(oldOwner))
	exec("CREATE ROLE " + pgQuote(p.Owner) + " NOLOGIN")
	exec("COMMENT ON ROLE " + pgQuote(p.Owner) + " IS " + pgLiteral(marker))
	if err = applyDatabaseOwnerSQL(ctx, conn, p); err == nil {
		t.Fatal("same-name replacement owner was adopted")
	}
	exec("DROP ROLE " + pgQuote(p.Owner))
	exec("ALTER ROLE " + pgQuote(oldOwner) + " RENAME TO " + pgQuote(p.Owner))
	exec("ALTER DATABASE " + pgQuote(p.Name) + " RENAME TO " + pgQuote(oldDB))
	exec("CREATE DATABASE " + pgQuote(p.Name) + " OWNER " + pgQuote(p.PreviousOwner))
	exec("COMMENT ON DATABASE " + pgQuote(p.Name) + " IS " + pgLiteral(marker))
	if err = applyDatabaseOwnerSQL(ctx, conn, p); err == nil {
		t.Fatal("same-name replacement database was adopted")
	}
	deletion := p
	deletion.PreviousOwner = ""
	deletion.NativeIdentity = &catalogMutationIdentity{Scope: p.NativeIdentity.Scope, Object: catalogObjectRef{OID: p.DatabaseOID, OwnerOID: target.OID}, Owner: &target}
	if err = verifyCatalogNativeBinding(ctx, conn, deletion, "delete_database", 0, false); err == nil {
		t.Fatal("database delete accepted replacement OID")
	}
	dataCfg.Database = oldDB
	data, err = pgx.ConnectConfig(ctx, dataCfg)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	err = data.QueryRow(ctx, `SELECT value FROM retained_binding_data`).Scan(&value)
	data.Close(ctx)
	if err != nil || value != "retained-native-binding-data" {
		t.Fatal("original database data changed", err)
	}
}
