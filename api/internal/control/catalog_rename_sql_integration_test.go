package control

import (
	"context"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCatalogRenameActualPostgresOIDDataCredentialAndRecovery(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	suffix := randomToken(6)
	oldRole, newRole, oldDB, newDB, service := "rename_old_"+suffix, "rename_new_"+suffix, "rename_db_"+suffix, "renamed_db_"+suffix, "rename_service_"+suffix
	password := "ci-managed-rename-original-password"
	servicePassword := "ci-disposable-rename-service"
	verifier, err := scramVerifier(password)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := conn.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec("CREATE ROLE " + pgQuote(service) + " LOGIN CREATEROLE CREATEDB PASSWORD '" + servicePassword + "'")
	exec("CREATE ROLE " + pgQuote(oldRole) + " LOGIN PASSWORD '" + verifier + "'")
	branch := "br_0123456789abcdef"
	marker := "neon-control/branch/" + branch
	exec("COMMENT ON ROLE " + pgQuote(oldRole) + " IS " + pgLiteral(marker))
	exec("GRANT " + pgQuote(oldRole) + " TO " + pgQuote(service) + " WITH ADMIN TRUE, INHERIT TRUE, SET TRUE")
	exec("CREATE DATABASE " + pgQuote(oldDB) + " OWNER " + pgQuote(oldRole))
	exec("COMMENT ON DATABASE " + pgQuote(oldDB) + " IS " + pgLiteral(marker))
	// All these names are confined to this ephemeral PostgreSQL instance. Keep
	// original objects available until every connection and assertion completes.
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = conn.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgQuote(newDB))
		_, _ = conn.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgQuote(oldDB))
		for _, name := range []string{newRole, oldRole, service} {
			_, _ = conn.Exec(cleanup, "DROP ROLE IF EXISTS "+pgQuote(name))
		}
	}()
	var roleOID, dbOID uint32
	if err = conn.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname=$1`, oldRole).Scan(&roleOID); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT oid FROM pg_database WHERE datname=$1`, oldDB).Scan(&dbOID); err != nil {
		t.Fatal(err)
	}
	dataCfg := cfg.Copy()
	dataCfg.Database = oldDB
	data, err := pgx.ConnectConfig(ctx, dataCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = data.Exec(ctx, `CREATE TABLE public.retained_marker(value text); INSERT INTO public.retained_marker VALUES('original-business-bytes')`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	if _, err = data.Exec(ctx, "ALTER TABLE public.retained_marker OWNER TO "+pgQuote(oldRole)); err != nil {
		t.Fatal(err)
	}
	data.Close(ctx)
	serviceCfg := cfg.Copy()
	serviceCfg.User, serviceCfg.Password = service, servicePassword
	creator, err := pgx.ConnectConfig(ctx, serviceCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer creator.Close(ctx)
	var super bool
	if err = creator.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&super); err != nil || super {
		t.Fatal("non-superuser service required", err)
	}
	p := renamePayloadForTest()
	p.BranchID = branch
	p.Name, p.NewName, p.ObjectOID = oldRole, newRole, roleOID
	if err = applyCatalogRenameSQL(ctx, creator, p, verifier); err != nil {
		t.Fatal(err)
	}
	if err = applyCatalogRenameSQL(ctx, creator, p, verifier); err != nil {
		t.Fatal("lost result recovery failed", err)
	}
	runtimeCfg := cfg.Copy()
	runtimeCfg.User, runtimeCfg.Password, runtimeCfg.Database = newRole, password, oldDB
	runtime, err := pgx.ConnectConfig(ctx, runtimeCfg)
	if err != nil {
		t.Fatal("original credential was not retained", err)
	}
	var value string
	if err = runtime.QueryRow(ctx, `SELECT value FROM public.retained_marker`).Scan(&value); err != nil || value != "original-business-bytes" {
		t.Fatal("original grants/data changed", err)
	}
	runtime.Close(ctx)
	dbPayload := p
	dbPayload.Kind, dbPayload.Name, dbPayload.NewName, dbPayload.ObjectOID, dbPayload.Owner = "database", oldDB, newDB, dbOID, newRole
	if err = applyCatalogRenameSQL(ctx, creator, dbPayload, ""); err != nil {
		t.Fatal(err)
	}
	if err = applyCatalogRenameSQL(ctx, creator, dbPayload, ""); err != nil {
		t.Fatal("database lost result recovery", err)
	}
	runtimeCfg.Database = newDB
	runtime, err = pgx.ConnectConfig(ctx, runtimeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.QueryRow(ctx, `SELECT value FROM public.retained_marker`).Scan(&value); err != nil || value != "original-business-bytes" {
		t.Fatal(err)
	}
	runtime.Close(ctx)
	var actual uint32
	if err = conn.QueryRow(ctx, `SELECT oid FROM pg_database WHERE datname=$1`, newDB).Scan(&actual); err != nil || actual != dbOID {
		t.Fatal("database OID changed", err)
	}
	wrong := dbPayload
	wrong.ObjectOID++
	if err = applyCatalogRenameSQL(ctx, creator, wrong, ""); err == nil {
		t.Fatal("foreign OID adopted")
	}
	// A later independent object can legitimately reuse the old name. Recovery
	// must then reject the collision, leaving both objects and credentials intact.
	exec("CREATE ROLE " + pgQuote(oldRole) + " NOLOGIN")
	if err = applyCatalogRenameSQL(ctx, creator, p, verifier); err == nil {
		t.Fatal("recreated old role silently adopted")
	}
	invalid := p
	invalid.NewName = "postgres"
	if err = applyCatalogRenameSQL(ctx, creator, invalid, verifier); err == nil {
		t.Fatal("system role rename accepted")
	}
	if err = applyCatalogRenameSQL(ctx, creator, p, strings.Repeat("x", 64)); err == nil {
		t.Fatal("invalid managed credential accepted")
	}
}
