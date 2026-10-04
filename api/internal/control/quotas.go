package control

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

var errOrganizationQuota = errors.New("organization resource quota exceeded")

// Lock the per-organization row so concurrent API instances cannot over-allocate.
// Deleted resources release quota only after their actual lifecycle is complete.
func projectQuota(ctx context.Context, tx pgx.Tx, org string) error {
	var limit, count int
	if err := tx.QueryRow(ctx, `SELECT max_projects FROM organization_quotas WHERE org_id=$1 FOR UPDATE`, org).Scan(&limit); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM projects WHERE org_id=$1 AND deleted_at IS NULL`, org).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return errOrganizationQuota
	}
	return nil
}

func endpointQuota(ctx context.Context, tx pgx.Tx, project string) error {
	var org string
	var limit, count int
	if err := tx.QueryRow(ctx, `SELECT q.org_id,q.max_endpoints FROM organization_quotas q JOIN projects p ON p.org_id=q.org_id
        WHERE p.id=$1 FOR UPDATE OF q`, project).Scan(&org, &limit); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM endpoints e JOIN projects p ON p.id=e.project_id WHERE p.org_id=$1 AND e.deleted_at IS NULL`, org).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return errOrganizationQuota
	}
	return nil
}
