package control

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

func TestFunctionBranchCopyMissingGrantOptionRollsBackBeforePublication(t *testing.T) {
	f := newStorageEventFixture(t)
	creator := "copygrant_" + randomToken(5)
	quoted := pgx.Identifier{creator}.Sanitize()
	database := f.native.Config().Database
	exec := func(sql string) {
		t.Helper()
		if _, err := f.native.Exec(f.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE ROLE " + quoted + " LOGIN CREATEROLE NOINHERIT PASSWORD 'copy-grant-disposable'")
	exec("REVOKE CREATE,TEMP ON DATABASE " + pgx.Identifier{database}.Sanitize() + " FROM PUBLIC")
	exec("GRANT CONNECT ON DATABASE " + pgx.Identifier{database}.Sanitize() + " TO " + quoted + " WITH GRANT OPTION")
	exec("CREATE SCHEMA app")
	exec("CREATE TABLE app.copy_grant_probe(id int PRIMARY KEY)")
	exec("GRANT USAGE ON SCHEMA app TO " + quoted)
	exec("GRANT SELECT,INSERT,UPDATE,DELETE ON app.copy_grant_probe TO " + quoted + " WITH GRANT OPTION")
	cfg := f.native.Config().Copy()
	cfg.User = creator
	cfg.Password = "copy-grant-disposable"
	conn, err := pgx.ConnectConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(f.ctx)
	scope := functionsql.Scope{FunctionID: "fnc_6123456789abcdef", ProjectID: f.p.ProjectID, BranchID: snapshotChild, Database: database, Schema: "app", ProvisioningRole: creator}
	verifier, err := scramVerifier(strings.Repeat("p", 43))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = functionsql.ProvisionTx(f.ctx, tx, scope, verifier)
	tx.Rollback(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "grants incomplete") {
		t.Fatal("GRANT warning did not reject an incomplete role inside transaction", err)
	}
	var exists bool
	if err = f.native.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, scope.Role()).Scan(&exists); err != nil || exists {
		t.Fatal("incomplete role survived failed original transaction", err)
	}
	// Only the owner changes grant options. Retrying the same scope succeeds;
	// role identity and permissions never come from a replacement definition.
	exec("GRANT USAGE ON SCHEMA app TO " + quoted + " WITH GRANT OPTION")
	tx, err = conn.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err = functionsql.ProvisionTx(f.ctx, tx, scope, verifier); err != nil {
		t.Fatal("owner-authorized original scope failed", err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	cfg = conn.Config().Copy()
	cfg.User = scope.Role()
	cfg.Password = strings.Repeat("p", 43)
	runtime, err := pgx.ConnectConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(f.ctx)
	if err = functionsql.VerifyRuntimeSession(f.ctx, runtime, scope); err != nil {
		t.Fatal("actual restored restricted SQL login rejected", err)
	}
	if _, err = runtime.Exec(f.ctx, `INSERT INTO app.copy_grant_probe VALUES(1)`); err != nil {
		t.Fatal("intended DML grant missing", err)
	}
	if _, err = runtime.Exec(f.ctx, `CREATE TABLE app.forbidden_owner_ddl(id int)`); err == nil {
		t.Fatal("new Function role acquired owner DDL")
	}
}
