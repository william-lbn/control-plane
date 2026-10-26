package control

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

type branchResetRoleCredential struct {
	Ref      string
	UID      string
	Verifier string `json:"-"`
}

func branchResetCatalogPostSnapshot(plan branchResetCatalogPlan, before catalogNativeSnapshot, branch string) (catalogNativeSnapshot, error) {
	after := before
	after.Roles = append([]catalogNativeRole(nil), before.Roles...)
	after.Databases = append([]catalogNativeDatabase(nil), before.Databases...)
	if !dataBranchID.MatchString(branch) || plan.Target != before.Scope || plan.ObservedSHA256 != branchResetNativeDigest(before) {
		return after, errBranchResetCatalog
	}
	seen := map[string]bool{}
	for _, change := range plan.Changes {
		if change.State != "ready" {
			continue
		}
		key := change.Kind + "/" + change.Name
		if seen[key] || change.OID == 0 || !validPGIdentifier(change.Name) || !dataBranchID.MatchString(change.MarkerBranch) || change.IdentityID == "" || change.SourceBinding == "" {
			return after, errBranchResetCatalog
		}
		seen[key] = true
		found := false
		switch change.Kind {
		case "role":
			if protectedPGRole(change.Name) || change.CredentialAction != "new_generation" && change.CredentialAction != "retain_current_generation" {
				return after, errBranchResetCatalog
			}
			for i, native := range after.Roles {
				if native.OID != change.OID {
					continue
				}
				if native.Name != change.Name || native.Flags.Superuser || native.Marker != "neon-control/branch/"+change.MarkerBranch {
					return after, errBranchResetCatalog
				}
				after.Roles[i].Marker = "neon-control/branch/" + branch
				found = true
				break
			}
		case "database":
			if protectedPGDatabase(change.Name) || change.OwnerOID == 0 {
				return after, errBranchResetCatalog
			}
			for i, native := range after.Databases {
				if native.OID != change.OID {
					continue
				}
				if native.Name != change.Name || native.OwnerOID != change.OwnerOID || native.Marker != "neon-control/branch/"+change.MarkerBranch {
					return after, errBranchResetCatalog
				}
				after.Databases[i].Marker = "neon-control/branch/" + branch
				found = true
				break
			}
		default:
			return after, errBranchResetCatalog
		}
		if !found {
			return after, errBranchResetCatalog
		}
	}
	return after, nil
}

// This never creates/drops/renames ordinary objects or changes grants/owners.
// Both initial execution and lost-commit recovery require the complete fixed
// historical catalog (including comment hashes) before applying the exact same
// acknowledged password generations and branch ownership markers.
func applyBranchResetCatalogSQL(ctx context.Context, conn *pgx.Conn, branch string, plan branchResetCatalogPlan, before catalogNativeSnapshot, credentials map[string]branchResetRoleCredential) (string, error) {
	after, err := branchResetCatalogPostSnapshot(plan, before, branch)
	if err != nil {
		return "", err
	}
	expectedPost := branchResetNativeDigest(after)
	expectedCredentials := 0
	for _, change := range plan.Changes {
		if change.State != "ready" || change.Kind != "role" {
			continue
		}
		expectedCredentials++
		value, ok := credentials[change.Name]
		if !ok || value.Ref == "" || !functionResourceUID.MatchString(value.UID) || !functionsql.ValidVerifier(value.Verifier) ||
			change.CredentialAction == "retain_current_generation" && (value.Ref != change.CredentialRef || value.UID != change.CredentialUID) {
			return "", errBranchResetCatalog
		}
	}
	if len(credentials) != expectedCredentials {
		return "", errBranchResetCatalog
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout='20s';SET LOCAL lock_timeout='2s';SET LOCAL standard_conforming_strings=on;SET LOCAL TIME ZONE 'UTC'`, pgx.QueryExecModeSimpleProtocol); err != nil {
		return "", err
	}
	current, err := readCatalogNativeSnapshotTx(ctx, tx, plan.Target)
	if err != nil {
		return "", err
	}
	digest := branchResetNativeDigest(current)
	if digest != plan.ObservedSHA256 && digest != expectedPost {
		return "", errBranchResetCatalog
	}
	for _, change := range plan.Changes {
		if change.State != "ready" {
			continue
		}
		if change.Kind == "role" {
			credential := credentials[change.Name]
			if _, err = tx.Exec(ctx, "ALTER ROLE "+pgQuote(change.Name)+" PASSWORD "+pgLiteral(credential.Verifier)); err != nil {
				return "", err
			}
			if _, err = tx.Exec(ctx, "COMMENT ON ROLE "+pgQuote(change.Name)+" IS "+pgLiteral("neon-control/branch/"+branch)); err != nil {
				return "", err
			}
		} else {
			if _, err = tx.Exec(ctx, "COMMENT ON DATABASE "+pgQuote(change.Name)+" IS "+pgLiteral("neon-control/branch/"+branch)); err != nil {
				return "", err
			}
		}
	}
	current, err = readCatalogNativeSnapshotTx(ctx, tx, plan.Target)
	if err != nil || branchResetNativeDigest(current) != expectedPost {
		return "", errBranchResetCatalog
	}
	for _, change := range plan.Changes {
		if change.State != "ready" || change.Kind != "role" {
			continue
		}
		var verifier string
		if err = tx.QueryRow(ctx, `SELECT rolpassword FROM pg_authid WHERE oid=$1 AND rolname=$2 AND NOT rolsuper`, change.OID, change.Name).Scan(&verifier); err != nil || verifier != credentials[change.Name].Verifier {
			return "", errBranchResetCatalog
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return expectedPost, nil
}
