package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestManagedAuthOrganizationPolicyDefaultsTrustAndMethods(t *testing.T) {
	spec := ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}}
	raw, _ := json.Marshal(spec)
	if strings.Contains(string(raw), "organization_") {
		t.Fatal("Default options changed legacy request hash")
	}
	users, members, creator := managedAuthOrganizationLimits(spec)
	if users != 10 || members != 100 || creator != "owner" {
		t.Fatal("Organization defaults changed")
	}
	for _, change := range []ManagedAuthSpec{{OrganizationLimit: 1}, {OrganizationInvitationURL: "https://app.example.test/invite"}, {OrganizationEnabled: true, OrganizationLimit: 101}, {OrganizationEnabled: true, OrganizationLimit: -1}, {OrganizationEnabled: true, OrganizationMembershipLimit: 1001}, {OrganizationEnabled: true, OrganizationCreatorRole: "superuser"}} {
		if validateManagedAuthOrganization(change) == nil {
			t.Fatal("Invalid organization policy accepted")
		}
	}
	t.Setenv("NEON_AUTH_LAB_HTTP", "true")
	t.Setenv("NEON_AUTH_PUBLIC_ORIGIN", "http://192.0.2.1:30788")
	t.Setenv("NEON_AUTH_MAIL_ENABLED", "true")
	t.Setenv("NEON_AUTH_MAIL_SECRET", "operator-mail")
	spec.OrganizationEnabled = true
	spec.OrganizationInvitationEmail = true
	spec.RequireEmailVerification = true
	spec.AllowedOrigins = []string{"https://app.example.test"}
	spec.OrganizationInvitationURL = "https://app.example.test/auth/accept-invitation"
	if err := validateManagedAuthOrganization(spec); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"https://untrusted.example.test/invite", "https://app.example.test/invite?target=elsewhere", "https://app.example.test/invite#fragment", "https://user:secret@app.example.test/invite", "javascript:alert(1)"} {
		candidate := spec
		candidate.OrganizationInvitationURL = target
		if validateManagedAuthOrganization(candidate) == nil {
			t.Fatal("Untrusted invitation URL accepted")
		}
	}
	spec.RequireEmailVerification = false
	if validateManagedAuthOrganization(spec) == nil {
		t.Fatal("Invitation email bypassed verified email policy")
	}
	if len(managedAuthOrganizationActions) != 22 {
		t.Fatal("Unexpected organization protocol surface")
	}
	for action, method := range managedAuthOrganizationActions {
		if !managedAuthAllowedAction(action, method) || !managedAuthAllowedAction(action, "OPTIONS") {
			t.Fatal("Organization method unavailable", action)
		}
		opposite := "GET"
		if method == "GET" {
			opposite = "POST"
		}
		if managedAuthAllowedAction(action, opposite) {
			t.Fatal("Wrong organization method allowed", action)
		}
	}
	for _, action := range []string{"organization/add-member", "organization/create-team", "organization/create-role", "organization/private-debug"} {
		if managedAuthAllowedAction(action, "POST") {
			t.Fatal("Server-only or unconfigured feature exposed", action)
		}
	}
}

func TestManagedAuthOrganizationNativePolicyPrivilegesQuotasAndCopiedInvitations(t *testing.T) {
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
	database := "auth_org_" + suffix
	if _, err = owner.Exec(ctx, "CREATE DATABASE "+pgQuote(database)); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(dsn)
	target.Path = "/" + database
	conn, err := pgx.Connect(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	p := managedAuthPayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ManagedAuthSpec{Database: database, AllowedOrigins: []string{}, OrganizationEnabled: true, OrganizationLimit: 1, OrganizationMembershipLimit: 2}}
	logins := []string{managedAuthLogin(p.BranchID)}
	var app *pgx.Conn
	defer func() {
		if app != nil {
			app.Close(ctx)
		}
		conn.Close(ctx)
		owner.Exec(ctx, "DROP DATABASE "+pgQuote(database)+" WITH (FORCE)")
		for _, login := range logins {
			owner.Exec(ctx, "DROP ROLE IF EXISTS "+pgQuote(login))
		}
	}()
	password := randomToken(32)
	verifier, err := scramVerifier(password)
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
	if _, err = conn.Exec(ctx, `INSERT INTO neon_auth."user"(id,name,email,"emailVerified") VALUES('owner','Owner','owner@example.test',true),('member','Member','member@example.test',true),('other','Other','other@example.test',true)`); err != nil {
		t.Fatal(err)
	}
	runtimeURL, _ := url.Parse(target.String())
	runtimeURL.User = url.UserPassword(logins[0], password)
	app, err = pgx.Connect(ctx, runtimeURL.String())
	if err != nil {
		t.Fatal("Restricted runtime login", err)
	}
	checkCode := func(err error, code string) {
		t.Helper()
		var native *pgconn.PgError
		if !errors.As(err, &native) || native.Code != code {
			t.Fatalf("Expected controlled native code %s, got %v", code, err)
		}
	}
	if _, err = app.Exec(ctx, `UPDATE neon_auth.control_organization_policy SET user_memberships=100`); err == nil {
		t.Fatal("Runtime changed the operator quota")
	} else {
		checkCode(err, "42501")
	}
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO neon_auth.organization(id,name,slug,"createdAt") VALUES('alpha','Alpha','alpha',now());INSERT INTO neon_auth.member(id,"organizationId","userId",role,"createdAt") VALUES('owner-membership','alpha','owner','owner',now())`, pgx.QueryExecModeSimpleProtocol); err != nil {
		tx.Rollback(ctx)
		t.Fatal("Restricted role could not invoke owned trigger", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO neon_auth.organization(id,name,slug,"createdAt") VALUES('rejected','Rejected','rejected',now());INSERT INTO neon_auth.member(id,"organizationId","userId",role,"createdAt") VALUES('over-limit','rejected','owner','owner',now())`, pgx.QueryExecModeSimpleProtocol); err == nil {
		tx.Rollback(ctx)
		t.Fatal("User membership quota exceeded")
	} else {
		checkCode(err, "P4001")
		tx.Rollback(ctx)
	}
	var rows int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.organization WHERE id='rejected'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("Failed transaction left an orphan", err)
	}
	if _, err = app.Exec(ctx, `INSERT INTO neon_auth.member(id,"organizationId","userId",role,"createdAt") VALUES('member-membership','alpha','member','member',now())`); err != nil {
		t.Fatal(err)
	}
	_, err = app.Exec(ctx, `INSERT INTO neon_auth.member(id,"organizationId","userId",role,"createdAt") VALUES('other-membership','alpha','other','member',now())`)
	checkCode(err, "P4002")
	_, err = app.Exec(ctx, `DELETE FROM neon_auth.member WHERE id='owner-membership'`)
	checkCode(err, "P4003")
	_, err = app.Exec(ctx, `UPDATE neon_auth.member SET role='superuser' WHERE id='member-membership'`)
	checkCode(err, "P4004")
	if _, err = app.Exec(ctx, `INSERT INTO neon_auth.invitation(id,"organizationId",email,role,status,"expiresAt","inviterId") VALUES('pending','alpha','other@example.test','member','pending',now()+interval '1 day','owner')`); err != nil {
		t.Fatal(err)
	}
	app.Close(ctx)
	app = nil
	p.Generation = 2
	if err = apply(); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = conn.QueryRow(ctx, `SELECT status FROM neon_auth.invitation WHERE id='pending'`).Scan(&status); err != nil || status != "pending" {
		t.Fatal("Same-branch re-enable canceled a current invitation", err)
	}
	p.Generation = 3
	p.Spec.OrganizationEnabled = false
	p.Spec.OrganizationLimit = 0
	p.Spec.OrganizationMembershipLimit = 0
	if err = apply(); err != nil {
		t.Fatal(err)
	}
	var enabled, write bool
	if err = conn.QueryRow(ctx, `SELECT enabled,has_table_privilege($1,'neon_auth.member','INSERT') FROM neon_auth.control_organization_policy`, logins[0]).Scan(&enabled, &write); err != nil || enabled || write {
		t.Fatal("Disabled policy retained runtime organization writes", err)
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.member`).Scan(&rows); err != nil || rows != 2 {
		t.Fatal("Disabling organizations deleted membership data", err)
	}
	// A new branch installation is simulated against copied native state. The
	// actual physical copy/source preservation is checked by the live suite.
	p.BranchID = "br_" + randomToken(8)
	p.Generation = 1
	p.Spec.OrganizationEnabled = true
	logins = append(logins, managedAuthLogin(p.BranchID))
	if err = apply(); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT status FROM neon_auth.invitation WHERE id='pending'`).Scan(&status); err != nil || status != "canceled" {
		t.Fatal("A copied pending invitation remained usable", err)
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.member`).Scan(&rows); err != nil || rows != 2 {
		t.Fatal("Child installation lost copied role data", err)
	}
	if err = apply(); err != nil {
		t.Fatal("Same generation retry", err)
	}
	runtimeURL.User = url.UserPassword(logins[1], password)
	app, err = pgx.Connect(ctx, runtimeURL.String())
	if err != nil {
		t.Fatal(err)
	}
	tx, err = app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('neon_control.auth_deleting_organization','alpha',true);DELETE FROM neon_auth.organization WHERE id='alpha'`, pgx.QueryExecModeSimpleProtocol); err != nil {
		tx.Rollback(ctx)
		t.Fatal("Restricted runtime could not complete an authorized organization deletion", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.member`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("Organization deletion left members", err)
	}
}
