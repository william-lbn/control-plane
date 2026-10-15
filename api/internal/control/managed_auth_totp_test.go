package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestManagedAuthTOTPDefaultHashAndCookieIsolation(t *testing.T) {
	p := ManagedAuthSpec{Database: "neondb", AllowedOrigins: []string{}}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "two_factor_enabled") {
		t.Fatal("false changed legacy request representation")
	}
	p.TwoFactorEnabled = true
	raw, _ = json.Marshal(p)
	if !strings.Contains(string(raw), `"two_factor_enabled":true`) {
		t.Fatal("enabled TOTP absent")
	}
	branch := "br_0123456789abcdef"
	session := "neon_app_" + branch + ".session_token"
	challenge := "neon_app_" + branch + ".two_factor"
	for _, name := range []string{session, "__Secure-" + session} {
		if !managedAuthRelayCookieAllowed(name, branch, false) {
			t.Error("old session cookie changed")
		}
	}
	for _, name := range []string{challenge, "__Secure-" + challenge} {
		if managedAuthRelayCookieAllowed(name, branch, false) || !managedAuthRelayCookieAllowed(name, branch, true) {
			t.Error("challenge opt-in boundary failed")
		}
	}
	for _, name := range []string{"neon_v2_session", "neon_app_br_other.two_factor", "neon_app_" + branch + ".trust_device", session + "suffix"} {
		if managedAuthRelayCookieAllowed(name, branch, true) {
			t.Error("foreign/Console/trusted-device cookie admitted")
		}
	}
	for action := range managedAuthTwoFactorActions {
		if !managedAuthAllowedAction(action, "POST") || managedAuthAllowedAction(action, "GET") {
			t.Error("factor action method boundary failed")
		}
	}
	if managedAuthAllowedAction("two-factor/send-otp", "POST") || managedAuthAllowedAction("two-factor/verify-otp", "POST") || managedAuthAllowedAction("two-factor/view-backup-codes", "POST") {
		t.Error("unimplemented/secret recovery path exposed")
	}
}

func TestManagedAuthTOTPBranchSecretSurvivesGenerationAndIsolatesBranches(t *testing.T) {
	s := &server{idempotencyKey: []byte(strings.Repeat("private-test", 4))}
	p := managedAuthPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", Generation: 1}
	first, e := s.managedAuthTOTPSecret(p)
	if e != nil || len(first) < 43 {
		t.Fatal("stable secret unavailable")
	}
	p.Generation = 2
	next, e := s.managedAuthTOTPSecret(p)
	if e != nil || first != next {
		t.Fatal("service generation rotated TOTP encryption secret")
	}
	p.BranchID = "br_ffffffffffffffff"
	other, e := s.managedAuthTOTPSecret(p)
	if e != nil || other == first {
		t.Fatal("branch secret isolation failed")
	}
}

func TestManagedAuthTOTPSQLUpgradeAndDowngradeGuard(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("Disposable control_ci required")
	}
	config, e := pgx.ParseConfig(dsn)
	if e != nil || config.Database != "control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	var bytes [8]byte
	if _, e = rand.Read(bytes[:]); e != nil {
		t.Fatal(e)
	}
	suffix := hex.EncodeToString(bytes[:])
	database := "auth_totp_" + suffix
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(database)); e != nil {
		t.Fatal(e)
	}
	target, _ := url.Parse(dsn)
	target.Path = "/" + database
	conn, e := pgx.Connect(ctx, target.String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	p := managedAuthPayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ManagedAuthSpec{Database: database, AllowedOrigins: []string{}}}
	login := managedAuthLogin(p.BranchID)
	defer func() {
		conn.Close(ctx)
		admin.Exec(ctx, "DROP DATABASE "+pgQuote(database)+" WITH (FORCE)")
		admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgQuote(login))
	}()
	verifier, e := scramVerifier(randomToken(32))
	if e != nil {
		t.Fatal(e)
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
	if e = apply(); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, `INSERT INTO neon_auth."user"(id,name,email,"emailVerified") VALUES('totp-user','TOTP','totp@example.test',true)`); e != nil {
		t.Fatal(e)
	}
	p.Generation = 2
	p.Spec.TwoFactorEnabled = true
	if e = apply(); e != nil {
		t.Fatal(e)
	}
	if e = apply(); e != nil {
		t.Fatal("additive retry", e)
	}
	var permitted bool
	if e = conn.QueryRow(ctx, `SELECT has_table_privilege($1,'neon_auth."twoFactor"','SELECT,INSERT,UPDATE,DELETE')`, login).Scan(&permitted); e != nil || !permitted {
		t.Fatal("runtime factor privileges unavailable", e)
	}
	if _, e = conn.Exec(ctx, `UPDATE neon_auth."user" SET "twoFactorEnabled"=true WHERE id='totp-user'`); e != nil {
		t.Fatal(e)
	}
	p.Generation = 3
	p.Spec.TwoFactorEnabled = false
	if e = apply(); e == nil {
		t.Fatal("factor policy downgrade bypassed enrollment")
	}
	p.Spec.TwoFactorEnabled = true
	if e = apply(); e != nil {
		t.Fatal("same branch restart blocked", e)
	}
	p.BranchID = "br_ffffffffffffffff"
	if e = apply(); e == nil {
		t.Fatal("cloned factor ciphertext admitted under unrelated key")
	}
}
