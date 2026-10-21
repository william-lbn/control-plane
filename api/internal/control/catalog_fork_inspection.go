package control

import (
	"context"
	"errors"
)

func (s *server) ensureCatalogForkInspection(ctx context.Context, operation, worker, action string, p createPayload) error {
	if p.CatalogPolicy == catalogPreserveInheritedSQL || action != "create_branch" || p.RestoreSource != "" {
		return nil
	}
	var prior string
	err := s.db.QueryRow(ctx, `SELECT operation_id FROM catalog_fork_inspections WHERE branch_id=$1 AND project_id=$2`, p.BranchID, p.ProjectID).Scan(&prior)
	if err == nil {
		if prior != operation {
			return errors.New("original fork inspection receipt changed")
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	// Do not retrospectively change a creation that already verified its SQL.
	var verified bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operation_steps WHERE operation_id=$1 AND name='sql_probe_via_proxy' AND state='succeeded')`, operation).Scan(&verified)
	if err != nil {
		return err
	}
	if verified {
		return nil
	}
	var parent string
	err = s.db.QueryRow(ctx, `SELECT b.parent_branch_id FROM branches b JOIN projects pr ON pr.id=b.project_id JOIN branches source ON source.id=b.parent_branch_id AND source.project_id=pr.id
 WHERE b.id=$1 AND pr.id=$2 AND pr.tenant_id=$3 AND b.timeline_id=$4 AND source.timeline_id=$5 AND pr.state='ready' AND pr.source='managed' AND pr.deleted_at IS NULL AND b.deleted_at IS NULL AND source.state='ready' AND source.deleted_at IS NULL`, p.BranchID, p.ProjectID, p.TenantID, p.TimelineID, p.ParentTimelineID).Scan(&parent)
	if err != nil {
		return err
	}
	if err = checkDefaultOwnerReservation(ctx, s.db, p.ProjectID, parent); err != nil {
		return err
	}
	rows, err := s.db.Query(ctx, `SELECT bs.name,bs.id,bs.object_oid,bs.credential_ref,bs.credential_uid FROM catalog_identity_baselines bs
 JOIN branch_roles source ON source.branch_id=bs.branch_id AND source.name=bs.name AND source.state='ready' AND source.credential_ref=bs.credential_ref
 JOIN branch_roles child ON child.branch_id=$3 AND child.name=bs.name AND child.state='ready' AND child.credential_ref=bs.credential_ref
 WHERE bs.project_id=$1 AND bs.branch_id=$2 AND bs.kind='role' ORDER BY bs.name LIMIT 501`, p.ProjectID, parent, p.BranchID)
	if err != nil {
		return err
	}
	candidates := []catalogForkExclusion{}
	for rows.Next() {
		var v catalogForkExclusion
		v.SourceBranchID = parent
		if err = rows.Scan(&v.Name, &v.BaselineID, &v.ExpectedOID, &v.CredentialRef, &v.CredentialUID); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(candidates) > 500 {
		return errCatalogInventoryBound
	}
	inherited, err := readCatalogForkExclusions(ctx, s.db, p.ProjectID, parent)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range candidates {
		seen[v.Name] = true
	}
	for _, v := range inherited {
		var same bool
		err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=$1 AND name=$2 AND state='ready' AND credential_ref=$3)`, p.BranchID, v.Name, v.CredentialRef).Scan(&same)
		if err != nil {
			return err
		}
		if !same {
			return errors.New("original excluded source credential intent changed")
		}
		if seen[v.Name] {
			return errors.New("ambiguous original fork source identity")
		}
		v.SourceBranchID = parent
		candidates = append(candidates, v)
		seen[v.Name] = true
	}
	excluded := []catalogForkExclusion{}
	if len(candidates) > 0 {
		writer, err := s.catalogWriter(ctx, p.ProjectID, parent)
		if err != nil {
			return err
		}
		release, err := s.acquireProbeGate(ctx, writer, true)
		if err != nil {
			return err
		}
		defer release()
		conn, err := s.catalogConnection(ctx, catalogPayload{ProjectID: p.ProjectID, BranchID: parent, EndpointID: writer})
		if err != nil {
			return err
		}
		defer conn.Close(context.Background())
		snapshot, err := readCatalogNativeSnapshot(ctx, conn, catalogNativeScope{TenantID: p.TenantID, TimelineID: p.ParentTimelineID})
		if err != nil {
			return err
		}
		critical := map[string]bool{}
		for _, v := range candidates {
			held, e := catalogForkRoleCritical(ctx, s.db, parent, v.Name)
			err = e
			if err != nil {
				return err
			}
			critical[v.Name] = held
		}
		excluded, err = prepareCatalogForkExclusions(candidates, snapshot, critical)
		if err != nil {
			return err
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = recordCatalogForkInspectionTx(ctx, tx, operation, worker, p, parent, "known_identity_check", excluded); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
