package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type catalogForkReader interface {
	connectionDefaultReader
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
type catalogForkExclusion struct {
	Name, BaselineID, SourceBranchID, CredentialRef, CredentialUID string
	ExpectedOID, ObservedOID                                       uint32
}

func checkCatalogForkExclusion(ctx context.Context, db connectionDefaultReader, project, branch, kind, name string) error {
	var excluded bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_fork_exclusions WHERE project_id=$1 AND branch_id=$2 AND name=$3 AND $4='role') OR EXISTS(SELECT 1 FROM catalog_inheritance_exclusions WHERE project_id=$1 AND branch_id=$2 AND name=$3 AND kind=$4)`, project, branch, name, kind).Scan(&excluded)
	if err != nil {
		return err
	}
	if excluded {
		return renameFault(409, "catalog_inheritance_identity_excluded", "This copied SQL role is preserved outside platform management because its original managed identity was replaced")
	}
	return nil
}
func readCatalogForkExclusions(ctx context.Context, db catalogForkReader, project, branch string) ([]catalogForkExclusion, error) {
	rows, err := db.Query(ctx, `SELECT name,source_baseline_id,source_branch_id,expected_oid,observed_oid,credential_ref,credential_uid FROM catalog_fork_exclusions WHERE project_id=$1 AND branch_id=$2 ORDER BY name LIMIT 501`, project, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []catalogForkExclusion{}
	for rows.Next() {
		var v catalogForkExclusion
		if err = rows.Scan(&v.Name, &v.BaselineID, &v.SourceBranchID, &v.ExpectedOID, &v.ObservedOID, &v.CredentialRef, &v.CredentialUID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 500 {
		return nil, errCatalogInventoryBound
	}
	return out, nil
}
func prepareCatalogForkExclusions(candidates []catalogForkExclusion, snapshot catalogNativeSnapshot, critical map[string]bool) ([]catalogForkExclusion, error) {
	if validateCatalogNativeSnapshot(snapshot) != nil || len(candidates) > 500 {
		return nil, errors.New("bounded original fork catalog observation required")
	}
	native := map[string]catalogNativeRole{}
	for _, v := range snapshot.Roles {
		native[v.Name] = v
	}
	out := []catalogForkExclusion{}
	seen := map[string]bool{}
	for _, v := range candidates {
		if !validPGIdentifier(v.Name) || protectedPGRole(v.Name) || v.ExpectedOID == 0 || v.CredentialRef == "" || !functionResourceUID.MatchString(v.CredentialUID) || seen[v.Name] {
			return nil, errors.New("original fork identity candidate rejected")
		}
		seen[v.Name] = true
		role, exists := native[v.Name]
		if !exists {
			return nil, renameFault(409, "catalog_fork_original_missing", "An original managed source role is missing; preserve the original fork and resolve its identity")
		}
		if v.ObservedOID != 0 {
			if role.OID != v.ObservedOID {
				return nil, renameFault(409, "catalog_fork_exclusion_changed", "The preserved source exclusion identity changed")
			}
		} else if role.OID == v.ExpectedOID {
			continue
		} else {
			v.ObservedOID = role.OID
		}
		if v.ObservedOID == v.ExpectedOID {
			return nil, errors.New("replaced fork identity proof rejected")
		}
		if critical[v.Name] {
			return nil, renameFault(409, "catalog_fork_identity_dependency", "A replaced source role has default or Backend dependencies; resolve those original dependencies before forking")
		}
		out = append(out, v)
	}
	return out, nil
}
func verifyCatalogForkCreationTx(ctx context.Context, tx pgx.Tx, operation, worker string, p createPayload, parent string) error {
	var tenant, timeline, source, parentTimeline, state string
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT pr.tenant_id,b.timeline_id,COALESCE(b.parent_branch_id,''),source.timeline_id,pr.state,pr.deleted_at IS NOT NULL OR b.deleted_at IS NOT NULL OR source.deleted_at IS NOT NULL OR pr.source<>'managed' OR source.state<>'ready' OR org.state<>'active'
 FROM projects pr JOIN organizations org ON org.id=pr.org_id JOIN branches b ON b.project_id=pr.id JOIN branches source ON source.id=b.parent_branch_id AND source.project_id=pr.id WHERE pr.id=$1 AND b.id=$2 FOR UPDATE OF pr,b,source`, p.ProjectID, p.BranchID).Scan(&tenant, &timeline, &source, &parentTimeline, &state, &deleted)
	if err != nil {
		return err
	}
	if deleted || state != "ready" || tenant != p.TenantID || timeline != p.TimelineID || source != parent || parentTimeline != p.ParentTimelineID {
		return errors.New("original live fork parent or scope changed")
	}
	if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1 AND project_id=$2 AND resource_id=$3 AND action='create_branch'`, operation, p.ProjectID, p.BranchID).Scan(&raw)
	var original createPayload
	if err != nil || json.Unmarshal(raw, &original) != nil || original.ProjectID != p.ProjectID || original.BranchID != p.BranchID || original.EndpointID != p.EndpointID || original.TenantID != p.TenantID || original.TimelineID != p.TimelineID || original.ParentTimelineID != p.ParentTimelineID || original.ParentLSN != p.ParentLSN || original.RestoreSource != "" {
		return errors.New("original live fork Operation scope rejected")
	}
	return nil
}
func recordCatalogForkInspectionTx(ctx context.Context, tx pgx.Tx, operation, worker string, p createPayload, parent, basis string, excluded []catalogForkExclusion) error {
	if err := verifyCatalogForkCreationTx(ctx, tx, operation, worker, p, parent); err != nil {
		return err
	}
	var prior string
	err := tx.QueryRow(ctx, `SELECT operation_id FROM catalog_fork_inspections WHERE branch_id=$1`, p.BranchID).Scan(&prior)
	if err == nil {
		if prior != operation {
			return errors.New("original fork inspection Operation changed")
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO catalog_fork_inspections(branch_id,project_id,operation_id,source_branch_id,source_tenant_id,source_timeline_id,timeline_id,basis,excluded_roles) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.BranchID, p.ProjectID, operation, parent, p.TenantID, p.ParentTimelineID, p.TimelineID, basis, len(excluded))
	if err != nil {
		return err
	}
	for _, v := range excluded {
		var known bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_identity_baselines WHERE id=$1 AND project_id=$2 AND kind='role' AND name=$3 AND object_oid=$4 AND credential_ref=$5 AND credential_uid=$6)`, v.BaselineID, p.ProjectID, v.Name, int64(v.ExpectedOID), v.CredentialRef, v.CredentialUID).Scan(&known)
		if err != nil {
			return err
		}
		if !known {
			return errors.New("original fork source baseline identity rejected")
		}
		critical, e := catalogForkRoleCritical(ctx, tx, parent, v.Name)
		if e != nil {
			return e
		}
		if critical {
			return renameFault(409, "catalog_fork_identity_dependency", "The replaced source role acquired a default or Backend dependency")
		}
		var valid bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=$1 AND name=$2 AND state='ready' AND credential_ref=$3)`, p.BranchID, v.Name, v.CredentialRef).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return errors.New("original inherited credential intent changed")
		}
		_, err = tx.Exec(ctx, `INSERT INTO catalog_fork_exclusions(branch_id,project_id,name,source_baseline_id,source_branch_id,expected_oid,observed_oid,credential_ref,credential_uid) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.BranchID, p.ProjectID, v.Name, v.BaselineID, parent, int64(v.ExpectedOID), int64(v.ObservedOID), v.CredentialRef, v.CredentialUID)
		if err != nil {
			return err
		}
	}
	if len(excluded) > 0 {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) SELECT actor_id,'exclude_replaced_inherited_roles','branch',resource_id,request_id,'succeeded' FROM operations WHERE id=$1`, operation)
	}
	return err
}

func catalogForkRoleCritical(ctx context.Context, db connectionDefaultReader, branch, name string) (bool, error) {
	var held bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_databases WHERE branch_id=$1 AND owner_name=$2 AND state<>'deleted')
 OR EXISTS(SELECT 1 FROM endpoints WHERE branch_id=$1 AND role_name=$2 AND deleted_at IS NULL)
 OR EXISTS(SELECT 1 FROM branch_connection_defaults WHERE branch_id=$1 AND mode='managed' AND role_name=$2)
 OR EXISTS(SELECT 1 FROM default_database_owners WHERE branch_id=$1 AND mode='managed' AND owner_name=$2)
 OR EXISTS(SELECT 1 FROM data_api_instances WHERE branch_id=$1 AND (spec->>'role'=$2 OR spec->>'role_name'=$2))
 OR EXISTS(SELECT 1 FROM managed_auth_instances WHERE branch_id=$1 AND (spec->>'role'=$2 OR spec->>'role_name'=$2))
 OR EXISTS(SELECT 1 FROM object_storage_instances WHERE branch_id=$1 AND (spec->>'role'=$2 OR spec->>'role_name'=$2))`, branch, name).Scan(&held)
	return held, err
}
