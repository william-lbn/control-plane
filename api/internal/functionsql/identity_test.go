package functionsql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestScopeValidation(t *testing.T) {
	s := Scope{"fnc_0123456789abcdef", "prj_0123456789abcdef", "br_0123456789abcdef", "neondb", "app"}
	if s.Validate() != nil || s.Role() != "fn_0123456789abcdef" {
		t.Fatal("valid immutable scope rejected")
	}
	for _, name := range []string{"pg_catalog", "neon_auth", "control_manifest", "information_schema", "app; DROP ROLE x", "a-b", "", strings.Repeat("x", 64)} {
		s.Schema = name
		if s.Validate() == nil {
			t.Fatal("reserved/invalid schema admitted", name)
		}
	}
}

// This is a real PostgreSQL authentication/authorization test, not a SQL-string
// assertion. The dedicated database is disposable and never a lab/user branch.
func TestRestrictedIdentityAuthenticationAndRollback(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "control_ci" {
		t.Fatal("disposable control_ci PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(random[:])
	s := Scope{"fnc_" + suffix, "prj_" + suffix, "br_" + suffix, "fn_ci_" + suffix, "app"}
	exec := func(c *pgx.Conn, q string, args ...any) {
		t.Helper()
		if _, err := c.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(admin, "CREATE DATABASE "+quote(s.Database))
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(s.Database)+" WITH (FORCE)")
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+quote(s.Role()))
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + s.Database
	owner, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	exec(owner, `CREATE SCHEMA app; CREATE SCHEMA private; CREATE TABLE app.items(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,value text NOT NULL); CREATE TABLE private.secrets(value text); INSERT INTO private.secrets VALUES('must-not-read')`, pgx.QueryExecModeSimpleProtocol)
	// PostgreSQL defaults TEMP to PUBLIC. Refuse an overpowered shared DB until
	// its owner explicitly changes that policy; provisioning never does it.
	password := "function-disposable-" + suffix
	exec(owner, "CREATE ROLE "+quote(s.Role())+" NOLOGIN NOINHERIT")
	exec(owner, "COMMENT ON ROLE "+quote(s.Role())+" IS "+literal(s.marker()))
	exec(owner, "SET password_encryption='scram-sha-256'")
	exec(owner, "ALTER ROLE "+quote(s.Role())+" PASSWORD "+literal(password))
	var verifier string
	if err = owner.QueryRow(ctx, `SELECT rolpassword FROM pg_authid WHERE rolname=$1`, s.Role()).Scan(&verifier); err != nil {
		t.Fatal(err)
	}
	apply := func(scope Scope) error {
		tx, e := owner.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if e = ProvisionTx(ctx, tx, scope, verifier); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	t.Run("public_temp_requires_owner_action", func(t *testing.T) {
		if apply(s) == nil {
			t.Fatal("PUBLIC TEMP silently admitted")
		}
	})
	exec(owner, "REVOKE TEMPORARY ON DATABASE "+quote(s.Database)+" FROM PUBLIC")
	if err = apply(s); err != nil {
		t.Fatal(err)
	}
	if err = apply(s); err != nil {
		t.Fatal("same-scope retry", err)
	}
	u.User = url.UserPassword(s.Role(), password)
	runtime, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	t.Run("actual_scram_and_dml", func(t *testing.T) {
		exec(runtime, `INSERT INTO app.items(value) VALUES('first'); UPDATE app.items SET value='second'`, pgx.QueryExecModeSimpleProtocol)
		var value string
		if err = runtime.QueryRow(ctx, `SELECT value FROM app.items`).Scan(&value); err != nil || value != "second" {
			t.Fatal("runtime DML failed", err)
		}
		exec(runtime, `DELETE FROM app.items`)
	})
	// After a permission error the connection remains usable because each
	// negative example is a separate statement, not an aborted transaction.
	for _, item := range []struct{ name, q string }{{"private_read", "SELECT * FROM private.secrets"}, {"private_write", "INSERT INTO private.secrets VALUES('bad')"}, {"schema_create", "CREATE TABLE app.bad(id int)"}, {"temp_create", "CREATE TEMP TABLE bad(id int)"}, {"role_admin", "CREATE ROLE fn_bad"}, {"role_switch", "SET ROLE postgres"}, {"auth_catalog", "SELECT rolpassword FROM pg_authid"}} {
		t.Run(item.name, func(t *testing.T) {
			if _, e := runtime.Exec(ctx, item.q); e == nil {
				t.Fatal("ungranted SQL admitted")
			}
		})
	}
	t.Run("wrong_branch_marker", func(t *testing.T) {
		other := s
		other.BranchID = "br_ffffffffffffffff"
		if apply(other) == nil {
			t.Fatal("sibling branch adopted original role")
		}
	})
	t.Run("wrong_database", func(t *testing.T) {
		other := s
		other.Database = "other_database"
		if apply(other) == nil {
			t.Fatal("different database admitted")
		}
	})
	t.Run("public_outside_read_rejected", func(t *testing.T) {
		exec(owner, `GRANT SELECT ON private.secrets TO PUBLIC`)
		if apply(s) == nil {
			t.Fatal("PUBLIC data access admitted")
		}
		exec(owner, `REVOKE SELECT ON private.secrets FROM PUBLIC`)
	})
	t.Run("security_definer_rejected", func(t *testing.T) {
		exec(owner, `CREATE FUNCTION app.escape() RETURNS text LANGUAGE sql SECURITY DEFINER AS 'SELECT value FROM private.secrets LIMIT 1'`)
		if apply(s) == nil {
			t.Fatal("PUBLIC SECURITY DEFINER admitted")
		}
		exec(owner, `REVOKE EXECUTE ON FUNCTION app.escape() FROM PUBLIC`)
	})
	t.Run("elevated_role_not_repaired", func(t *testing.T) {
		exec(owner, "ALTER ROLE "+quote(s.Role())+" CREATEROLE")
		if apply(s) == nil {
			t.Fatal("elevated role silently repaired")
		}
		exec(owner, "ALTER ROLE "+quote(s.Role())+" NOCREATEROLE")
	})
	t.Run("rollback_no_orphan_login", func(t *testing.T) {
		other := s
		other.FunctionID = "fnc_ffffffffffffffff"
		exec(owner, `GRANT SELECT ON private.secrets TO PUBLIC`)
		if apply(other) == nil {
			t.Fatal("unsafe fresh role admitted")
		}
		var exists bool
		if err = owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, other.Role()).Scan(&exists); err != nil || exists {
			t.Fatal("failed transaction left role", err)
		}
		exec(owner, `REVOKE SELECT ON private.secrets FROM PUBLIC`)
	})
	if err = apply(s); err != nil {
		t.Fatal("owner-authorized repair failed", err)
	}
}
