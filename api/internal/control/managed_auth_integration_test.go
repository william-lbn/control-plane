package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"testing"
)

func TestManagedAuthSQLPrivilegeAndGeneration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL is required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var bytes [8]byte
	if _, err = rand.Read(bytes[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(bytes[:])
	database := "auth_ci_" + suffix
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(database)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgQuote(database)+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	target.Path = "/" + database
	conn, err := pgx.Connect(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	p := managedAuthPayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ManagedAuthSpec{Database: database, AllowedOrigins: []string{}}}
	login := managedAuthLogin(p.BranchID)
	defer func() {
		if _, e := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgQuote(login)); e != nil {
			t.Error(e)
		}
	}()
	// Cleanup LIFO: close/drop our disposable database before dropping its role.
	defer func() {
		_ = conn.Close(ctx)
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgQuote(database)+" WITH (FORCE)")
	}()
	verifier, err := scramVerifier(randomToken(32))
	if err != nil {
		t.Fatal(err)
	}
	apply := func() {
		t.Helper()
		tx, e := conn.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = applyManagedAuthSQLTx(ctx, tx, p, verifier); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	apply()
	if _, err = conn.Exec(ctx, `INSERT INTO neon_auth.jwks(id,"publicKey","privateKey","createdAt") VALUES('synthetic','public','encrypted',now())`); err != nil {
		t.Fatal(err)
	}
	apply() // A retry never resets issued signing material in the same generation.
	var count int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.jwks`).Scan(&count); err != nil || count != 1 {
		t.Fatal("same-generation retry erased signing material", err)
	}
	p.Generation = 2
	apply()
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.jwks`).Scan(&count); err != nil || count != 0 {
		t.Fatal("new generation retained old signing material", err)
	}
	var unsafe bool
	if err = conn.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication OR rolinherit OR has_schema_privilege(rolname,'neon_auth','CREATE') OR has_table_privilege(rolname,'neon_auth.control_installations','SELECT') FROM pg_roles WHERE rolname=$1`, login).Scan(&unsafe); err != nil || unsafe {
		t.Fatal("service role exceeds owned identity CRUD privileges", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `COMMENT ON SCHEMA neon_auth IS 'unowned-user-schema'`); err != nil {
		t.Fatal(err)
	}
	if applyManagedAuthSQLTx(ctx, tx, p, verifier) == nil {
		t.Fatal("unowned reserved schema was taken over")
	}
}
