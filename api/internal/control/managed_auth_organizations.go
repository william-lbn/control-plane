package control

import (
	"context"
	_ "embed"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
)

//go:embed assets/auth-organization-schema.sql
var managedAuthOrganizationSchema string

func managedAuthOrganizationLimits(spec ManagedAuthSpec) (int, int, string) {
	users, members, creator := spec.OrganizationLimit, spec.OrganizationMembershipLimit, spec.OrganizationCreatorRole
	if users == 0 {
		users = 10
	}
	if members == 0 {
		members = 100
	}
	if creator == "" {
		creator = "owner"
	}
	return users, members, creator
}
func validateManagedAuthOrganization(spec ManagedAuthSpec) error {
	if !spec.OrganizationEnabled && (spec.OrganizationLimit != 0 || spec.OrganizationMembershipLimit != 0 || spec.OrganizationCreatorRole != "" || spec.OrganizationInvitationEmail || spec.OrganizationInvitationURL != "") {
		return errors.New("organization options require the organization plugin")
	}
	users, members, creator := managedAuthOrganizationLimits(spec)
	if users < 1 || users > 100 || members < 1 || members > 1000 || (creator != "owner" && creator != "admin") {
		return errors.New("invalid organization limits or creator role")
	}
	if spec.OrganizationInvitationEmail {
		if !spec.RequireEmailVerification || !managedAuthMailConfigured() || len(spec.OrganizationInvitationURL) == 0 || len(spec.OrganizationInvitationURL) > 2048 {
			return errors.New("organization invitations require verified email, operator mail and an application URL")
		}
		target, err := url.Parse(spec.OrganizationInvitationURL)
		if err != nil || target.User != nil || target.RawQuery != "" || target.Fragment != "" || target.Opaque != "" || !validAuthOrigin(target.Scheme+"://"+target.Host) {
			return errors.New("invalid organization invitation URL")
		}
		origin := target.Scheme + "://" + target.Host
		allowed := origin == os.Getenv("NEON_AUTH_PUBLIC_ORIGIN")
		for _, trusted := range spec.AllowedOrigins {
			allowed = allowed || origin == trusted
		}
		if !allowed {
			return errors.New("organization invitation URL requires a trusted application origin")
		}
	} else if spec.OrganizationInvitationURL != "" {
		return errors.New("organization invitation URL requires invitation email")
	}
	return nil
}
func applyManagedAuthOrganizationsTx(ctx context.Context, tx pgx.Tx, p managedAuthPayload, inherited bool) error {
	var existing bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('neon_auth.organization') IS NOT NULL`).Scan(&existing); err != nil {
		return err
	}
	if !p.Spec.OrganizationEnabled && !existing {
		return nil
	}
	if _, err := tx.Exec(ctx, managedAuthOrganizationSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	users, members, _ := managedAuthOrganizationLimits(p.Spec)
	if _, err := tx.Exec(ctx, `INSERT INTO neon_auth.control_organization_policy(singleton,enabled,user_memberships,organization_members,branch_id,generation) VALUES(true,$1,$2,$3,$4,$5) ON CONFLICT(singleton) DO UPDATE SET enabled=EXCLUDED.enabled,user_memberships=EXCLUDED.user_memberships,organization_members=EXCLUDED.organization_members,branch_id=EXCLUDED.branch_id,generation=EXCLUDED.generation`, p.Spec.OrganizationEnabled, users, members, p.BranchID, p.Generation); err != nil {
		return err
	}
	if inherited {
		_, err := tx.Exec(ctx, `UPDATE neon_auth.invitation SET status='canceled' WHERE status='pending'`)
		return err
	}
	return nil
}
func grantManagedAuthOrganizationsTx(ctx context.Context, tx pgx.Tx, login string, spec ManagedAuthSpec) error {
	var existing bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('neon_auth.organization') IS NOT NULL`).Scan(&existing); err != nil {
		return err
	}
	if !existing {
		return nil
	}
	for _, table := range []string{"organization", "member", "invitation", "control_organization_policy"} {
		if _, err := tx.Exec(ctx, "REVOKE ALL ON TABLE neon_auth."+pgQuote(table)+" FROM "+pgQuote(login)); err != nil {
			return err
		}
		if spec.OrganizationEnabled {
			grants := "SELECT,INSERT,UPDATE,DELETE"
			if table == "control_organization_policy" {
				grants = "SELECT"
			}
			if _, err := tx.Exec(ctx, "GRANT "+grants+" ON TABLE neon_auth."+pgQuote(table)+" TO "+pgQuote(login)); err != nil {
				return err
			}
		}
	}
	return nil
}
