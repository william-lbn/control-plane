package control

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestManagedAuthAdminPolicyMethodsAndCookieIsolation(t *testing.T) {
	p := managedAuthPayload{BranchID: "br_0123456789abcdef", Spec: ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}}}
	raw, _ := json.Marshal(p.Spec)
	if strings.Contains(string(raw), "admin_") {
		t.Fatal("Default policy changed legacy durable hashes")
	}
	for _, id := range []string{"../user", "a b", strings.Repeat("x", 129)} {
		p.Spec.AdminEnabled = true
		p.Spec.AdminBootstrapUserID = id
		if validateManagedAuth(p) == nil {
			t.Fatal("Invalid bootstrap ID accepted")
		}
	}
	p.Spec.AdminBootstrapUserID = "existing-user"
	p.Spec.AdminEnabled = false
	if validateManagedAuth(p) == nil {
		t.Fatal("Bootstrap without admin enabled accepted")
	}
	p.Spec.AdminEnabled = true
	if err := validateManagedAuth(p); err != nil {
		t.Fatal(err)
	}
	for action, method := range managedAuthAdminActions {
		if !managedAuthAllowedAction(action, method) || !managedAuthAllowedAction(action, "OPTIONS") {
			t.Fatal("Required admin method unavailable", action)
		}
		opposite := "GET"
		if method == "GET" {
			opposite = "POST"
		}
		if managedAuthAllowedAction(action, opposite) {
			t.Fatal("Wrong admin method admitted", action)
		}
	}
	branch := p.BranchID
	cookie := "neon_app_" + branch + ".admin_session"
	for _, name := range []string{cookie, "__Secure-" + cookie} {
		if managedAuthRelayCookieAllowed(name, branch, true) || !managedAuthRelayCookieAllowed(name, branch, false, true) {
			t.Fatal("Admin cookie opt-in boundary failed")
		}
	}
	for _, name := range []string{"neon_v2_session", "neon_app_br_ffffffffffffffff.admin_session", cookie + "suffix", "neon_app_" + branch + ".trust_device"} {
		if managedAuthRelayCookieAllowed(name, branch, true, true) {
			t.Fatal("Foreign or trusted-device cookie admitted")
		}
	}
	if managedAuthAllowedAction("admin/private-debug", "POST") {
		t.Fatal("Unapproved admin action exposed")
	}
}

func TestManagedAuthAdminNativeUpgradeGrantReplayAndDisabledBanPreservation(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	config, err := pgx.ParseConfig(dsn)
	if err != nil || config.Database != "control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	ctx := context.Background()
	owner, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	suffix := randomToken(8)
	database := "auth_admin_" + suffix
	if _, err = owner.Exec(ctx, "CREATE DATABASE "+pgQuote(database)); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(dsn)
	target.Path = "/" + database
	conn, err := pgx.Connect(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	p := managedAuthPayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ManagedAuthSpec{Database: database, AllowedOrigins: []string{}}}
	login := managedAuthLogin(p.BranchID)
	defer func() {
		conn.Close(ctx)
		owner.Exec(ctx, "DROP DATABASE "+pgQuote(database)+" WITH (FORCE)")
		owner.Exec(ctx, "DROP ROLE IF EXISTS "+pgQuote(login))
	}()
	verifier, err := scramVerifier(randomToken(32))
	if err != nil {
		t.Fatal(err)
	}
	apply := func() error {
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if e = applyManagedAuthSQLTx(ctx, tx, p, verifier); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if err = apply(); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO neon_auth."user"(id,name,email,"emailVerified") VALUES('existing-user','Owned fixture','admin@example.test',true)`); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-plugin owned installation, preserving the existing user.
	if _, err = conn.Exec(ctx, `ALTER TABLE neon_auth."user" DROP COLUMN role,DROP COLUMN banned,DROP COLUMN "banReason",DROP COLUMN "banExpires"; ALTER TABLE neon_auth.session DROP COLUMN "impersonatedBy"`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	p.Generation = 2
	p.Spec.AdminEnabled = true
	p.Spec.AdminBootstrapUserID = "missing-user"
	if err = apply(); err == nil {
		t.Fatal("Missing bootstrap user published")
	}
	var generation int64
	if err = conn.QueryRow(ctx, `SELECT generation FROM neon_auth.control_installations WHERE branch_id=$1`, p.BranchID).Scan(&generation); err != nil || generation != 1 {
		t.Fatal("Failed grant advanced installation", err)
	}
	p.Spec.AdminBootstrapUserID = "existing-user"
	if err = apply(); err != nil {
		t.Fatal(err)
	}
	var role string
	if err = conn.QueryRow(ctx, `SELECT role FROM neon_auth."user" WHERE id='existing-user'`).Scan(&role); err != nil || role != "admin" {
		t.Fatal("Explicit bootstrap unavailable", err)
	}
	if _, err = conn.Exec(ctx, `UPDATE neon_auth."user" SET role='user',banned=true,"banReason"='owned ban' WHERE id='existing-user'`); err != nil {
		t.Fatal(err)
	}
	if err = apply(); err != nil {
		t.Fatal("Original generation retry failed", err)
	}
	if err = conn.QueryRow(ctx, `SELECT role FROM neon_auth."user" WHERE id='existing-user'`).Scan(&role); err != nil || role != "user" {
		t.Fatal("Retried generation re-granted a revoked role", err)
	}
	p.Generation = 3
	if err = apply(); err == nil {
		t.Fatal("Banned user received a new grant")
	}
	p.Spec.AdminEnabled = false
	p.Spec.AdminBootstrapUserID = ""
	if err = apply(); err != nil {
		t.Fatal(err)
	}
	var banned bool
	var reason string
	if err = conn.QueryRow(ctx, `SELECT role,banned,"banReason" FROM neon_auth."user" WHERE id='existing-user'`).Scan(&role, &banned, &reason); err != nil || role != "user" || !banned || reason != "owned ban" {
		t.Fatal("Disabling admin lost persisted role or ban", err)
	}
	var safe bool
	if err = conn.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolbypassrls AND NOT rolcreatedb AND NOT rolcreaterole FROM pg_roles WHERE rolname=$1`, login).Scan(&safe); err != nil || !safe {
		t.Fatal("Runtime role gained SQL administration", err)
	}
}
