package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func catalogInheritancePayloadDigest(p createPayload) string {
	raw, _ := json.Marshal(p)
	return catalogMarkerDigest(string(raw))
}

// The source OID witness is frozen with admission. Today's source catalog is
// never used later to manufacture a witness for an older fork LSN.
func (s *server) captureCatalogInheritanceTx(ctx context.Context, tx pgx.Tx, operation string, p createPayload, sourceBranch string, historical bool) error {
	if p.CatalogPolicy != catalogPreserveInheritedSQL {
		return errors.New("explicit inherited catalog policy required")
	}
	set := catalogInheritanceSource{Operation: operation, PayloadSHA256: catalogInheritancePayloadDigest(p), Project: p.ProjectID, Branch: p.BranchID, Endpoint: p.EndpointID, SourceBranch: sourceBranch, Scope: catalogNativeScope{p.TenantID, p.TimelineID}, ParentLSN: p.ParentLSN, Historical: historical, Items: []catalogInheritanceItem{}}
	if err := tx.QueryRow(ctx, `SELECT p.tenant_id,b.timeline_id FROM projects p JOIN branches b ON b.project_id=p.id WHERE p.id=$1 AND b.id=$2`, p.ProjectID, sourceBranch).Scan(&set.SourceScope.TenantID, &set.SourceScope.TimelineID); err != nil {
		return err
	}
	if !set.Scope.valid() || !set.SourceScope.valid() || set.Scope.TenantID != set.SourceScope.TenantID {
		return errCatalogInventoryScope
	}
	var previousWriter, initialized, retiredFailed bool
	if err := tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND endpoint_type='read_write' AND id<>$3),
 EXISTS(SELECT 1 FROM endpoints e WHERE e.project_id=$1 AND e.branch_id=$2 AND e.endpoint_type='read_write' AND e.id<>$3 AND
 (EXISTS(SELECT 1 FROM compute_catalog_bootstrap_receipts r JOIN compute_catalog_bootstrap_publications pub ON pub.endpoint_id=r.endpoint_id WHERE r.endpoint_id=e.id)
 OR EXISTS(SELECT 1 FROM operations o WHERE o.project_id=$1 AND o.action IN ('create_project','create_branch','create_endpoint') AND o.state='succeeded' AND o.payload->>'endpoint_id'=e.id AND o.payload->>'branch_id'=$2 AND o.payload->>'tenant_id'=$4 AND o.payload->>'timeline_id'=$5
 AND (SELECT count(*) FROM operations other WHERE other.project_id=$1 AND other.action IN ('create_project','create_branch','create_endpoint') AND other.payload->>'endpoint_id'=e.id)=1
 AND EXISTS(SELECT 1 FROM operation_steps st WHERE st.operation_id=o.id AND st.name='sql_probe_via_proxy' AND st.state='succeeded')))),
 NOT EXISTS(SELECT 1 FROM endpoints e WHERE e.project_id=$1 AND e.branch_id=$2 AND e.endpoint_type='read_write' AND e.id<>$3 AND NOT EXISTS(
 SELECT 1 FROM operations cleanup JOIN operations original ON original.id=cleanup.payload->>'failed_create_operation_id'
 WHERE cleanup.id=e.deletion_operation_id AND e.state='deleted' AND e.deleted_at IS NOT NULL AND cleanup.action='cleanup_failed_endpoint' AND cleanup.state='succeeded'
 AND original.project_id=$1 AND original.resource_id=e.id AND original.action='create_endpoint' AND original.state='failed' AND original.lease_owner IS NULL
 AND original.payload->>'branch_id'=$2 AND original.payload->>'tenant_id'=$4 AND original.payload->>'timeline_id'=$5))`, p.ProjectID, p.BranchID, p.EndpointID, p.TenantID, p.TimelineID).Scan(&previousWriter, &initialized, &retiredFailed); err != nil {
		return err
	}
	initialize, basis, err := catalogInheritanceInitialization(previousWriter, initialized, retiredFailed, p.EndpointType == "read_only")
	if err != nil {
		return err
	}
	set.InitializePlatform, set.PlatformWitness = initialize, basis
	if !historical && sourceBranch == p.BranchID {
		var raw []byte
		// A data branch created without Compute retains its original admission
		// snapshot until its first Writer. It must not borrow today's parent head.
		err := tx.QueryRow(ctx, `SELECT s.proof FROM catalog_inheritance_sources s JOIN operations o ON o.id=s.operation_id WHERE s.project_id=$1 AND s.branch_id=$2 AND o.action='create_branch' AND NOT EXISTS(SELECT 1 FROM catalog_inheritance_observations v WHERE v.branch_id=$2) ORDER BY s.created_at LIMIT 1`, p.ProjectID, p.BranchID).Scan(&raw)
		if err == nil {
			var original catalogInheritanceSource
			if json.Unmarshal(raw, &original) != nil || original.Project != set.Project || original.Branch != set.Branch || original.Scope != set.Scope || original.Endpoint != "" {
				return errors.New("original uninitialized branch inheritance snapshot rejected")
			}
			set.SourceBranch, set.SourceScope, set.ParentLSN, set.Items, set.Historical = original.SourceBranch, original.SourceScope, original.ParentLSN, original.Items, original.Historical
			return storeCatalogInheritanceSourceTx(ctx, tx, set)
		}
		if !isNoRows(err) {
			return err
		}
	}
	if !historical {
		rows, err := tx.Query(ctx, `SELECT 'role',name,''::text,version,COALESCE(credential_ref,'') FROM branch_roles WHERE branch_id=$1 AND state='ready' UNION ALL SELECT 'database',name,owner_name,version,''::text FROM branch_databases WHERE branch_id=$1 AND state='ready' AND name NOT IN ('postgres','template0','template1') ORDER BY 1,2 LIMIT 1025`, sourceBranch)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item catalogInheritanceItem
			if err = rows.Scan(&item.Kind, &item.Name, &item.Owner, &item.Version, &item.CredentialRef); err != nil {
				break
			}
			set.Items = append(set.Items, item)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(set.Items) > catalogInventoryLimit {
			return errCatalogInventoryBound
		}
		for i := range set.Items {
			item := &set.Items[i]
			var expected, observed uint32
			err = tx.QueryRow(ctx, `SELECT expected_oid,observed_oid FROM catalog_inheritance_exclusions WHERE project_id=$1 AND branch_id=$2 AND kind=$3 AND name=$4`, p.ProjectID, sourceBranch, item.Kind, item.Name).Scan(&expected, &observed)
			if isNoRows(err) && item.Kind == "role" {
				err = tx.QueryRow(ctx, `SELECT expected_oid,observed_oid FROM catalog_fork_exclusions WHERE project_id=$1 AND branch_id=$2 AND name=$3`, p.ProjectID, sourceBranch, item.Name).Scan(&expected, &observed)
			}
			if err == nil {
				item.External, item.ExpectedOID, item.ObservedOID = true, expected, observed
			} else if !isNoRows(err) {
				return err
			} else {
				ref, e := loadCatalogObjectBinding(ctx, tx, p.ProjectID, sourceBranch, item.Kind, item.Name, set.SourceScope)
				if e == nil {
					item.Binding = &ref
					item.ExpectedOID = ref.OID
				} else {
					var fault *catalogRenameFault
					if !errors.As(e, &fault) || fault.Code != "catalog_initial_observation_required" {
						return e
					}
				}
			}
			if item.Kind == "role" {
				secret, e := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", item.CredentialRef), nil)
				if e != nil || secret["immutable"] != true || stringVal(nested(secret, "metadata", "labels", "neon-control/project-id")) != p.ProjectID {
					return errors.New("source inheritance credential unavailable or outside project")
				}
				item.CredentialUID = stringVal(nested(secret, "metadata", "uid"))
				if !functionResourceUID.MatchString(item.CredentialUID) || item.Binding != nil && (item.Binding.CredentialRef != item.CredentialRef || item.Binding.CredentialUID != item.CredentialUID) {
					return errors.New("source inheritance immutable credential generation changed")
				}
			}
		}
	}
	return storeCatalogInheritanceSourceTx(ctx, tx, set)
}
func storeCatalogInheritanceSourceTx(ctx context.Context, tx pgx.Tx, set catalogInheritanceSource) error {
	if !set.Scope.valid() || !set.SourceScope.valid() || set.Operation == "" || set.PayloadSHA256 == "" || set.Project == "" || set.Branch == "" || len(set.Items) > catalogInventoryLimit {
		return errors.New("invalid original inheritance source")
	}
	raw, err := json.Marshal(set)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO catalog_inheritance_sources(operation_id,project_id,branch_id,proof) VALUES($1,$2,$3,$4)`, set.Operation, set.Project, set.Branch, raw)
	return err
}
