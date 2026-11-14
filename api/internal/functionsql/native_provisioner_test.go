package functionsql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestNativeGrantsUseOrdinaryBoundProvisioner(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("dedicated disposable PostgreSQL required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/control_ci" {
		t.Fatal("disposable control_ci PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(id[:])
	scope := Scope{FunctionID: "fnc_" + suffix, ProjectID: "prj_" + suffix, BranchID: "br_" + suffix,
		Database: "fn_native_ci_" + suffix, Schema: "function_app"}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	exec := func(conn *pgx.Conn, command string) {
		t.Helper()
		if _, err := conn.Exec(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	const password = "disposable-function-native-probe-ci"
	exec(admin, "CREATE ROLE control_probe LOGIN NOCREATEROLE NOBYPASSRLS PASSWORD "+literal(password))
	defer admin.Exec(ctx, "DROP ROLE IF EXISTS control_probe")
	exec(admin, "CREATE DATABASE "+quote(scope.Database))
	defer admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(scope.Database)+" WITH (FORCE)")
	u.Path = "/" + scope.Database
	schemaAdmin, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer schemaAdmin.Close(context.Background())
	defer admin.Exec(ctx, "DROP ROLE IF EXISTS "+quote(scope.Role()))
	exec(schemaAdmin, "REVOKE TEMP ON DATABASE "+quote(scope.Database)+" FROM PUBLIC")
	exec(schemaAdmin, "CREATE SCHEMA function_app")
	exec(schemaAdmin, "CREATE TABLE function_app.items (id integer PRIMARY KEY)")
	exec(schemaAdmin, "GRANT USAGE ON SCHEMA function_app TO control_probe WITH GRANT OPTION")
	exec(schemaAdmin, "GRANT SELECT,INSERT,UPDATE,DELETE ON function_app.items TO control_probe WITH GRANT OPTION")
	u.User = url.UserPassword("control_probe", password)
	probe, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close(context.Background())
	bind := func(branch string) {
		t.Helper()
		if _, err := probe.Exec(ctx, `SELECT set_config('neon.project_id',$1,false),set_config('neon.branch_id',$2,false)`, scope.ProjectID, branch); err != nil {
			t.Fatal(err)
		}
	}
	apply := func() error {
		tx, err := probe.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err := ProvisionNativeGrantsTx(ctx, tx, scope); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	bind(scope.BranchID)
	if err := apply(); err == nil {
		t.Fatal("ordinary provisioner created a missing login")
	}
	exec(schemaAdmin, "CREATE ROLE "+quote(scope.Role())+" LOGIN NOINHERIT NOCREATEROLE")
	exec(schemaAdmin, "COMMENT ON ROLE "+quote(scope.Role())+" IS "+literal(scope.marker()))
	bind("br_ffffffffffffffff")
	if err := apply(); err == nil {
		t.Fatal("foreign branch obtained grants")
	}
	bind(scope.BranchID)
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
}
