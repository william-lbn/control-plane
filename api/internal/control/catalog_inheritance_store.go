package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

func readCatalogInheritanceExclusionNames(ctx context.Context, db catalogForkReader, project, branch string) (map[string]bool, error) {
	rows, err := db.Query(ctx, `SELECT kind,name FROM catalog_inheritance_exclusions WHERE project_id=$1 AND branch_id=$2 ORDER BY kind,name LIMIT 1025`, project, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var kind, name string
		if err = rows.Scan(&kind, &name); err != nil {
			return nil, err
		}
		result[kind+"/"+name] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(result) > catalogInventoryLimit {
		return nil, errCatalogInventoryBound
	}
	return result, nil
}

type catalogInheritanceObservation struct {
	Operation     string                     `json:"operation"`
	SourceSHA256  string                     `json:"source_sha256"`
	Project       string                     `json:"project"`
	Branch        string                     `json:"branch"`
	Endpoint      string                     `json:"endpoint"`
	Scope         catalogNativeScope         `json:"scope"`
	ConfigUID     string                     `json:"config_uid"`
	CredentialUID string                     `json:"credential_uid"`
	RoutesUID     string                     `json:"routes_uid"`
	Objects       []catalogInheritanceObject `json:"objects"`
}

func catalogInheritanceSourceDigest(source catalogInheritanceSource) string {
	raw, _ := json.Marshal(source)
	return catalogMarkerDigest(string(raw))
}
func readCatalogInheritanceSource(ctx context.Context, db connectionDefaultReader, operation string, p createPayload) (catalogInheritanceSource, error) {
	var source catalogInheritanceSource
	var raw []byte
	err := db.QueryRow(ctx, `SELECT proof FROM catalog_inheritance_sources WHERE operation_id=$1 AND project_id=$2 AND branch_id=$3`, operation, p.ProjectID, p.BranchID).Scan(&raw)
	if err != nil {
		return source, err
	}
	if json.Unmarshal(raw, &source) != nil || source.Operation != operation || source.PayloadSHA256 != catalogInheritancePayloadDigest(p) || source.Project != p.ProjectID || source.Branch != p.BranchID || source.Endpoint != p.EndpointID || source.Scope != (catalogNativeScope{p.TenantID, p.TimelineID}) || !source.SourceScope.valid() {
		return source, errors.New("original inheritance source scope rejected")
	}
	return source, nil
}
func readCatalogInheritanceObservation(ctx context.Context, db connectionDefaultReader, operation string, p createPayload) (catalogInheritanceObservation, bool, error) {
	var v catalogInheritanceObservation
	var raw []byte
	var published bool
	err := db.QueryRow(ctx, `SELECT o.proof,EXISTS(SELECT 1 FROM catalog_inheritance_publications pub WHERE pub.endpoint_id=o.endpoint_id) FROM catalog_inheritance_observations o WHERE o.endpoint_id=$1 AND o.operation_id=$2 AND o.project_id=$3 AND o.branch_id=$4`, p.EndpointID, operation, p.ProjectID, p.BranchID).Scan(&raw, &published)
	if err != nil {
		return v, false, err
	}
	if json.Unmarshal(raw, &v) != nil || v.Operation != operation || v.Project != p.ProjectID || v.Branch != p.BranchID || v.Endpoint != p.EndpointID || v.Scope != (catalogNativeScope{p.TenantID, p.TimelineID}) || !functionResourceUID.MatchString(v.ConfigUID) || !functionResourceUID.MatchString(v.CredentialUID) || !functionResourceUID.MatchString(v.RoutesUID) {
		return v, false, errors.New("original inheritance observation identity rejected")
	}
	return v, published, nil
}
func storeCatalogInheritanceObservationTx(ctx context.Context, tx pgx.Tx, operation, worker string, p createPayload, source catalogInheritanceSource, v catalogInheritanceObservation) error {
	if err := assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	original, err := readCatalogInheritanceSource(ctx, tx, operation, p)
	if err != nil {
		return err
	}
	if catalogInheritanceSourceDigest(original) != catalogInheritanceSourceDigest(source) || v.SourceSHA256 != catalogInheritanceSourceDigest(source) || v.Operation != operation || v.Project != p.ProjectID || v.Branch != p.BranchID || v.Endpoint != p.EndpointID || v.Scope != source.Scope || len(v.Objects) != len(source.Items) || !functionResourceUID.MatchString(v.ConfigUID) || !functionResourceUID.MatchString(v.CredentialUID) || !functionResourceUID.MatchString(v.RoutesUID) {
		return errors.New("original inheritance observation proof rejected")
	}
	var eligible bool
	var action, projectState, branchState, endpointState string
	err = tx.QueryRow(ctx, `SELECT pr.source='managed' AND pr.deleted_at IS NULL AND b.deleted_at IS NULL AND e.deleted_at IS NULL AND org.state='active' AND pr.tenant_id=$4 AND b.timeline_id=$5,op.action,pr.state,b.state,e.state FROM projects pr JOIN organizations org ON org.id=pr.org_id JOIN branches b ON b.project_id=pr.id JOIN endpoints e ON e.branch_id=b.id AND e.project_id=pr.id JOIN operations op ON op.id=$6 AND op.project_id=pr.id WHERE pr.id=$1 AND b.id=$2 AND e.id=$3 FOR UPDATE OF pr,b,e`, p.ProjectID, p.BranchID, p.EndpointID, p.TenantID, p.TimelineID, operation).Scan(&eligible, &action, &projectState, &branchState, &endpointState)
	if err != nil {
		return err
	}
	if !eligible || !computeCatalogBootstrapStates(action, projectState, branchState, endpointState, false) {
		return errors.New("inheritance original parent creation changed")
	}
	old, _, err := readCatalogInheritanceObservation(ctx, tx, operation, p)
	if err == nil {
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(v)
		if string(a) != string(b) {
			return errors.New("original inheritance observation cannot be replaced")
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	for i, item := range source.Items {
		object := v.Objects[i]
		if object.Kind != item.Kind || object.Name != item.Name || object.Status != "inherited_identity" && object.Status != "legacy_unattested" && object.Status != "external_identity" {
			return errors.New("original inherited object classification rejected")
		}
		table, column, expectedRef := "branch_databases", "owner_name", item.Owner
		if item.Kind == "role" {
			table, column, expectedRef = "branch_roles", "credential_ref", item.CredentialRef
		}
		var version int64
		var state, reference string
		err = tx.QueryRow(ctx, "SELECT version,state,COALESCE("+column+",'') FROM "+table+" WHERE branch_id=$1 AND name=$2 FOR UPDATE", p.BranchID, item.Name).Scan(&version, &state, &reference)
		expectedVersion := item.Version
		if source.SourceBranch != source.Branch {
			expectedVersion = 1
		}
		if err != nil || version != expectedVersion || state != "ready" || reference != expectedRef {
			return errors.New("copied inheritance intent changed before observation")
		}
		if source.SourceBranch == source.Branch {
			continue
		} // Existing branch heads are not reseeded by another Compute.
		if object.Status == "inherited_identity" {
			if item.Binding == nil || item.Binding.OID != object.OID || item.Kind == "role" && (item.Binding.CredentialRef != item.CredentialRef || item.Binding.CredentialUID != item.CredentialUID) {
				return errors.New("inherited identity has no original source witness")
			}
			sourcePayload := catalogPayload{ProjectID: source.Project, BranchID: source.SourceBranch, NativeIdentity: &catalogMutationIdentity{Scope: source.SourceScope}}
			if err = verifyCatalogObjectRef(ctx, tx, sourcePayload, item.Kind, item.Name, *item.Binding); err != nil {
				return err
			}
			ref := catalogObjectRef{ID: newID("cob_"), IdentityID: newID("coi_"), OID: object.OID, OwnerOID: object.OwnerOID, Version: version, CredentialRef: item.CredentialRef, CredentialUID: item.CredentialUID}
			if err = insertCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, item.Kind, item.Name, "ready", "inherited_identity", item.Binding.ID, source.Scope, ref); err != nil {
				return err
			}
		} else if object.Status == "external_identity" {
			critical, e := catalogInheritanceCritical(ctx, tx, p.BranchID, item.Kind, item.Name)
			if e != nil {
				return e
			}
			sourceCritical, e := catalogInheritanceCritical(ctx, tx, source.SourceBranch, item.Kind, item.Name)
			if e != nil {
				return e
			}
			if critical || sourceCritical {
				return renameFault(409, "catalog_inheritance_dependency_conflict", "A copied default or Backend depends on an external catalog identity; preserve the original fork and resolve that dependency")
			}
			id := ""
			expected := item.ExpectedOID
			if item.Binding != nil {
				id = item.Binding.ID
				expected = item.Binding.OID
			}
			_, err = tx.Exec(ctx, `INSERT INTO catalog_inheritance_exclusions(project_id,branch_id,kind,name,operation_id,source_binding_id,expected_oid,observed_oid,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.ProjectID, p.BranchID, item.Kind, item.Name, operation, id, expected, object.OID, object.Reason)
			if err != nil {
				return err
			}
		}
	}
	raw, _ := json.Marshal(v)
	_, err = tx.Exec(ctx, `INSERT INTO catalog_inheritance_observations(endpoint_id,operation_id,project_id,branch_id,proof) VALUES($1,$2,$3,$4,$5)`, p.EndpointID, operation, p.ProjectID, p.BranchID, raw)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) SELECT actor_id,'observe_catalog_inheritance','branch_catalog',resource_id,request_id,'succeeded' FROM operations WHERE id=$1`, operation)
	return err
}
func catalogInheritanceCritical(ctx context.Context, db connectionDefaultReader, branch, kind, name string) (bool, error) {
	var held bool
	if kind == "role" {
		err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoints WHERE branch_id=$1 AND role_name=$2 AND deleted_at IS NULL) OR EXISTS(SELECT 1 FROM branch_connection_defaults WHERE branch_id=$1 AND mode='managed' AND role_name=$2) OR EXISTS(SELECT 1 FROM default_database_owners WHERE branch_id=$1 AND mode='managed' AND owner_name=$2) OR EXISTS(SELECT 1 FROM data_api_instances WHERE branch_id=$1 AND (spec->>'role'=$2 OR spec->>'role_name'=$2)) OR EXISTS(SELECT 1 FROM managed_auth_instances WHERE branch_id=$1 AND (spec->>'role'=$2 OR spec->>'role_name'=$2)) OR EXISTS(SELECT 1 FROM object_storage_instances WHERE branch_id=$1 AND (spec->>'role'=$2 OR spec->>'role_name'=$2))`, branch, name).Scan(&held)
		return held, err
	}
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoints WHERE branch_id=$1 AND database_name=$2 AND deleted_at IS NULL) OR EXISTS(SELECT 1 FROM branch_connection_defaults WHERE branch_id=$1 AND mode='managed' AND database_name=$2) OR EXISTS(SELECT 1 FROM default_database_owners WHERE branch_id=$1 AND database_name=$2) OR EXISTS(SELECT 1 FROM data_api_instances WHERE branch_id=$1 AND spec->>'database'=$2) OR EXISTS(SELECT 1 FROM managed_auth_instances WHERE branch_id=$1 AND spec->>'database'=$2) OR EXISTS(SELECT 1 FROM object_storage_instances WHERE branch_id=$1 AND spec->>'database'=$2) OR EXISTS(SELECT 1 FROM function_definitions WHERE branch_id=$1 AND database_name=$2 AND state<>'deleted')`, branch, name).Scan(&held)
	return held, err
}
