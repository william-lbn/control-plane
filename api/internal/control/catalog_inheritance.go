package control

import (
	"encoding/json"
	"errors"

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

const catalogPreserveInheritedSQL = "preserve_inherited_sql_v1"

type catalogInheritanceItem struct {
	Kind          string            `json:"kind"`
	Name          string            `json:"name"`
	Owner         string            `json:"owner,omitempty"`
	Version       int64             `json:"version"`
	CredentialRef string            `json:"credential_ref,omitempty"`
	CredentialUID string            `json:"credential_uid,omitempty"`
	Binding       *catalogObjectRef `json:"binding,omitempty"`
	External      bool              `json:"external,omitempty"`
	ExpectedOID   uint32            `json:"expected_oid,omitempty"`
	ObservedOID   uint32            `json:"observed_oid,omitempty"`
}
type catalogInheritanceSource struct {
	Operation          string                   `json:"operation"`
	PayloadSHA256      string                   `json:"payload_sha256"`
	Project            string                   `json:"project"`
	Branch             string                   `json:"branch"`
	Endpoint           string                   `json:"endpoint"`
	SourceBranch       string                   `json:"source_branch"`
	Scope              catalogNativeScope       `json:"scope"`
	SourceScope        catalogNativeScope       `json:"source_scope"`
	ParentLSN          string                   `json:"parent_lsn,omitempty"`
	Historical         bool                     `json:"historical"`
	InitializePlatform bool                     `json:"initialize_platform"`
	PlatformWitness    string                   `json:"platform_witness,omitempty"`
	Items              []catalogInheritanceItem `json:"items"`
}

func catalogInheritanceInitialization(previous, initialized, retiredFailed, readOnly bool) (bool, string, error) {
	if initialized {
		return false, "published_or_original_successful_branch_sql", nil
	}
	if readOnly {
		return false, "", renameFault(409, "catalog_inheritance_initializer_required", "A verified branch Writer is required before a Reader")
	}
	if !previous {
		return true, "first_branch_initializer", nil
	}
	if retiredFailed {
		return true, "retired_original_failed_initializer", nil
	}
	return false, "", renameFault(409, "catalog_inheritance_initializer_required", "Prior Writer initialization is unattested; preserve the branch and its original recovery records")
}

type catalogInheritanceObject struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	OID      uint32 `json:"oid"`
	OwnerOID uint32 `json:"owner_oid,omitempty"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

// Only the two explicitly reserved platform identities enter the first native
// apply. The protected view is later published after SQL identity observation.
// Neither transform changes unknown configuration or topology fields.
func catalogInheritanceConfig(source map[string]any, initial bool, excluded map[string]bool) (map[string]any, error) {
	raw, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	var copy map[string]any
	if err = json.Unmarshal(raw, &copy); err != nil {
		return nil, err
	}
	spec, ok := copy["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("inheritance spec missing")
	}
	cluster, ok := spec["cluster"].(map[string]any)
	if !ok {
		return nil, errors.New("inheritance cluster missing")
	}
	roles, ok := cluster["roles"].([]any)
	if !ok {
		return nil, errors.New("inheritance role inventory missing")
	}
	kept := []any{}
	seen := map[string]bool{}
	for _, entry := range roles {
		role, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("inheritance role rejected")
		}
		name := stringVal(role["name"])
		if !validPGIdentifier(name) || seen[name] {
			return nil, errors.New("inheritance role identity ambiguous")
		}
		seen[name] = true
		platform := name == "cloud_admin" || name == "control_probe"
		if platform && (!functionsql.ValidVerifier(stringVal(role["encrypted_password"])) || excluded["role/"+name]) {
			return nil, errors.New("reserved platform role credential or exclusion rejected")
		}
		if platform || !initial && !excluded["role/"+name] {
			kept = append(kept, role)
		}
	}
	if !seen["cloud_admin"] || !seen["control_probe"] {
		return nil, errors.New("inheritance platform identities missing")
	}
	cluster["roles"] = kept
	databases, ok := cluster["databases"].([]any)
	if !ok {
		return nil, errors.New("inheritance database inventory missing")
	}
	dbs := []any{}
	seen = map[string]bool{}
	for _, entry := range databases {
		database, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("inheritance database rejected")
		}
		name := stringVal(database["name"])
		if !validPGIdentifier(name) || seen[name] {
			return nil, errors.New("inheritance database identity ambiguous")
		}
		seen[name] = true
		if !initial && !excluded["database/"+name] && !excluded["role/"+stringVal(database["owner"])] {
			dbs = append(dbs, database)
		}
	}
	cluster["databases"] = dbs
	if initial {
		spec["delta_operations"] = []any{}
	} else {
		// A retained exclusion is not a DROP intent, even if an older view contains
		// a stale tombstone with the same SQL name.
		deltas, ok := spec["delta_operations"].([]any)
		if spec["delta_operations"] != nil && !ok {
			return nil, errors.New("inheritance deltas rejected")
		}
		next := []any{}
		for _, entry := range deltas {
			delta, ok := entry.(map[string]any)
			if !ok {
				return nil, errors.New("inheritance delta rejected")
			}
			name := stringVal(delta["name"])
			newName := stringVal(delta["new_name"])
			if !excluded["role/"+name] && !excluded["database/"+name] && !excluded["role/"+newName] && !excluded["database/"+newName] {
				next = append(next, delta)
			}
		}
		spec["delta_operations"] = next
		spec["skip_pg_catalog_updates"] = true
	}
	return copy, nil
}

// A copied name is not proof of the original source OID. Known replacements
// remain SQL objects; legacy names are explicitly left without a native head.
func observeCatalogInheritance(source catalogInheritanceSource, snapshot catalogNativeSnapshot) ([]catalogInheritanceObject, error) {
	if !source.Scope.valid() || !source.SourceScope.valid() || source.Scope.TenantID != source.SourceScope.TenantID || source.Scope != snapshot.Scope || validateCatalogNativeSnapshot(snapshot) != nil || len(source.Items) > catalogInventoryLimit {
		return nil, errCatalogInventoryScope
	}
	roles := map[string]catalogNativeRole{}
	databases := map[string]catalogNativeDatabase{}
	for _, v := range snapshot.Roles {
		roles[v.Name] = v
	}
	for _, v := range snapshot.Databases {
		databases[v.Name] = v
	}
	result := []catalogInheritanceObject{}
	excludedRoles := map[string]bool{}
	seen := map[string]bool{}
	for _, item := range source.Items {
		key := item.Kind + "/" + item.Name
		if seen[key] || item.Version < 1 || !validPGIdentifier(item.Name) || item.Kind != "role" && item.Kind != "database" {
			return nil, errors.New("inheritance source intent rejected")
		}
		seen[key] = true
		object := catalogInheritanceObject{Kind: item.Kind, Name: item.Name, Status: "legacy_unattested"}
		marker := ""
		if item.Kind == "role" {
			v := roles[item.Name]
			object.OID = v.OID
			marker = v.Marker
			if v.Flags.Superuser || protectedPGRole(item.Name) {
				return nil, errors.New("customer inheritance contains a system role")
			}
		} else {
			v := databases[item.Name]
			object.OID = v.OID
			object.OwnerOID = v.OwnerOID
			marker = v.Marker
			if protectedPGDatabase(item.Name) {
				return nil, errors.New("customer inheritance contains a system database")
			}
		}
		if object.OID == 0 {
			object.Status, object.Reason = "external_identity", "source_name_missing"
		} else if item.External {
			if item.ObservedOID != 0 && item.ObservedOID != object.OID {
				return nil, errors.New("previous inherited exclusion identity changed")
			}
			object.Status, object.Reason = "external_identity", "retained_source_exclusion"
		} else if marker != "neon-control/branch/"+source.SourceBranch && marker != "neon-control/branch/"+source.Branch {
			object.Status, object.Reason = "external_identity", "source_marker_changed"
		} else if item.Binding != nil {
			if item.Binding.OID == 0 || item.Binding.Version < 1 {
				return nil, errors.New("original inheritance binding rejected")
			}
			object.Status = "inherited_identity"
			if object.OID != item.Binding.OID {
				object.Status, object.Reason = "external_identity", "source_oid_replaced"
			} else if item.Kind == "database" && object.OwnerOID != item.Binding.OwnerOID {
				object.Status, object.Reason = "external_identity", "source_owner_changed"
			}
		}
		if item.Kind == "database" && object.Status != "external_identity" {
			owner, ok := roles[item.Owner]
			if !ok || owner.OID != object.OwnerOID {
				object.Status, object.Reason = "external_identity", "source_owner_name_changed"
			}
		}
		if item.Kind == "role" && object.Status == "external_identity" {
			excludedRoles[item.Name] = true
		}
		result = append(result, object)
	}
	for i := range result {
		if result[i].Kind == "database" {
			item := source.Items[i]
			if excludedRoles[item.Owner] {
				result[i].Status, result[i].Reason = "external_identity", "source_owner_excluded"
			}
		}
	}
	return result, nil
}
