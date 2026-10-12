package control

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exercise actual PostgreSQL privilege enforcement with a non-superuser and
// original OID/marker guards. This isolated control_ci fixture is never Neon
// runtime acceptance; the real Proxy/UI gate remains independent.
func TestDatabaseOwnerSQLNonSuperuserAndUnknownOutcome(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "control_ci" {
		t.Fatal("isolated control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	suffix := strings.ToLower(randomToken(6))
	old, newOwner, probe, name := "ci_owner_old_"+suffix, "ci_owner_new_"+suffix, "ci_owner_probe_"+suffix, "ci_owner_db_"+suffix
	run := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = conn.Exec(cleanup, "RESET SESSION AUTHORIZATION")
		_, _ = conn.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgQuote(name))
		for _, role := range []string{probe, newOwner, old} {
			_, _ = conn.Exec(cleanup, "DROP ROLE IF EXISTS "+pgQuote(role))
		}
	}()
	for _, role := range []string{old, newOwner, probe} {
		run("CREATE ROLE " + pgQuote(role) + " NOSUPERUSER CREATEDB INHERIT")
	}
	run("GRANT " + pgQuote(old) + "," + pgQuote(newOwner) + " TO " + pgQuote(probe) + " WITH INHERIT TRUE, SET TRUE")
	p := catalogPayload{BranchID: "br_" + suffix, Name: name, PreviousOwner: old, Owner: newOwner, ExpectedVersion: 1}
	marker := "neon-control/branch/" + p.BranchID
	for _, role := range []string{old, newOwner} {
		run("COMMENT ON ROLE " + pgQuote(role) + " IS '" + marker + "'")
	}
	run("CREATE DATABASE " + pgQuote(name) + " OWNER " + pgQuote(old))
	run("COMMENT ON DATABASE " + pgQuote(name) + " IS '" + marker + "'")
	if err = conn.QueryRow(ctx, "SELECT oid FROM pg_database WHERE datname=$1", name).Scan(&p.DatabaseOID); err != nil {
		t.Fatal(err)
	}
	run("SET SESSION AUTHORIZATION " + pgQuote(probe))
	var super bool
	if err = conn.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname=current_user").Scan(&super); err != nil || super {
		t.Fatal("non-superuser fixture", err)
	}
	t.Run("actual_owner_transfer", func(t *testing.T) {
		if err := applyDatabaseOwnerSQL(ctx, conn, p); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("same_original_oid_replay", func(t *testing.T) {
		if err := applyDatabaseOwnerSQL(ctx, conn, p); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("wrong_oid_rejected", func(t *testing.T) {
		bad := p
		bad.DatabaseOID++
		if applyDatabaseOwnerSQL(ctx, conn, bad) == nil {
			t.Fatal("adopted another database")
		}
	})
	t.Run("foreign_branch_marker_rejected", func(t *testing.T) {
		bad := p
		bad.BranchID += "other"
		if applyDatabaseOwnerSQL(ctx, conn, bad) == nil {
			t.Fatal("foreign branch admitted")
		}
	})
	t.Run("protected_target_rejected", func(t *testing.T) {
		bad := p
		bad.Owner = "control_probe"
		if applyDatabaseOwnerSQL(ctx, conn, bad) == nil {
			t.Fatal("protected role admitted")
		}
	})
	t.Run("third_owner_rejected", func(t *testing.T) {
		bad := p
		bad.Owner = probe
		if applyDatabaseOwnerSQL(ctx, conn, bad) == nil {
			t.Fatal("external owner change adopted")
		}
	})
	var actual string
	if err = conn.QueryRow(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE oid=$1", p.DatabaseOID).Scan(&actual); err != nil || actual != newOwner {
		t.Fatal("original database owner not retained", actual, err)
	}
}

func TestDatabaseOwnerCommitCASAndReplay(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("isolated control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_owner_" + randomToken(6)
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
		if _, err = db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org','owner-ci','Owner')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('project','org','Owner','rke2-lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('branch','project','main','ready',now())`)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES('branch','owned','old','updating')`)
	p := catalogPayload{BranchID: "branch", Name: "owned", PreviousOwner: "old", Owner: "new", ExpectedVersion: 1}
	apply := func(p catalogPayload) error {
		tx, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err = commitDatabaseOwnerTx(ctx, tx, p); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err = apply(p); err != nil {
		t.Fatal(err)
	}
	if err = apply(p); err != nil {
		t.Fatal("lost acknowledgement replay", err)
	}
	var owner, state string
	var version int64
	if err = db.QueryRow(ctx, `SELECT owner_name,state,version FROM branch_databases WHERE branch_id='branch' AND name='owned'`).Scan(&owner, &state, &version); err != nil || owner != "new" || state != "ready" || version != 2 {
		t.Fatal("not committed exactly once", err)
	}
	bad := p
	bad.Owner = "foreign"
	if apply(bad) == nil {
		t.Fatal("foreign outcome adopted")
	}
	exec(`UPDATE branch_databases SET version=3 WHERE branch_id='branch' AND name='owned'`)
	if apply(p) == nil {
		t.Fatal("stale operation overwrote a later catalog version")
	}
}
