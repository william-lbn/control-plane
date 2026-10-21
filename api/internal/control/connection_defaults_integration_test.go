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

func TestConnectionDefaultsAtomicAdmissionReplayScopeAndDependencies(t *testing.T) {
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
	schema := "v2_defaults_" + randomToken(6)
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
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	const project = "prj_c0123456789abcde"
	const branch = "br_c0123456789abcde"
	const writer = "ep_c0123456789abcde"
	const reader = "ep_c1123456789abcde"
	const tenant = "1123456789abcdef0123456789abcdef"
	const timeline = "2123456789abcdef0123456789abcdef"
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('defaults_admin','defaults_admin','unused','owner'),('defaults_editor','defaults_editor','unused','owner'),('defaults_foreign','defaults_foreign','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('defaults_org','defaults-org','Defaults')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('defaults_org','defaults_admin','admin'),('defaults_org','defaults_editor','editor')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES($1,'defaults_org','Defaults','lab',16,'ready','managed',$2,now(),now())`, project, tenant)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'main',$3,'ready',now())`, branch, project, timeline)
	for _, endpoint := range []string{writer, reader} {
		kind := "read_write"
		if endpoint == reader {
			kind = "read_only"
		}
		exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',$4,now(),now())`, endpoint, project, branch, kind)
	}
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES($1,'customer','ready','original-customer-credential')`, branch)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES($1,'customer_db','customer','ready')`, branch)
	input := connectionDefaultInput{Mode: "managed", Role: "customer", Database: "customer_db"}
	observed := connectionDefaultObservation{RoleOID: 12345, DatabaseOID: 23456, TenantID: tenant, TimelineID: timeline, CredentialRef: "original-customer-credential", RoleVersion: 1, DatabaseVersion: 1, DatabaseOwner: "customer", PlatformDatabase: "neondb"}
	expectFault := func(e error, status int) {
		t.Helper()
		var fault *catalogRenameFault
		if !errors.As(e, &fault) || fault.Status != status {
			t.Fatalf("wrong defaults rejection: %v, want %d", e, status)
		}
	}
	admit := func(actor, key, hash string, version int64, value connectionDefaultInput, proof connectionDefaultObservation, want int) record {
		t.Helper()
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		result, e := commitConnectionDefaultTx(ctx, tx, project, branch, actor, "defaults-request", key, hash, version, value, proof)
		if want != 0 {
			expectFault(e, want)
			return nil
		}
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		return result
	}
	s := &server{db: db}
	public := func(endpoint string) record {
		t.Helper()
		v, e := s.publicConnectionDefaults(ctx, record{"id": endpoint, "project_id": project, "branch_id": branch})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	if v := public(reader); v["role"] != "cloud_admin" || v["database"] != "neondb" || v["version_token"] != "1" {
		t.Fatal("legacy defaults changed")
	}
	admit("defaults_editor", "original", "hash", 1, input, observed, 403)
	admit("defaults_foreign", "original", "hash", 1, input, observed, 403)
	exec(`UPDATE users SET disabled=true WHERE id='defaults_admin'`)
	admit("defaults_admin", "original", "hash", 1, input, observed, 403)
	exec(`UPDATE users SET disabled=false WHERE id='defaults_admin'`)
	admit("defaults_admin", "original", "hash", 2, input, observed, 412)
	bad := observed
	bad.TimelineID = strings.Repeat("a", 32)
	admit("defaults_admin", "original", "hash", 1, input, bad, 409)
	bad = observed
	bad.CredentialRef = "new-credential"
	admit("defaults_admin", "original", "hash", 1, input, bad, 409)
	bad = observed
	bad.DatabaseVersion = 2
	admit("defaults_admin", "original", "hash", 1, input, bad, 409)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('op_defaults_pending',$1,'branch_catalog',$2,'create_role','queued','defaults_admin','req','{}')`, project, branch)
	admit("defaults_admin", "original", "hash", 1, input, observed, 409)
	exec(`UPDATE operations SET state='succeeded' WHERE id='op_defaults_pending'`)
	// Audit failure must roll back the defaults AND replay receipt together.
	exec(`CREATE FUNCTION reject_defaults_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_defaults_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_defaults_audit()`)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = commitConnectionDefaultTx(ctx, tx, project, branch, "defaults_admin", "req", "original", "hash", 1, input, observed)
	if err == nil {
		t.Fatal("audit failure accepted defaults")
	}
	_ = tx.Rollback(ctx)
	if v := public(writer); v["version_token"] != "1" {
		t.Fatal("audit failure left partial defaults")
	}
	if value, e := readConnectionDefaultReceipt(ctx, db, "defaults_admin", "original", "hash"); e != nil || value != nil {
		t.Fatal("audit failure left a receipt", e)
	}
	exec(`DROP TRIGGER reject_defaults_audit ON audit_events`)
	v := admit("defaults_admin", "original", "hash", 1, input, observed, 0)
	if v["version_token"] != "2" {
		t.Fatal("defaults version missing")
	}
	for _, endpoint := range []string{writer, reader} {
		if v := public(endpoint); v["role"] != "customer" || v["database"] != "customer_db" {
			t.Fatal("Writer/Reader defaults disagree")
		}
	}
	// Replaying the lost response precedes version checks and changes no state.
	v = admit("defaults_admin", "original", "hash", 1, input, observed, 0)
	if v["version_token"] != "2" {
		t.Fatal("lost response did not replay original receipt")
	}
	admit("defaults_admin", "original", "other-body", 2, input, observed, 409)
	admit("defaults_admin", "new-key", "new-hash", 1, input, observed, 412)
	for _, kind := range []string{"role", "database"} {
		name := "customer_db"
		if kind == "role" {
			name = "customer"
		}
		expectFault(checkConnectionDefaultDependency(ctx, db, branch, kind, name), 409)
	}
	// Existing rename admission is fenced by the same default dependency.
	expectFault(checkCatalogRenameDependencies(ctx, db, catalogRenamePayload{catalogPayload: catalogPayload{BranchID: branch, ProjectID: project, Name: "customer"}, Kind: "role", NewName: "renamed"}), 409)
	v = admit("defaults_admin", "restore-key", "restore-hash", 2, connectionDefaultInput{Mode: "platform"}, connectionDefaultObservation{}, 0)
	if v["role"] != "cloud_admin" || v["database"] != "neondb" || v["version_token"] != "3" {
		t.Fatal("platform defaults not restored")
	}
	if e := checkConnectionDefaultDependency(ctx, db, branch, "role", "customer"); e != nil {
		t.Fatal("restored platform still holds customer dependency", e)
	}
	v = admit("defaults_admin", "original", "hash", 1, input, observed, 0)
	if v["version_token"] != "2" || public(reader)["version_token"] != "3" {
		t.Fatal("old replay changed current defaults")
	}
	var platformRole, platformDB string
	if err = db.QueryRow(ctx, `SELECT role_name,database_name FROM endpoints WHERE id=$1`, writer).Scan(&platformRole, &platformDB); err != nil || platformRole != "cloud_admin" || platformDB != "neondb" {
		t.Fatal("customer default mutation modified bootstrap identity", err)
	}
	var requests, audits int
	if err = db.QueryRow(ctx, `SELECT (SELECT count(*) FROM connection_default_requests),(SELECT count(*) FROM audit_events WHERE action='update_connection_defaults')`).Scan(&requests, &audits); err != nil || requests != 2 || audits != 2 {
		t.Fatal("receipt/audit duplicated", err)
	}
	for _, actor := range []string{"defaults_admin", "defaults_editor", "defaults_foreign"} {
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	handler := s.routes()
	path := "/api/v1/projects/" + project + "/branches/" + branch + "/connection-defaults"
	request := func(actor, method, url, body, match string, csrf bool, status int) record {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		if csrf {
			req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
			req.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		req.Header.Set("If-Match", match)
		req.Header.Set("Idempotency-Key", "http-original-defaults-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != status {
			t.Fatalf("defaults HTTP %s status %d, expected %d: %s", method, response.Code, status, response.Body.String())
		}
		var value record
		if e := json.Unmarshal(response.Body.Bytes(), &value); e != nil {
			t.Fatal(e)
		}
		return value
	}
	request("defaults_foreign", "GET", path, "", "", true, 404)
	request("defaults_editor", "PATCH", path, `{"mode":"platform"}`, `"3"`, true, 403)
	request("defaults_admin", "PATCH", path, `{"mode":"platform"}`, `"3"`, false, 403)
	request("defaults_admin", "PATCH", path, `{"mode":"platform"}`, "", true, 428)
	request("defaults_admin", "PATCH", path, `{"mode":"platform"}`, "3", true, 422)
	request("defaults_admin", "PATCH", path, `{"mode":"managed","role":"cloud_admin","database":"neondb"}`, `"3"`, true, 422)
	if v := request("defaults_editor", "GET", path, "", "", true, 200); v["version_token"] != "3" {
		t.Fatal("Editor metadata read failed")
	}
	request("defaults_admin", "PATCH", path, `{"mode":"platform"}`, `"3"`, true, 200)
	if v := request("defaults_admin", "PATCH", path, `{"mode":"platform"}`, `"3"`, true, 200); v["version_token"] != "4" {
		t.Fatal("HTTP lost acknowledgement did not replay")
	}
	exec(`UPDATE endpoints SET state='deleted',deleted_at=now() WHERE id=$1`, writer)
	if v := request("defaults_admin", "PATCH", path, `{"mode":"platform"}`, `"3"`, true, 200); v["version_token"] != "4" {
		t.Fatal("committed HTTP replay required a replacement Writer")
	}
	exec(`UPDATE endpoints SET state='active',deleted_at=NULL WHERE id=$1`, writer)
	if v := request("defaults_editor", "GET", "/api/v1/projects/"+project+"/endpoints/"+reader+"/connection-info", "", "", true, 200); v["role"] != "cloud_admin" || v["password_included"] != false {
		t.Fatal("public connection info exposed the wrong identity")
	}
}

func TestConnectionDefaultsNativeObservationScopeMarkerAndConnect(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := conn.Exec(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	role := "defaults_native_" + randomToken(5)
	const branch = "br_0123456789abcdef"
	tenant, timeline := strings.Repeat("a", 32), strings.Repeat("b", 32)
	exec(`SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, tenant, timeline)
	exec("CREATE ROLE " + pgQuote(role) + " LOGIN NOSUPERUSER")
	defer conn.Exec(context.Background(), "DROP ROLE "+pgQuote(role))
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS 'neon-control/branch/" + branch + "'")
	exec("COMMENT ON DATABASE control_ci IS 'neon-control/branch/" + branch + "'")
	original := connectionDefaultObservation{TenantID: tenant, TimelineID: timeline, PlatformDatabase: "control_ci"}
	input := connectionDefaultInput{Mode: "managed", Role: role, Database: "control_ci"}
	observe := func(v connectionDefaultObservation, managed bool, owner string, want int) connectionDefaultObservation {
		t.Helper()
		got, e := observeConnectionDefaultSQL(ctx, conn, input, v, managed, owner, branch)
		if want != 0 {
			var fault *catalogRenameFault
			if !errors.As(e, &fault) || fault.Status != want {
				t.Fatalf("wrong native observation rejection: %v", e)
			}
			return got
		}
		if e != nil {
			t.Fatal(e)
		}
		return got
	}
	got := observe(original, true, cfg.User, 0)
	if got.RoleOID == 0 || got.DatabaseOID == 0 {
		t.Fatal("native OIDs not recorded")
	}
	// These custom GUCs test the scope guard on disposable vanilla PostgreSQL;
	// the separate live Neon check must establish actual tenant/timeline values.
	wrong := original
	wrong.TimelineID = strings.Repeat("c", 32)
	observe(wrong, true, cfg.User, 409)
	observe(original, true, "wrong_owner", 409)
	exec("ALTER ROLE " + pgQuote(role) + " NOLOGIN")
	observe(original, true, cfg.User, 409)
	exec("ALTER ROLE " + pgQuote(role) + " LOGIN SUPERUSER")
	observe(original, true, cfg.User, 409)
	exec("ALTER ROLE " + pgQuote(role) + " NOSUPERUSER")
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS 'external'")
	observe(original, true, cfg.User, 409)
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS 'neon-control/branch/" + branch + "'")
	exec("COMMENT ON DATABASE control_ci IS 'external'")
	observe(original, true, cfg.User, 409)
	observe(original, false, "", 0)
	wrong = original
	wrong.PlatformDatabase = "another_default"
	observe(wrong, false, "", 409)
	exec("REVOKE CONNECT ON DATABASE control_ci FROM PUBLIC")
	defer conn.Exec(context.Background(), "GRANT CONNECT ON DATABASE control_ci TO PUBLIC")
	observe(original, false, "", 409)
	exec("GRANT CONNECT ON DATABASE control_ci TO PUBLIC")
	last := observe(original, false, "", 0)
	if last.RoleOID != got.RoleOID || last.DatabaseOID != got.DatabaseOID {
		t.Fatal("read-only observation changed object identity")
	}
	// postgres is a legitimate original bootstrap database for new Endpoints.
	// Allow selecting it only as that exact original database, never by treating
	// it as an ordinary managed catalog object eligible for rename or deletion.
	input.Database = "postgres"
	platform := original
	platform.PlatformDatabase = "postgres"
	if v := observe(platform, false, "", 0); v.DatabaseOID == 0 {
		t.Fatal("original platform postgres was not observed")
	}
	observe(original, false, "", 409)
}

func TestConnectionDefaultsInputKeepsPlatformIdentitiesProtected(t *testing.T) {
	for _, v := range []connectionDefaultInput{{Mode: "platform"}, {Mode: "managed", Role: "customer", Database: "neondb"}, {Mode: "managed", Role: "customer", Database: "postgres"}, {Mode: "managed", Role: "客户", Database: "应用"}} {
		if !v.valid() {
			t.Fatal("valid defaults rejected")
		}
	}
	for _, v := range []connectionDefaultInput{{Mode: "platform", Role: "cloud_admin"}, {Mode: "managed", Role: "cloud_admin", Database: "neondb"}, {Mode: "managed", Role: "control_probe", Database: "app"}, {Mode: "managed", Role: "customer", Database: "neon_admin"}, {Mode: "managed", Role: "customer", Database: "template1"}, {Mode: "managed", Role: "", Database: "app"}, {Mode: "other"}} {
		if v.valid() {
			t.Fatal("unsafe defaults accepted")
		}
	}
}
