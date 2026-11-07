package control

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Functional, small fixtures only. No blob writes or physical deletion.
func TestStorageGatewayLedgerCrossProcessQuotaAndUnknown(t *testing.T) {
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
	schema := "v2_storage_gateway_" + newID("")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgQuote(schema)); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgQuote(schema)+" CASCADE") }()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_gateway','local','Gateway','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_gateway','prj_gateway','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_gateway','prj_gateway','br_gateway','ep-storage-gateway','VirtualMachine','gateway-test','gateway-test','suspended','gateway_role','gateway_db',now(),now())`)
	exec(`INSERT INTO object_storage_instances(branch_id,project_id,endpoint_id,generation,state,spec) VALUES('br_gateway','prj_gateway','ep_gateway',1,'active','{"database":"gateway_db"}')`)
	exec(`INSERT INTO storage_gateway_limits(project_id,concurrent_requests) VALUES('prj_gateway',1)`)
	s, second := &server{db: db}, &server{db: other}
	admit := func(s *server, project string) (*storageUsage, error) {
		t.Helper()
		u := &storageUsage{s: s, protocol: "s3", started: time.Now()}
		r := httptest.NewRequest("PUT", "/s3/br_gateway/uploads/file", nil)
		r = r.WithContext(context.WithValue(ctx, storageUsageKey{}, u))
		return u, s.admitStorageUsage(r, project, "br_gateway")
	}
	first, err := admit(s, "prj_gateway")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admit(second, "prj_gateway"); !errors.Is(err, errStorageTenantCapacity) {
		t.Fatal("second process bypassed shared tenant cap", err)
	}
	if _, err = admit(second, "prj_other"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("caller-selected project accepted", err)
	}
	first.ingress, first.egress, first.status = 7, 5, 206
	first.finish()
	first.finish() // Duplicate finalization is a no-op, never double charged.
	var observed, reserved, requests int64
	if err = db.QueryRow(ctx, `SELECT observed_bytes,reserved_bytes,requests FROM storage_gateway_days WHERE project_id='prj_gateway'`).Scan(&observed, &reserved, &requests); err != nil || observed != 12 || reserved != 0 || requests != 1 {
		t.Fatal("non-atomic usage receipt", observed, reserved, requests, err)
	}
	if _, err = db.Exec(ctx, `UPDATE storage_gateway_requests SET egress_bytes=999 WHERE id=$1`, first.id); err == nil {
		t.Fatal("final receipt was editable")
	}
	if _, err = db.Exec(ctx, `DELETE FROM storage_gateway_requests WHERE id=$1`, first.id); err == nil {
		t.Fatal("receipt deleted")
	}
	// Request limit is committed admission count, not client success claims.
	exec(`UPDATE storage_gateway_limits SET daily_requests=1 WHERE project_id='prj_gateway'`)
	if _, err = admit(second, "prj_gateway"); !errors.Is(err, errStorageTenantCapacity) {
		t.Fatal("daily request quota bypassed", err)
	}
	exec(`UPDATE storage_gateway_limits SET daily_requests=0,daily_transfer_bytes=$1 WHERE project_id='prj_gateway'`, storageTransferReservation())
	if _, err = admit(second, "prj_gateway"); !errors.Is(err, errStorageTenantCapacity) {
		t.Fatal("observed plus worst-case reservation exceeded daily budget", err)
	}
	exec(`UPDATE storage_gateway_limits SET daily_transfer_bytes=0 WHERE project_id='prj_gateway'`)
	// Simulate a crashed gateway by inserting its retained, already-expired lease.
	exec(`WITH day AS (UPDATE storage_gateway_days SET requests=requests+1,reserved_bytes=reserved_bytes+$1 WHERE project_id='prj_gateway' RETURNING period)
 INSERT INTO storage_gateway_requests(id,project_id,branch_id,period,method,protocol,lease_until,reserved_bytes)
 SELECT $2,'prj_gateway','br_gateway',period,'GET','s3',clock_timestamp()-interval '1 second',$1 FROM day`, storageTransferReservation(), newID("sgr_"))
	next, err := admit(second, "prj_gateway")
	if err != nil {
		t.Fatal("expired crash lease prevented future concurrency", err)
	}
	next.failed, next.status = true, 503
	next.finish()
	if err = db.QueryRow(ctx, `SELECT reserved_bytes FROM storage_gateway_days WHERE project_id='prj_gateway'`).Scan(&reserved); err != nil || reserved != storageTransferReservation() {
		t.Fatal("crash fabricated zero usage or released unknown reservation", reserved, err)
	}
	// Least privilege lock helper resolves the real owner, not a supplied tenant.
	var project string
	if err = db.QueryRow(ctx, `SELECT * FROM storage_gateway_lock_project('br_gateway','prj_other')`).Scan(&project); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("lock helper accepted foreign expected project", err)
	}
	if err = db.QueryRow(ctx, `SELECT * FROM storage_gateway_lock_project('br_gateway','prj_gateway')`).Scan(&project); err != nil || project != "prj_gateway" {
		t.Fatal("canonical lifecycle lock unavailable", project, err)
	}
}
