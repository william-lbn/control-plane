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
	t.Run("actual_runtime_identity_probe", func(t *testing.T) {
		if err := VerifyRuntimeSession(ctx, runtime, s); err != nil || runtime.PgConn().TxStatus() != 'I' {
			t.Fatal("restricted authenticated session rejected or transaction retained", err)
		}
		if VerifyRuntimeSession(ctx, owner, s) == nil {
			t.Fatal("owner session accepted as restricted runtime")
		}
		other := s
		other.BranchID = "br_ffffffffffffffff"
		if VerifyRuntimeSession(ctx, runtime, other) == nil {
			t.Fatal("foreign branch marker accepted")
		}
	})
	t.Run("runtime_probe_preserves_callers_transaction", func(t *testing.T) {
		tx, err := runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if VerifyRuntimeSession(ctx, runtime, s) == nil || runtime.PgConn().TxStatus() != 'T' {
			t.Fatal("active caller transaction changed or admitted")
		}
	})
	t.Run("runtime_probe_rejects_session_policy_override", func(t *testing.T) {
		exec(runtime, `SET statement_timeout='0'`)
		if VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("unbounded session accepted")
		}
		exec(runtime, `SET statement_timeout='30s'`)
		if VerifyRuntimeSession(ctx, runtime, s) != nil {
			t.Fatal("restored session rejected")
		}
	})
	t.Run("actual_scram_and_dml", func(t *testing.T) {
		exec(runtime, `INSERT INTO app.items(value) VALUES('first'); UPDATE app.items SET value='second'`, pgx.QueryExecModeSimpleProtocol)
		var value string
		if err = runtime.QueryRow(ctx, `SELECT value FROM app.items`).Scan(&value); err != nil || value != "second" {
			t.Fatal("runtime DML failed", err)
		}
		exec(runtime, `DELETE FROM app.items`)
	})
	t.Run("actual_row_level_security_is_not_bypassed", func(t *testing.T) {
		exec(owner, `CREATE TABLE app.rls_rows(id integer PRIMARY KEY,visible boolean NOT NULL);
 INSERT INTO app.rls_rows VALUES(1,true),(2,false);
 ALTER TABLE app.rls_rows ENABLE ROW LEVEL SECURITY;
 CREATE POLICY visible_rows ON app.rls_rows USING(visible) WITH CHECK(visible)`, pgx.QueryExecModeSimpleProtocol)
		if err := apply(s); err != nil {
			t.Fatal("explicit RLS table grant failed", err)
		}
		var count int
		if err := runtime.QueryRow(ctx, `SELECT count(*) FROM app.rls_rows`).Scan(&count); err != nil || count != 1 {
			t.Fatal("runtime read exposed an RLS-hidden row", err)
		}
		if _, err := runtime.Exec(ctx, `INSERT INTO app.rls_rows VALUES(3,false)`); err == nil {
			t.Fatal("runtime insert bypassed RLS WITH CHECK")
		}
		if tag, err := runtime.Exec(ctx, `DELETE FROM app.rls_rows WHERE id=2`); err != nil || tag.RowsAffected() != 0 {
			t.Fatal("runtime deleted an RLS-hidden row", err)
		}
		exec(runtime, `SET row_security=off`)
		if _, err := runtime.Exec(ctx, `SELECT * FROM app.rls_rows`); err == nil {
			t.Fatal("session setting bypassed row-level security")
		}
		exec(runtime, `SET row_security=on`)
		exec(runtime, `INSERT INTO app.rls_rows VALUES(3,true)`)
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM app.rls_rows`).Scan(&count); err != nil || count != 3 {
			t.Fatal("owner's hidden fixture or permitted write was lost", err)
		}
		if VerifyRuntimeSession(ctx, runtime, s) != nil {
			t.Fatal("RLS policy or fixed runtime session boundary rejected")
		}
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
	t.Run("outside_column_grant_rejected", func(t *testing.T) {
		exec(owner, "GRANT USAGE ON SCHEMA private TO "+quote(s.Role()))
		exec(owner, "GRANT SELECT(value) ON private.secrets TO "+quote(s.Role()))
		var value string
		if err := runtime.QueryRow(ctx, `SELECT value FROM private.secrets`).Scan(&value); err != nil || value != "must-not-read" {
			t.Fatal("column grant permission fixture did not expose actual outside data", err)
		}
		if apply(s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("outside column grant admitted")
		}
		exec(owner, "REVOKE SELECT(value) ON private.secrets FROM "+quote(s.Role()))
		exec(owner, "REVOKE USAGE ON SCHEMA private FROM "+quote(s.Role()))
	})
	t.Run("literal_catalog_prefix_does_not_hide_customer_schema", func(t *testing.T) {
		exec(owner, `CREATE SCHEMA pgx_private; CREATE TABLE pgx_private.secret(value text); INSERT INTO pgx_private.secret VALUES('literal-prefix-private')`, pgx.QueryExecModeSimpleProtocol)
		exec(owner, "GRANT USAGE ON SCHEMA pgx_private TO "+quote(s.Role()))
		exec(owner, "GRANT SELECT ON pgx_private.secret TO "+quote(s.Role()))
		var value string
		if err := runtime.QueryRow(ctx, `SELECT value FROM pgx_private.secret`).Scan(&value); err != nil || value != "literal-prefix-private" {
			t.Fatal("ordinary pgx schema fixture did not expose its granted table", err)
		}
		if apply(s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("LIKE wildcard hid an ordinary customer schema")
		}
		exec(owner, "REVOKE SELECT ON pgx_private.secret FROM "+quote(s.Role()))
		exec(owner, "REVOKE USAGE ON SCHEMA pgx_private FROM "+quote(s.Role()))
	})
	for _, item := range []struct{ name, grant, revoke string }{
		{"truncate_rejected", "GRANT TRUNCATE ON app.items TO ", "REVOKE TRUNCATE ON app.items FROM "},
		{"trigger_privilege_rejected", "GRANT TRIGGER ON app.items TO ", "REVOKE TRIGGER ON app.items FROM "},
		{"column_references_rejected", "GRANT REFERENCES(value) ON app.items TO ", "REVOKE REFERENCES(value) ON app.items FROM "},
		{"table_grant_option_rejected", "GRANT SELECT ON app.items TO ", "REVOKE GRANT OPTION FOR SELECT ON app.items FROM "},
		{"column_grant_option_rejected", "GRANT SELECT(value) ON app.items TO ", "REVOKE GRANT OPTION FOR SELECT(value) ON app.items FROM "},
		{"sequence_update_rejected", "GRANT UPDATE ON app.items_id_seq TO ", "REVOKE UPDATE ON app.items_id_seq FROM "},
		{"sequence_grant_option_rejected", "GRANT USAGE ON app.items_id_seq TO ", "REVOKE GRANT OPTION FOR USAGE ON app.items_id_seq FROM "},
	} {
		t.Run(item.name, func(t *testing.T) {
			option := ""
			if strings.Contains(item.name, "grant_option") {
				option = " WITH GRANT OPTION"
			}
			exec(owner, item.grant+quote(s.Role())+option)
			if apply(s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
				t.Fatal("unexpected permission admitted")
			}
			exec(owner, item.revoke+quote(s.Role()))
			if VerifyRuntimeSession(ctx, runtime, s) != nil {
				t.Fatal("explicit owner repair did not restore boundary")
			}
		})
	}
	t.Run("same_schema_owner_view_rejected", func(t *testing.T) {
		exec(owner, `CREATE VIEW app.leak AS SELECT value FROM private.secrets; GRANT SELECT ON app.leak TO PUBLIC`, pgx.QueryExecModeSimpleProtocol)
		var value string
		if err := runtime.QueryRow(ctx, `SELECT value FROM app.leak`).Scan(&value); err != nil || value != "must-not-read" {
			t.Fatal("actual owner view did not expose fixture data", err)
		}
		if apply(s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("owner view adopted by ordinary-table grant")
		}
		exec(owner, `DROP VIEW app.leak`)
	})
	t.Run("enabled_trigger_rejected_without_public_execute", func(t *testing.T) {
		exec(owner, `CREATE FUNCTION app.trigger_leak() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$BEGIN NEW.value := (SELECT value FROM private.secrets LIMIT 1); RETURN NEW; END$$;
 REVOKE EXECUTE ON FUNCTION app.trigger_leak() FROM PUBLIC;
 CREATE TRIGGER leak BEFORE INSERT ON app.items FOR EACH ROW EXECUTE FUNCTION app.trigger_leak()`, pgx.QueryExecModeSimpleProtocol)
		var value string
		if err := runtime.QueryRow(ctx, `INSERT INTO app.items(value) VALUES('ordinary') RETURNING value`).Scan(&value); err != nil || value != "must-not-read" {
			t.Fatal("actual trigger permission fixture did not exercise owner execution", err)
		}
		if apply(s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("enabled executable trigger admitted")
		}
		exec(owner, `DROP TRIGGER leak ON app.items; DROP FUNCTION app.trigger_leak()`, pgx.QueryExecModeSimpleProtocol)
	})
	t.Run("new_table_requires_explicit_reconciliation", func(t *testing.T) {
		exec(owner, `CREATE TABLE app.later(id bigint)`)
		if VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("new ungranted application table silently admitted")
		}
		if apply(s) != nil || VerifyRuntimeSession(ctx, runtime, s) != nil {
			t.Fatal("owner-authorized reconciliation did not grant new table")
		}
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
