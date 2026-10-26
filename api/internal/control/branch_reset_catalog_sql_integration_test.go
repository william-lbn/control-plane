package control

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestBranchResetPostgresNativeCatalogCredentialsOIDAndLostCommit(t *testing.T) {
	cfg, err := pgx.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := conn.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	suffix := randomToken(6)
	role, database := "reset_native_role_"+suffix, "reset_native_db_"+suffix
	sourceBranch, targetBranch := "br_0123456789abcdef", "br_"+strings.Repeat("f", 16)
	oldPassword, newPassword := "synthetic-revoked-reset-password", "synthetic-new-reset-generation"
	oldVerifier, err := scramVerifier(oldPassword)
	if err != nil {
		t.Fatal(err)
	}
	newVerifier, err := scramVerifier(newPassword)
	if err != nil {
		t.Fatal(err)
	}
	exec("CREATE ROLE " + pgQuote(role) + " LOGIN PASSWORD " + pgLiteral(oldVerifier))
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS " + pgLiteral("neon-control/branch/"+sourceBranch))
	exec("CREATE DATABASE " + pgQuote(database) + " OWNER " + pgQuote(role))
	exec("COMMENT ON DATABASE " + pgQuote(database) + " IS " + pgLiteral("neon-control/branch/"+sourceBranch))
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = conn.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgQuote(database))
		_, _ = conn.Exec(cleanup, "DROP ROLE IF EXISTS "+pgQuote(role))
	}()
	dataCfg := cfg.Copy()
	dataCfg.Database = database
	data, err := pgx.ConnectConfig(ctx, dataCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = data.Exec(ctx, `CREATE TABLE public.retained_reset_marker(value text);INSERT INTO public.retained_reset_marker VALUES('historical-original-bytes')`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	if _, err = data.Exec(ctx, "ALTER TABLE public.retained_reset_marker OWNER TO "+pgQuote(role)); err != nil {
		t.Fatal(err)
	}
	data.Close(ctx)
	source := catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	target := catalogNativeScope{source.TenantID, strings.Repeat("c", 32)}
	exec(`SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, target.TenantID, target.TimelineID)
	before, err := readCatalogNativeSnapshot(ctx, conn, target)
	if err != nil {
		t.Fatal(err)
	}
	var roleOID, dbOID uint32
	for _, value := range before.Roles {
		if value.Name == role {
			roleOID = value.OID
		}
	}
	for _, value := range before.Databases {
		if value.Name == database {
			dbOID = value.OID
		}
	}
	if roleOID == 0 || dbOID == 0 {
		t.Fatal("original native identities absent")
	}
	roleRef := catalogObjectRef{ID: "original-revoked-role", IdentityID: "native-original-role", OID: roleOID, Version: 1, CredentialRef: "revoked-private-credential", CredentialUID: "11111111-1111-4111-8111-111111111111"}
	dbRef := catalogObjectRef{ID: "original-db", IdentityID: "native-original-db", OID: dbOID, OwnerOID: roleOID, Version: 1}
	history := []branchResetCatalogHistory{{Branch: sourceBranch, MarkerBranch: sourceBranch, Kind: "role", Name: role, State: "ready", Scope: source, Ref: roleRef}, {Branch: sourceBranch, MarkerBranch: sourceBranch, Kind: "database", Name: database, State: "ready", Scope: source, Ref: dbRef}}
	current := []branchResetCatalogIntent{{Kind: "role", Name: role, State: "deleted", Version: 5, Reference: roleRef.CredentialRef}, {Kind: "database", Name: database, State: "deleted", Version: 4, Reference: role}}
	plan, err := planBranchResetCatalog(source, before, history, current)
	if err != nil {
		t.Fatal(err)
	}
	credentials := map[string]branchResetRoleCredential{role: {Ref: "new-private-reset-generation", UID: "22222222-2222-4222-8222-222222222222", Verifier: newVerifier}}
	// Exact scope validation happens before any SQL password or marker mutation.
	wrong := plan
	wrong.Target.TimelineID = strings.Repeat("e", 32)
	if _, err = applyBranchResetCatalogSQL(ctx, conn, targetBranch, wrong, before, credentials); err == nil {
		t.Fatal("foreign timeline accepted native restore")
	}
	result, err := applyBranchResetCatalogSQL(ctx, conn, targetBranch, plan, before, credentials)
	if err != nil {
		t.Fatal("native catalog credential restoration", err)
	}
	replay, err := applyBranchResetCatalogSQL(ctx, conn, targetBranch, plan, before, credentials)
	if err != nil || result != replay {
		t.Fatal("lost SQL commit response recovery did not retain exact result", err)
	}
	after, err := readCatalogNativeSnapshot(ctx, conn, target)
	if err != nil || branchResetNativeDigest(after) != result {
		t.Fatal("native SQL postcondition changed", err)
	}
	var verifier string
	if err = conn.QueryRow(ctx, `SELECT rolpassword FROM pg_authid WHERE oid=$1 AND rolname=$2`, roleOID, role).Scan(&verifier); err != nil || !scramMatches(newPassword, verifier) || scramMatches(oldPassword, verifier) {
		t.Fatal("old credential was resurrected", err)
	}
	runtimeCfg := cfg.Copy()
	runtimeCfg.User, runtimeCfg.Password, runtimeCfg.Database = role, newPassword, database
	runtime, err := pgx.ConnectConfig(ctx, runtimeCfg)
	if err != nil {
		t.Fatal("new credential failed real SCRAM login", err)
	}
	var value string
	if err = runtime.QueryRow(ctx, `SELECT value FROM public.retained_reset_marker`).Scan(&value); err != nil || value != "historical-original-bytes" {
		t.Fatal("original row or owner permissions lost", err)
	}
	runtime.Close(ctx)
	runtimeCfg.Password = oldPassword
	revoked, e := pgx.ConnectConfig(ctx, runtimeCfg)
	if e == nil {
		revoked.Close(ctx)
		t.Fatal("revoked historical password still authenticated")
	}
	var retainedDB, retainedOwner uint32
	if err = conn.QueryRow(ctx, `SELECT oid,datdba FROM pg_database WHERE datname=$1`, database).Scan(&retainedDB, &retainedOwner); err != nil || retainedDB != dbOID || retainedOwner != roleOID {
		t.Fatal("original database or owner OID changed", err)
	}
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS 'unrelated-later-user-comment'")
	if _, err = applyBranchResetCatalogSQL(ctx, conn, targetBranch, plan, before, credentials); err == nil {
		t.Fatal("third-state catalog was overwritten during recovery")
	}
}
