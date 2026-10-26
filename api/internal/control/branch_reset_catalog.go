package control

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
)

// A history entry is an existing immutable identity witness. MarkerBranch is
// resolved from its original inheritance chain, never inferred from SQL names.
type branchResetCatalogHistory struct {
	Branch       string             `json:"branch"`
	MarkerBranch string             `json:"marker_branch"`
	Kind         string             `json:"kind"`
	Name         string             `json:"name"`
	State        string             `json:"state"`
	Scope        catalogNativeScope `json:"scope"`
	Ref          catalogObjectRef   `json:"ref"`
}
type branchResetCatalogIntent struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	State     string            `json:"state"`
	Version   int64             `json:"version"`
	Reference string            `json:"reference"`
	Binding   *catalogObjectRef `json:"binding,omitempty"`
}
type branchResetCatalogChange struct {
	Kind             string `json:"kind"`
	Name             string `json:"name"`
	State            string `json:"state"`
	Version          int64  `json:"version"`
	OID              uint32 `json:"oid"`
	OwnerOID         uint32 `json:"owner_oid,omitempty"`
	Owner            string `json:"owner,omitempty"`
	IdentityID       string `json:"identity_id,omitempty"`
	SourceBinding    string `json:"source_binding,omitempty"`
	CredentialAction string `json:"credential_action,omitempty"`
	CredentialRef    string `json:"credential_ref,omitempty"`
	CredentialUID    string `json:"credential_uid,omitempty"`
	MarkerBranch     string `json:"marker_branch,omitempty"`
}
type branchResetCatalogExclusion struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	OID    uint32 `json:"oid"`
	Reason string `json:"reason"`
}
type branchResetCatalogPlan struct {
	Source         catalogNativeScope            `json:"source"`
	Target         catalogNativeScope            `json:"target"`
	ObservedSHA256 string                        `json:"observed_sha256"`
	Changes        []branchResetCatalogChange    `json:"changes"`
	Unmanaged      []branchResetCatalogExclusion `json:"unmanaged"`
}

var errBranchResetCatalog = errors.New("retained branch reset catalog identity history rejected")

// PostgreSQL/WAL supplies historical objects and their OIDs. Current ordinary
// CREATE/DROP/owner/privilege intent is never replayed. An absent object retires
// only its current API intent. A previously revoked role receives a new private
// credential generation rather than reactivating a historical password.
func planBranchResetCatalog(source catalogNativeScope, snapshot catalogNativeSnapshot, history []branchResetCatalogHistory, current []branchResetCatalogIntent) (branchResetCatalogPlan, error) {
	plan := branchResetCatalogPlan{Source: source, Target: snapshot.Scope, Changes: []branchResetCatalogChange{}, Unmanaged: []branchResetCatalogExclusion{}}
	if !source.valid() || !snapshot.Scope.valid() || source.TenantID != snapshot.Scope.TenantID || source.TimelineID == snapshot.Scope.TimelineID ||
		validateCatalogNativeSnapshot(snapshot) != nil || len(history) > 16384 || len(current) > 2*catalogInventoryLimit {
		return plan, errBranchResetCatalog
	}
	plan.ObservedSHA256 = branchResetNativeDigest(snapshot)
	histories := map[string][]branchResetCatalogHistory{}
	for _, item := range history {
		if item.Scope != source || !dataBranchID.MatchString(item.Branch) || !dataBranchID.MatchString(item.MarkerBranch) || item.Ref.ID == "" || item.Ref.IdentityID == "" || item.Ref.OID == 0 || item.Ref.Version < 1 ||
			item.Kind != "role" && item.Kind != "database" || !validPGIdentifier(item.Name) || item.State != "ready" && item.State != "deleted" {
			return plan, errBranchResetCatalog
		}
		if item.Kind == "role" && (protectedPGRole(item.Name) || item.Ref.CredentialRef == "" || !functionResourceUID.MatchString(item.Ref.CredentialUID)) || item.Kind == "database" && (protectedPGDatabase(item.Name) || item.Ref.OwnerOID == 0) {
			return plan, errBranchResetCatalog
		}
		key := item.Kind + "/" + strconv.FormatUint(uint64(item.Ref.OID), 10)
		histories[key] = append(histories[key], item)
	}
	byName := map[string]branchResetCatalogIntent{}
	activeIdentities := map[string]branchResetCatalogIntent{}
	versions := map[string]int64{}
	current = append([]branchResetCatalogIntent(nil), current...)
	sort.Slice(current, func(i, j int) bool {
		if current[i].Kind != current[j].Kind {
			return current[i].Kind < current[j].Kind
		}
		return current[i].Name < current[j].Name
	})
	for _, item := range current {
		key := item.Kind + "/" + item.Name
		if byName[key].Name != "" || item.Version < 1 || !validPGIdentifier(item.Name) || item.Kind != "role" && item.Kind != "database" || item.State != "ready" && item.State != "deleted" ||
			item.Kind == "role" && protectedPGRole(item.Name) || item.Kind == "database" && protectedPGDatabase(item.Name) {
			return plan, errBranchResetCatalog
		}
		if item.Binding != nil && (item.Binding.OID == 0 || item.Binding.Version != item.Version || item.Binding.IdentityID == "" || item.Binding.ID == "" ||
			item.Kind == "role" && (item.Binding.CredentialRef != item.Reference || !functionResourceUID.MatchString(item.Binding.CredentialUID))) {
			return plan, errBranchResetCatalog
		}
		byName[key], versions[key] = item, item.Version
		if item.State != "ready" {
			continue
		}
		change := branchResetCatalogChange{Kind: item.Kind, Name: item.Name, State: "deleted", Version: item.Version + 1}
		versions[key]++
		if item.Binding != nil {
			change.OID, change.OwnerOID, change.IdentityID, change.SourceBinding = item.Binding.OID, item.Binding.OwnerOID, item.Binding.IdentityID, item.Binding.ID
			change.CredentialRef, change.CredentialUID = item.Binding.CredentialRef, item.Binding.CredentialUID
			identityKey := item.Kind + "/" + item.Binding.IdentityID
			if _, exists := activeIdentities[identityKey]; exists {
				return plan, errBranchResetCatalog
			}
			activeIdentities[identityKey] = item
		}
		plan.Changes = append(plan.Changes, change)
	}
	selectHistory := func(kind, name, marker string, oid uint32) (branchResetCatalogHistory, string) {
		entries := histories[kind+"/"+strconv.FormatUint(uint64(oid), 10)]
		identity := ""
		var chosen branchResetCatalogHistory
		for _, item := range entries {
			// Multiple logical identities at one historical OID are ambiguous even if
			// a reused name happens to match. No credential or ownership is adopted.
			if identity != "" && identity != item.Ref.IdentityID {
				return chosen, "ambiguous_oid_history"
			}
			identity = item.Ref.IdentityID
			if item.State == "ready" && item.Name == name && marker == "neon-control/branch/"+item.MarkerBranch && (chosen.Ref.ID == "" || item.Ref.Version > chosen.Ref.Version) {
				chosen = item
			}
		}
		if chosen.Ref.ID == "" {
			return chosen, "no_original_identity_at_historical_name"
		}
		return chosen, ""
	}
	roleOwners := map[uint32]string{}
	roles := append([]catalogNativeRole(nil), snapshot.Roles...)
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	for _, native := range roles {
		if protectedPGRole(native.Name) {
			continue
		}
		if !validPGIdentifier(native.Name) || native.Flags.Superuser {
			plan.Unmanaged = append(plan.Unmanaged, branchResetCatalogExclusion{"role", native.Name, native.OID, "outside_managed_role_boundary"})
			continue
		}
		prior, reason := selectHistory("role", native.Name, native.Marker, native.OID)
		if reason != "" {
			plan.Unmanaged = append(plan.Unmanaged, branchResetCatalogExclusion{"role", native.Name, native.OID, reason})
			continue
		}
		key := "role/" + native.Name
		change := branchResetCatalogChange{Kind: "role", Name: native.Name, State: "ready", Version: versions[key] + 1, OID: native.OID, IdentityID: prior.Ref.IdentityID, SourceBinding: prior.Ref.ID, CredentialAction: "new_generation", MarkerBranch: prior.MarkerBranch}
		if live, ok := activeIdentities["role/"+prior.Ref.IdentityID]; ok && live.Binding.OID == native.OID {
			change.CredentialAction, change.CredentialRef, change.CredentialUID = "retain_current_generation", live.Binding.CredentialRef, live.Binding.CredentialUID
		}
		plan.Changes = append(plan.Changes, change)
		roleOwners[native.OID] = native.Name
	}
	databases := append([]catalogNativeDatabase(nil), snapshot.Databases...)
	sort.Slice(databases, func(i, j int) bool { return databases[i].Name < databases[j].Name })
	for _, native := range databases {
		if protectedPGDatabase(native.Name) {
			continue
		}
		prior, reason := selectHistory("database", native.Name, native.Marker, native.OID)
		if !validPGIdentifier(native.Name) {
			reason = "outside_managed_database_boundary"
		}
		owner := roleOwners[native.OwnerOID]
		if reason == "" && owner == "" {
			reason = "historical_owner_has_no_original_managed_role"
		}
		if reason != "" {
			plan.Unmanaged = append(plan.Unmanaged, branchResetCatalogExclusion{"database", native.Name, native.OID, reason})
			continue
		}
		key := "database/" + native.Name
		plan.Changes = append(plan.Changes, branchResetCatalogChange{Kind: "database", Name: native.Name, State: "ready", Version: versions[key] + 1, OID: native.OID, OwnerOID: native.OwnerOID, Owner: owner, IdentityID: prior.Ref.IdentityID, SourceBinding: prior.Ref.ID, MarkerBranch: prior.MarkerBranch})
	}
	return plan, nil
}

// Native comments are user content and excluded from snapshot JSON. Bind their
// digests explicitly; a normal JSON hash would miss an intervening comment edit.
func branchResetNativeDigest(snapshot catalogNativeSnapshot) string {
	type roleWitness struct {
		Value        catalogNativeRole
		MarkerSHA256 string
	}
	type databaseWitness struct {
		Value        catalogNativeDatabase
		MarkerSHA256 string
	}
	roles := make([]roleWitness, 0, len(snapshot.Roles))
	databases := make([]databaseWitness, 0, len(snapshot.Databases))
	for _, value := range snapshot.Roles {
		roles = append(roles, roleWitness{value, catalogMarkerDigest(value.Marker)})
	}
	for _, value := range snapshot.Databases {
		databases = append(databases, databaseWitness{value, catalogMarkerDigest(value.Marker)})
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Value.OID < roles[j].Value.OID })
	sort.Slice(databases, func(i, j int) bool { return databases[i].Value.OID < databases[j].Value.OID })
	raw, _ := json.Marshal(struct {
		Scope     catalogNativeScope
		Roles     []roleWitness
		Databases []databaseWitness
	}{snapshot.Scope, roles, databases})
	return catalogMarkerDigest(string(raw))
}
