package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func restoreFixture() (catalogStoredBaseline, catalogNativeSnapshot, catalogIdentityRestorePayload) {
	e, snapshot := inventoryFixture()
	snapshot.Roles[0].Name = "outside_customer"
	b := catalogStoredBaseline{ID: "cbi_111111111111111111111111", Intent: catalogBaselineIntent{Scope: e.Scope, Version: 2, State: "ready", Reference: "original-managed-credential"}, Expected: e, CredentialUID: "11111111-1111-4111-8111-111111111111"}
	p := catalogIdentityRestorePayload{BindingID: b.ID, Scope: e.Scope, ResetCredential: true, catalogRenamePayload: catalogRenamePayload{catalogPayload: catalogPayload{ProjectID: "prj_b0123456789abcde", BranchID: "br_b0123456789abcde", EndpointID: "ep_b0123456789abcde", Name: "outside_customer", CredentialRef: b.Intent.Reference, ExpectedVersion: 2}, Kind: "role", NewName: e.Name, ObjectOID: e.OID, CredentialUID: b.CredentialUID, RoutesUID: "22222222-2222-4222-8222-222222222222", ConfigUIDs: map[string]string{"ep_b0123456789abcde": "33333333-3333-4333-8333-333333333333"}}}
	return b, snapshot, p
}
func TestCatalogRestoreAcknowledgementAndConflictRejection(t *testing.T) {
	b, snapshot, p := restoreFixture()
	r, err := prepareCatalogIdentityRestore(b, snapshot, catalogIdentityRestoreInput{Acknowledge: true, ResetCredential: true})
	if err != nil || r.ObjectOID != 41 || r.NewName != "customer" {
		t.Fatal("same OID rename rejected", err)
	}
	if !catalogRestoreMatchesBaseline(p, b) {
		t.Fatal("original payload rejected")
	}
	for _, input := range []catalogIdentityRestoreInput{{}, {Acknowledge: true}, {ResetCredential: true}} {
		if _, err = prepareCatalogIdentityRestore(b, snapshot, input); err == nil {
			t.Fatal("missing acknowledgment accepted")
		}
	}
	for _, edit := range []func(*catalogNativeSnapshot){
		func(s *catalogNativeSnapshot) { s.Roles[0].OID = 43; s.Databases[0].OwnerOID = 43 },
		func(s *catalogNativeSnapshot) { s.Roles[0].Flags.Superuser = true },
		func(s *catalogNativeSnapshot) { s.Roles[0].Flags.Login = false },
		func(s *catalogNativeSnapshot) { s.Roles[0].Marker = "outside" },
		func(s *catalogNativeSnapshot) {
			s.Roles = append(s.Roles, catalogNativeRole{OID: 43, Name: "customer"})
		},
		func(s *catalogNativeSnapshot) { s.Scope.TimelineID = strings.Repeat("3", 32) },
	} {
		s := snapshot
		s.Roles = append([]catalogNativeRole{}, snapshot.Roles...)
		s.Databases = append([]catalogNativeDatabase{}, snapshot.Databases...)
		edit(&s)
		if _, err = prepareCatalogIdentityRestore(b, s, catalogIdentityRestoreInput{Acknowledge: true, ResetCredential: true}); err == nil {
			t.Fatal("native conflict accepted for restore")
		}
	}
	altered := p
	altered.CredentialUID = "44444444-4444-4444-8444-444444444444"
	if catalogRestoreMatchesBaseline(altered, b) {
		t.Fatal("replacement Secret adopted")
	}
	altered = p
	altered.Scope.TimelineID = strings.Repeat("4", 32)
	if catalogRestoreMatchesBaseline(altered, b) {
		t.Fatal("replacement timeline adopted")
	}
}

// Actual ALTER/rollback and data/ACL/OID preservation in disposable PostgreSQL.
// Explicit custom GUCs exercise scope validation; live Neon is separately tested.
func TestCatalogRestoreDisposableNativeIdentityAndData(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := conn.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	role := "restore_" + randomToken(5)
	outside := role + "_outside"
	database := role + "_db"
	outsideDB := database + "_outside"
	exec("CREATE ROLE " + pgQuote(role) + " LOGIN INHERIT")
	defer conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgQuote(role)+", "+pgQuote(outside))
	exec("CREATE DATABASE " + pgQuote(database) + " OWNER " + pgQuote(role))
	defer conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgQuote(database))
	defer conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgQuote(outsideDB))
	dbCfg := cfg.Copy()
	dbCfg.Database = database
	data, err := pgx.ConnectConfig(ctx, dbCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = data.Exec(ctx, `CREATE TABLE identity_restore_probe(id integer PRIMARY KEY,marker text); INSERT INTO identity_restore_probe VALUES(1,'retained-native-restore')`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	_ = data.Close(ctx)
	b, _, p := restoreFixture()
	p.Name = outside
	p.NewName = role
	p.BranchID = "br_b0123456789abcde"
	marker := "neon-control/branch/" + p.BranchID
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS '" + marker + "'")
	exec("COMMENT ON DATABASE " + pgQuote(database) + " IS '" + marker + "'")
	exec(`SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, p.Scope.TenantID, p.Scope.TimelineID)
	snap, err := readCatalogNativeSnapshot(ctx, conn, p.Scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range snap.Roles {
		if v.Name == role {
			b.Expected = catalogIdentityExpectation{Scope: p.Scope, Kind: "role", Name: role, OID: v.OID, RoleFlags: v.Flags, MarkerSHA256: catalogMarkerDigest(v.Marker)}
		}
	}
	p.ObjectOID = b.Expected.OID
	verifier, err := scramVerifier("only-owned-restore-credential-12345")
	if err != nil {
		t.Fatal(err)
	}
	exec("ALTER ROLE " + pgQuote(role) + " RENAME TO " + pgQuote(outside))
	wrong := p
	wrong.Scope.TimelineID = strings.Repeat("c", 32)
	if err = applyCatalogIdentityRestoreSQL(ctx, conn, wrong, b.Expected, verifier); err == nil {
		t.Fatal("cross-timeline restore accepted")
	}
	if err = applyCatalogIdentityRestoreSQL(ctx, conn, p, b.Expected, verifier); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err = conn.QueryRow(ctx, `SELECT oid=$2 AND rolpassword=$3 FROM pg_authid WHERE rolname=$1`, role, p.ObjectOID, verifier).Scan(&correct); err != nil || !correct {
		t.Fatal("original OID/stored credential not restored", err)
	}
	// Retrying an unknown native success keeps the same OID and desired name.
	if err = applyCatalogIdentityRestoreSQL(ctx, conn, p, b.Expected, verifier); err != nil {
		t.Fatal(err)
	}
	exec("ALTER ROLE " + pgQuote(role) + " RENAME TO " + pgQuote(outside))
	exec("COMMENT ON ROLE " + pgQuote(outside) + " IS 'external'")
	if err = applyCatalogIdentityRestoreSQL(ctx, conn, p, b.Expected, verifier); err == nil {
		t.Fatal("changed provenance restored")
	}
	exec("COMMENT ON ROLE " + pgQuote(outside) + " IS '" + marker + "'")
	if err = applyCatalogIdentityRestoreSQL(ctx, conn, p, b.Expected, verifier); err != nil {
		t.Fatal(err)
	}
	var dbExpected catalogIdentityExpectation
	for _, v := range snap.Databases {
		if v.Name == database {
			dbExpected = catalogIdentityExpectation{Scope: p.Scope, Kind: "database", Name: database, OID: v.OID, OwnerOID: v.OwnerOID, OwnerName: role, DatabaseFlags: v.Flags, MarkerSHA256: catalogMarkerDigest(v.Marker)}
		}
	}
	dbp := p
	dbp.Kind = "database"
	dbp.Name = outsideDB
	dbp.NewName = database
	dbp.ObjectOID = dbExpected.OID
	dbp.Owner = role
	dbp.CredentialRef = ""
	dbp.CredentialUID = ""
	dbp.ResetCredential = false
	exec("ALTER DATABASE " + pgQuote(database) + " RENAME TO " + pgQuote(outsideDB))
	if err = applyCatalogIdentityRestoreSQL(ctx, conn, dbp, dbExpected, ""); err != nil {
		t.Fatal(err)
	}
	data, err = pgx.ConnectConfig(ctx, dbCfg)
	if err != nil {
		t.Fatal(err)
	}
	var markerData string
	err = data.QueryRow(ctx, `SELECT marker FROM identity_restore_probe WHERE id=1`).Scan(&markerData)
	_ = data.Close(ctx)
	if err != nil || markerData != "retained-native-restore" {
		t.Fatal("native data changed", err)
	}
	// Deleting/recreating a name, even copying its comment, must never repair.
	exec("ALTER DATABASE " + pgQuote(database) + " OWNER TO " + pgQuote(cfg.User))
	exec("DROP ROLE " + pgQuote(role))
	exec("CREATE ROLE " + pgQuote(role) + " LOGIN INHERIT")
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS '" + marker + "'")
	if err = applyCatalogIdentityRestoreSQL(ctx, conn, p, b.Expected, verifier); err == nil {
		t.Fatal("replacement native OID adopted")
	}
}

func TestCatalogRestoreAtomicAdmissionResultAndOriginalRecovery(t *testing.T) {
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
	schema := "v2_restore_" + randomToken(5)
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
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	b, _, p := restoreFixture()
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('restore_admin','restore_admin','unused','owner'),('restore_editor','restore_editor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('restore_org','restore-org','Restore')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('restore_org','restore_admin','admin'),('restore_org','restore_editor','editor')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES($1,'restore_org','Restore','lab',16,'ready','managed',$2,now(),now())`, p.ProjectID, p.Scope.TenantID)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'main',$3,'ready',now())`, p.BranchID, p.ProjectID, p.Scope.TimelineID)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','postgres','read_write',now(),now())`, p.EndpointID, p.ProjectID, p.BranchID)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,$2,'ready',$3,2)`, p.BranchID, p.NewName, p.CredentialRef)
	raw, _ := json.Marshal(b.Expected)
	exec(`INSERT INTO catalog_identity_baselines(id,project_id,branch_id,kind,name,object_oid,intent_version,tenant_id,timeline_id,expectation,marker_sha256,credential_ref,credential_uid,captured_by) VALUES($1,$2,$3,'role',$4,$5,2,$6,$7,$8,$9,$10,$11,'restore_admin')`, b.ID, p.ProjectID, p.BranchID, p.NewName, int64(p.ObjectOID), p.Scope.TenantID, p.Scope.TimelineID, raw, b.Expected.MarkerSHA256, p.CredentialRef, p.CredentialUID)
	fault := func(e error, status int) {
		t.Helper()
		var f *catalogRenameFault
		if !errors.As(e, &f) || f.Status != status {
			t.Fatalf("restore admission rejection: %v", e)
		}
	}
	s := &server{db: db}
	url := "/api/v1/projects/" + p.ProjectID + "/branches/" + p.BranchID + "/catalog-identities/" + p.BindingID + "/restore"
	input := catalogIdentityRestoreInput{Acknowledge: true, ResetCredential: true}
	hash := s.createRequestHash("POST:"+url, record{"input": input, "version": int64(2)})
	admit := func(actor, key string, value catalogIdentityRestorePayload, want int) string {
		t.Helper()
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		op, e := admitCatalogIdentityRestoreTx(ctx, tx, value, actor, "restore-request", keyHash(key), hash)
		if want != 0 {
			fault(e, want)
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
	admit("restore_editor", "editor", p, 403)
	wrong := p
	wrong.CredentialUID = "44444444-4444-4444-8444-444444444444"
	admit("restore_admin", "wrong-secret", wrong, 409)
	exec(`UPDATE branch_roles SET version=3 WHERE branch_id=$1`, p.BranchID)
	admit("restore_admin", "stale", p, 412)
	exec(`UPDATE branch_roles SET version=2 WHERE branch_id=$1`, p.BranchID)
	op := admit("restore_admin", "original", p, 0)
	admit("restore_admin", "competing", p, 409)
	exec(`UPDATE operations SET state='running',lease_owner='restore-worker',lease_expires_at=now()+interval '5 minutes' WHERE id=$1`, op)
	commit := func(worker string, wantError bool) {
		t.Helper()
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		e = commitCatalogIdentityRestoreResultTx(ctx, tx, op, worker, p)
		if wantError {
			if e == nil {
				t.Fatal("invalid result committed")
			}
			return
		}
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	commit("old-worker", true)
	exec(`UPDATE branches SET state='deleting' WHERE id=$1`, p.BranchID)
	commit("restore-worker", true)
	exec(`UPDATE branches SET state='ready' WHERE id=$1`, p.BranchID)
	exec(`CREATE FUNCTION reject_restore_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_restore_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_restore_audit()`)
	commit("restore-worker", true)
	var n int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM catalog_identity_restore_results`).Scan(&n); err != nil || n != 0 {
		t.Fatal("result survived failed audit", err)
	}
	exec(`DROP TRIGGER reject_restore_audit ON audit_events`)
	commit("restore-worker", false)
	commit("restore-worker", false)
	exec(`UPDATE operation_steps SET state='failed' WHERE operation_id=$1 AND ordinal=3`, op)
	// Native/Kubernetes clients are deliberately absent. A committed result must
	// recover only its original metadata steps, even after later intent changes.
	exec(`UPDATE branch_roles SET version=3 WHERE branch_id=$1`, p.BranchID)
	if err = s.reconcileCatalogIdentityRestore(ctx, op, "restore-worker", p); err != nil {
		t.Fatal("original result contacted native stores", err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM operation_steps WHERE operation_id=$1 AND state='succeeded'`, op).Scan(&n); err != nil || n != 4 {
		t.Fatal("original step outcome not recovered", err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='restore_catalog_identity' AND outcome='succeeded'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("restore audit duplicated", err)
	}
	for _, actor := range []string{"restore_admin", "restore_editor"} {
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	handler := s.routes()
	body, _ := json.Marshal(input)
	request := func(actor string, csrf bool, want int) {
		t.Helper()
		req := httptest.NewRequest("POST", url, strings.NewReader(string(body)))
		req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		if csrf {
			req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
			req.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		req.Header.Set("If-Match", `"2"`)
		req.Header.Set("Idempotency-Key", "original")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("restore HTTP %d, want %d: %s", response.Code, want, response.Body.String())
		}
		if want == 202 {
			var v struct {
				Operation struct {
					ID string `json:"id"`
				} `json:"operation"`
			}
			if e := json.Unmarshal(response.Body.Bytes(), &v); e != nil || v.Operation.ID != op {
				t.Fatal("original restore Operation was not replayed", e)
			}
		}
	}
	request("restore_editor", true, 403)
	request("restore_admin", false, 403)
	exec(`UPDATE endpoints SET state='deleted',deleted_at=now() WHERE id=$1`, p.EndpointID)
	request("restore_admin", true, 202)
}
