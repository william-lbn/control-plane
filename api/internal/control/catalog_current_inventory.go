package control

import (
	"context"
	"encoding/json"
	"strconv"
)

// Current bindings attest OID/name/Owner and provenance. They do not contain a
// historical options or password snapshot; the public report states that limit.
func compareCatalogCurrentIdentity(scope catalogNativeScope, branch, kind, name, owner string, ref catalogObjectRef, snapshot catalogNativeSnapshot) catalogIdentityDifference {
	expected := catalogIdentityExpectation{Scope: scope, Kind: kind, Name: name, OwnerName: owner, OID: ref.OID, OwnerOID: ref.OwnerOID, MarkerSHA256: catalogMarkerDigest("neon-control/branch/" + branch)}
	for _, role := range snapshot.Roles {
		if role.OID == ref.OID && kind == "role" {
			expected.RoleFlags = role.Flags
			// Managed customer roles cannot become SQL superusers.
			expected.RoleFlags.Superuser = false
		}
	}
	for _, database := range snapshot.Databases {
		if database.OID == ref.OID && kind == "database" {
			expected.DatabaseFlags = database.Flags
		}
	}
	return compareCatalogNativeIdentity(expected, snapshot)
}

type catalogCurrentInventory struct {
	Items                 []record
	KnownNames, KnownOIDs map[string]bool
	CurrentOIDs           map[string]uint32
	Status                map[string]string
}

func catalogInventoryKey(kind, name string) string { return kind + ":" + name }

func (s *server) catalogCurrentIdentityReport(ctx context.Context, project, branch string, snapshot catalogNativeSnapshot, excluded map[string]bool) (catalogCurrentInventory, error) {
	out := catalogCurrentInventory{Items: []record{}, KnownNames: map[string]bool{}, KnownOIDs: map[string]bool{}, CurrentOIDs: map[string]uint32{}, Status: map[string]string{}}
	// Historical bindings also prevent another "first" observation. A read must
	// not manufacture a new head or allow a same-name successor to reset history.
	rows, err := s.db.Query(ctx, `SELECT DISTINCT kind,name,object_oid FROM catalog_object_bindings WHERE project_id=$1 AND branch_id=$2 ORDER BY kind,name,object_oid LIMIT 1025`, project, branch)
	if err != nil {
		return out, err
	}
	count := 0
	for rows.Next() {
		var kind, name string
		var oid uint32
		if err = rows.Scan(&kind, &name, &oid); err != nil {
			break
		}
		count++
		out.KnownNames[catalogInventoryKey(kind, name)] = true
		out.KnownOIDs[catalogInventoryKey(kind, strconv.FormatUint(uint64(oid), 10))] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	if count > catalogInventoryLimit {
		return out, errCatalogInventoryBound
	}
	rows, err = s.db.Query(ctx, `WITH intent AS (
 SELECT 'role' AS kind,name,state,version,COALESCE(credential_ref,'') AS reference FROM branch_roles WHERE branch_id=$2
 UNION ALL SELECT 'database',name,state,version,owner_name FROM branch_databases WHERE branch_id=$2
 ) SELECT i.kind,i.name,i.state,i.version,i.reference,h.id,h.identity_id,h.object_oid,h.intent_version,h.credential_ref,h.credential_uid,h.owner_oid,h.tenant_id,h.timeline_id,h.state,h.source_type,h.created_at::text
 FROM intent i JOIN LATERAL (SELECT * FROM catalog_object_bindings c WHERE c.project_id=$1 AND c.branch_id=$2 AND c.kind=i.kind AND c.name=i.name ORDER BY c.intent_version DESC LIMIT 1) h ON true
 WHERE i.state<>'deleted' ORDER BY i.kind,i.name LIMIT 1025`, project, branch)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name, state, reference, headState, source, created string
		var version int64
		var ref catalogObjectRef
		var scope catalogNativeScope
		if err = rows.Scan(&kind, &name, &state, &version, &reference, &ref.ID, &ref.IdentityID, &ref.OID, &ref.Version, &ref.CredentialRef, &ref.CredentialUID, &ref.OwnerOID, &scope.TenantID, &scope.TimelineID, &headState, &source, &created); err != nil {
			return out, err
		}
		if len(out.Items) == catalogInventoryLimit {
			return out, errCatalogInventoryBound
		}
		owner := ""
		if kind == "database" {
			owner = reference
		}
		diff := compareCatalogCurrentIdentity(scope, branch, kind, name, owner, ref, snapshot)
		raw, _ := json.Marshal(diff)
		value := record{}
		_ = json.Unmarshal(raw, &value)
		value["binding_id"], value["identity_id"] = ref.ID, ref.IdentityID
		value["intent_version"], value["current_intent_version"] = strconv.FormatInt(ref.Version, 10), strconv.FormatInt(version, 10)
		value["source_type"], value["captured_at"], value["basis"], value["options"] = source, created, "current_native_identity", "not_recorded"
		value["catalog_status"] = diff.Status
		if state != "ready" || headState != "ready" || version != ref.Version || kind == "role" && reference != ref.CredentialRef {
			value["status"] = "intent_changed"
		}
		if excluded[kind+"/"+name] {
			value["status"], value["management_excluded"] = "management_excluded", true
		}
		key := catalogInventoryKey(kind, name)
		out.CurrentOIDs[key], out.Status[key] = ref.OID, stringVal(value["status"])
		out.Items = append(out.Items, value)
	}
	return out, rows.Err()
}
