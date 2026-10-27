package control

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Applies the operator's real grants file to a NOLOGIN fixture identity. This
// never uses the privileged metadata owner as the gateway and never writes a
// blob, starts Compute or changes an existing tenant's limits/credentials.
func TestStorageGatewayRestrictedMetadataRole(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated disposable control_ci required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := newID("")
	schema, role := "v2_storage_gateway_role_"+suffix, "storage_role_"+suffix
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgQuote(schema)); err != nil {
		t.Fatal(err)
	}
	var owner, gateway *pgxpool.Pool
	roleCreated := false
	defer func() {
		if gateway != nil {
			gateway.Close()
		}
		if owner != nil {
			owner.Close()
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, e := admin.Exec(cleanup, "DROP SCHEMA "+pgQuote(schema)+" CASCADE"); e != nil {
			t.Error(e)
		}
		if roleCreated {
			if _, e := admin.Exec(cleanup, "DROP ROLE "+pgQuote(role)); e != nil {
				t.Error(e)
			}
		}
	}()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	owner, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := owner.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec("CREATE ROLE " + pgQuote(role) + " NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION NOINHERIT")
	roleCreated = true
	raw, err := os.ReadFile("../../../tools/storage-gateway/metadata-grants.sql")
	if err != nil {
		t.Fatal("operator grants source required", err)
	}
	grants := string(raw)
	if !strings.Contains(grants, `GRANT USAGE ON SCHEMA public TO :"gateway_role";`) {
		t.Fatal("operator grants schema declaration changed; review fixture adaptation")
	}
	grants = strings.ReplaceAll(grants, `GRANT USAGE ON SCHEMA public TO :"gateway_role";`, "GRANT USAGE ON SCHEMA "+pgQuote(schema)+` TO :"gateway_role";`)
	grants = strings.ReplaceAll(grants, `:"gateway_role"`, pgQuote(role))
	apply := func() { t.Helper(); exec(grants, pgx.QueryExecModeSimpleProtocol) }
	apply()
	restricted := cfg.Copy()
	restricted.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, e := conn.Exec(ctx, "SET ROLE "+pgQuote(role))
		return e
	}
	gateway, err = pgxpool.NewWithConfig(ctx, restricted)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateStorageGatewayRole(ctx, gateway); err != nil {
		t.Fatal("real restricted grants failed readiness", err)
	}
	if err = validateStorageGatewayRole(ctx, owner); err == nil {
		t.Fatal("metadata owner accepted as gateway identity")
	}
	for _, revoke := range []string{
		"REVOKE INSERT ON storage_gateway_requests FROM ",
		"REVOKE UPDATE(last_used_at) ON backend_credentials FROM ",
		"REVOKE UPDATE(egress_bytes) ON storage_gateway_requests FROM ",
		"REVOKE UPDATE(observed_bytes) ON storage_gateway_days FROM ",
		"REVOKE SELECT(disabled) ON users FROM ",
		"REVOKE INSERT(request_id) ON audit_events FROM ",
		"REVOKE UPDATE(generation) ON storage_event_branches FROM ",
		"REVOKE EXECUTE ON FUNCTION storage_gateway_lock_project(text,text) FROM ",
		"REVOKE EXECUTE ON FUNCTION storage_gateway_lock_branch(text,text) FROM ",
		"REVOKE EXECUTE ON FUNCTION storage_gateway_lock_instance(text,text) FROM ",
	} {
		exec(revoke + pgQuote(role))
		if err = validateStorageGatewayRole(ctx, gateway); err == nil {
			t.Fatalf("incomplete role was ready after %s", revoke)
		}
		apply() // Restore from the actual operator script, never a mirrored grant set.
		if err = validateStorageGatewayRole(ctx, gateway); err != nil {
			t.Fatal("operator grants did not restore readiness", err)
		}
	}
	project, branch, endpoint := "prj_"+suffix, "br_"+suffix, "ep_"+suffix
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'local','Gateway role','lab',16,'ready','managed',now(),now())`, project)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now())`, branch, project)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$4,'VirtualMachine','role-fixture','role-fixture','suspended','role_fixture','role_fixture',now(),now())`, endpoint, project, branch, "ep-"+suffix)
	exec(`INSERT INTO object_storage_instances(branch_id,project_id,endpoint_id,generation,state,spec) VALUES($1,$2,$3,1,'active','{"database":"role_fixture"}')`, branch, project, endpoint)
	denied := func(query string) {
		t.Helper()
		_, e := gateway.Exec(ctx, query)
		var fault *pgconn.PgError
		if !errors.As(e, &fault) || fault.Code != "42501" {
			t.Fatalf("expected permission denial for %s: %v", query, e)
		}
	}
	denied(`SELECT password_hash FROM users`)
	denied(`SELECT * FROM sessions`)
	denied(`UPDATE projects SET state='ready'`)
	denied(`SELECT id FROM projects FOR SHARE`)
	denied(`INSERT INTO operations(id) VALUES('op_role_forbidden')`)
	tx, err := gateway.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var id, ep string
	var generation int64
	var spec []byte
	if err = tx.QueryRow(ctx, `SELECT * FROM storage_gateway_lock_project($1,$2)`, branch, project).Scan(&id); err != nil || id != project {
		t.Fatal("restricted project row lock failed", err)
	}
	if err = tx.QueryRow(ctx, `SELECT * FROM storage_gateway_lock_branch($1,$2)`, branch, project).Scan(&id); err != nil || id != branch {
		t.Fatal("restricted branch row lock failed", err)
	}
	if err = tx.QueryRow(ctx, `SELECT * FROM storage_gateway_lock_instance($1,$2)`, branch, project).Scan(&ep, &generation, &spec); err != nil || ep != endpoint || generation != 1 {
		t.Fatal("restricted instance row lock failed", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	s := &server{db: gateway, processRole: roleStorageGateway}
	usage := &storageUsage{s: s, protocol: "rest", started: time.Now()}
	request := httptest.NewRequest("GET", "/storage/v1/fixture/bucket", nil).WithContext(context.WithValue(ctx, storageUsageKey{}, usage))
	if err = s.admitStorageUsage(request, project, branch); err != nil {
		t.Fatal("restricted ledger admission failed", err)
	}
	usage.ingress, usage.egress, usage.status = 3, 4, 200
	usage.finish()
	var state string
	var ingress, egress, reserved int64
	if err = owner.QueryRow(ctx, `SELECT r.state,r.ingress_bytes,r.egress_bytes,d.reserved_bytes FROM storage_gateway_requests r JOIN storage_gateway_days d USING(project_id,period) WHERE r.id=$1`, usage.id).Scan(&state, &ingress, &egress, &reserved); err != nil || state != "completed" || ingress != 3 || egress != 4 || reserved != 0 {
		t.Fatal("restricted ledger finalization failed", state, ingress, egress, reserved, err)
	}
	if _, err = gateway.Exec(ctx, `UPDATE backend_credentials SET last_used_at=clock_timestamp() WHERE false`); err != nil {
		t.Fatal("last_used_at column update denied", err)
	}
	if _, err = gateway.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES('role_fixture','presigned_put_object','object_storage',$1,'succeeded','role_fixture')`, branch); err != nil {
		t.Fatal("native upload audit insert denied", err)
	}
	// A column-level lifecycle grant must also be rejected, not just table UPDATE.
	exec("GRANT UPDATE(state) ON projects TO " + pgQuote(role))
	if err = validateStorageGatewayRole(ctx, gateway); err == nil {
		t.Fatal("column-level lifecycle mutation privilege accepted")
	}
}
