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

// PG16 CREATEROLE's implicit ADMIN grant is different from a customer's role
// inheritance. Exercise actual non-superuser creation/authentication rather than
// weakening the existing superuser test's membership predicate.
func TestPG16ExplicitProvisioningPrincipal(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "control_ci" {
		t.Fatal("disposable control_ci PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	principal := "fn_provisioner_" + suffix
	foreign := "fn_foreign_" + suffix
	seed := "fn_seed_" + suffix
	s := Scope{FunctionID: "fnc_" + suffix, ProjectID: "prj_" + suffix, BranchID: "br_" + suffix,
		Database: "fn_ci_" + suffix, Schema: "app", ProvisioningRole: principal}
	exec := func(conn *pgx.Conn, sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	password := "disposable-function-" + suffix
	ownerPassword := "disposable-provisioner-" + suffix
	exec(admin, "CREATE ROLE "+quote(principal)+" LOGIN CREATEROLE NOSUPERUSER NOCREATEDB NOBYPASSRLS NOREPLICATION PASSWORD "+literal(ownerPassword))
	exec(admin, "CREATE ROLE "+quote(foreign)+" NOLOGIN")
	exec(admin, "CREATE DATABASE "+quote(s.Database)+" OWNER "+quote(principal))
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(s.Database)+" WITH (FORCE)")
		for _, role := range []string{s.Role(), seed, foreign, principal} {
			_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+quote(role))
		}
	}()
	// Generate an actual SCRAM verifier on this disposable server. The target
	// role does not exist when the non-superuser ProvisionTx creates it.
	exec(admin, "SET password_encryption='scram-sha-256'")
	exec(admin, "CREATE ROLE "+quote(seed)+" NOLOGIN PASSWORD "+literal(password))
	var verifier string
	if err = admin.QueryRow(ctx, `SELECT rolpassword FROM pg_authid WHERE rolname=$1`, seed).Scan(&verifier); err != nil {
		t.Fatal(err)
	}
	exec(admin, "DROP ROLE "+quote(seed))
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + s.Database
	u.User = url.UserPassword(principal, ownerPassword)
	owner, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	exec(owner, "REVOKE TEMPORARY ON DATABASE "+quote(s.Database)+" FROM PUBLIC")
	exec(owner, `CREATE SCHEMA app; CREATE TABLE app.items(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,value text NOT NULL)`, pgx.QueryExecModeSimpleProtocol)
	apply := func(conn *pgx.Conn, scope Scope) error {
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if e = ProvisionTx(ctx, tx, scope, verifier); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	t.Run("creator_self_grant_rejected_before_role_creation", func(t *testing.T) {
		exec(owner, "SET createrole_self_grant='SET'")
		if apply(owner, s) == nil {
			t.Fatal("automatic SET membership admitted")
		}
		var exists bool
		if err := owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, s.Role()).Scan(&exists); err != nil || exists {
			t.Fatal("failed precondition left a role")
		}
		exec(owner, "RESET createrole_self_grant")
	})
	if err = apply(owner, s); err != nil {
		t.Fatal("actual CREATEROLE provisioning failed", err)
	}
	if err = apply(owner, s); err != nil {
		t.Fatal("original creator replay failed", err)
	}
	u.User = url.UserPassword(s.Role(), password)
	runtime, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	t.Run("actual_non_superuser_runtime_and_admin_only_membership", func(t *testing.T) {
		if VerifyRuntimeSession(ctx, runtime, s) != nil {
			t.Fatal("valid restricted runtime rejected")
		}
		var count int
		err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles f ON f.oid=m.roleid JOIN pg_roles p ON p.oid=m.member WHERE f.rolname=$1 AND p.rolname=$2 AND m.grantor=10 AND m.admin_option AND NOT m.inherit_option AND NOT m.set_option`, s.Role(), principal).Scan(&count)
		if err != nil || count != 1 {
			t.Fatal("original implicit membership mismatch")
		}
		exec(runtime, `INSERT INTO items(value) VALUES('restricted-nonsuper'); UPDATE items SET value='verified-nonsuper' WHERE value='restricted-nonsuper'`, pgx.QueryExecModeSimpleProtocol)
		var value string
		if err := owner.QueryRow(ctx, `SELECT value FROM app.items`).Scan(&value); err != nil || value != "verified-nonsuper" {
			t.Fatal("real DML not observed by owner")
		}
		for _, check := range []struct {
			conn *pgx.Conn
			sql  string
		}{{owner, "SET ROLE " + quote(s.Role())}, {runtime, "SET ROLE " + quote(principal)}, {runtime, `CREATE TEMP TABLE denied(id int)`}} {
			if _, err := check.conn.Exec(ctx, check.sql); err == nil {
				t.Fatal("forbidden inheritance/role switch/TEMP admitted")
			}
		}
	})
	t.Run("immutable_principal_marker_and_authenticated_creator", func(t *testing.T) {
		wrong := s
		wrong.ProvisioningRole = foreign
		if VerifyRuntimeSession(ctx, runtime, wrong) == nil || apply(owner, wrong) == nil {
			t.Fatal("foreign creator scope admitted")
		}
		legacy := s
		legacy.ProvisioningRole = ""
		if VerifyRuntimeSession(ctx, runtime, legacy) == nil || apply(owner, legacy) == nil {
			t.Fatal("legacy scope adopted creator-managed role")
		}
		// SET ROLE changes current_user but not session_user; it cannot authorize
		// a different authenticated control identity to impersonate the creator.
		u.User = url.UserPassword(cfg.User, cfg.Password)
		other, err := pgx.Connect(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close(context.Background())
		exec(other, "SET ROLE "+quote(principal))
		if apply(other, s) == nil {
			t.Fatal("authenticated principal mismatch admitted")
		}
	})
	for _, option := range []string{"INHERIT", "SET"} {
		t.Run("extra_creator_"+strings.ToLower(option)+"_grant_rejected", func(t *testing.T) {
			exec(owner, "GRANT "+quote(s.Role())+" TO "+quote(principal)+" WITH "+option+" TRUE")
			if apply(owner, s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
				t.Fatal("extra creator grant admitted")
			}
			// Revoke only this creator's explicit grant; bootstrap grant persists.
			exec(owner, "REVOKE "+quote(s.Role())+" FROM "+quote(principal)+" GRANTED BY CURRENT_USER")
			if VerifyRuntimeSession(ctx, runtime, s) != nil {
				t.Fatal("original implicit grant not preserved")
			}
		})
	}
	t.Run("creator_cannot_grant_admin_back_to_own_grantor", func(t *testing.T) {
		// PostgreSQL itself rejects this ADMIN cycle. It is not a valid fixture
		// for our catalogue guard; assert the real server failure and unchanged
		// original scope instead of pretending a forbidden grant was installed.
		if _, err := owner.Exec(ctx, "GRANT "+quote(s.Role())+" TO "+quote(principal)+" WITH ADMIN TRUE, INHERIT FALSE, SET FALSE"); err == nil {
			t.Fatal("PostgreSQL unexpectedly admitted an ADMIN grantor cycle")
		}
		if VerifyRuntimeSession(ctx, runtime, s) != nil {
			t.Fatal("rejected ADMIN cycle changed original identity")
		}
	})
	t.Run("extra_admin_false_grant_with_foreign_grantor_rejected", func(t *testing.T) {
		exec(owner, "GRANT "+quote(s.Role())+" TO "+quote(principal)+" WITH ADMIN FALSE, INHERIT FALSE, SET FALSE")
		if apply(owner, s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("different grantor admitted despite identical options")
		}
		exec(owner, "REVOKE "+quote(s.Role())+" FROM "+quote(principal)+" GRANTED BY CURRENT_USER")
	})
	t.Run("foreign_incoming_membership_rejected", func(t *testing.T) {
		exec(owner, "GRANT "+quote(s.Role())+" TO "+quote(foreign)+" WITH INHERIT FALSE, SET FALSE")
		if apply(owner, s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("foreign grantee admitted")
		}
		exec(owner, "REVOKE "+quote(s.Role())+" FROM "+quote(foreign))
	})
	t.Run("runtime_member_of_other_role_rejected", func(t *testing.T) {
		exec(admin, "GRANT "+quote(foreign)+" TO "+quote(s.Role())+" WITH INHERIT FALSE, SET FALSE")
		if apply(owner, s) == nil || VerifyRuntimeSession(ctx, runtime, s) == nil {
			t.Fatal("runtime outgoing membership admitted")
		}
		exec(admin, "REVOKE "+quote(foreign)+" FROM "+quote(s.Role()))
	})
	if VerifyRuntimeSession(ctx, runtime, s) != nil {
		t.Fatal("restored original scope rejected")
	}
}
