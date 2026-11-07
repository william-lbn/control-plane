package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func resetCatalogFixture() (catalogNativeScope, catalogNativeSnapshot, []branchResetCatalogHistory, []branchResetCatalogIntent) {
	source := catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	target := catalogNativeScope{source.TenantID, strings.Repeat("c", 32)}
	branch := "br_0123456789abcdef"
	role := catalogObjectRef{ID: "original-role-binding", IdentityID: "original-role-identity", OID: 42, Version: 1, CredentialRef: "old-revoked-role-credential", CredentialUID: "11111111-1111-4111-8111-111111111111"}
	database := catalogObjectRef{ID: "original-database-binding", IdentityID: "original-database-identity", OID: 84, OwnerOID: 42, Version: 1}
	history := []branchResetCatalogHistory{{Branch: branch, MarkerBranch: branch, Kind: "role", Name: "original_role", State: "ready", Scope: source, Ref: role}, {Branch: branch, MarkerBranch: branch, Kind: "database", Name: "original_database", State: "ready", Scope: source, Ref: database}}
	currentRole := role
	currentRole.ID = "current-role-binding"
	currentRole.Version = 9
	currentRole.CredentialRef = "current-kept-credential"
	currentRole.CredentialUID = "22222222-2222-4222-8222-222222222222"
	currentDatabase := database
	currentDatabase.ID = "current-database-binding"
	currentDatabase.Version = 3
	after := catalogObjectRef{ID: "after-point-role-binding", IdentityID: "after-point-role-identity", OID: 99, Version: 2, CredentialRef: "after-point-credential", CredentialUID: "33333333-3333-4333-8333-333333333333"}
	current := []branchResetCatalogIntent{{Kind: "role", Name: "renamed_role", State: "ready", Version: 9, Reference: currentRole.CredentialRef, Binding: &currentRole}, {Kind: "database", Name: "renamed_database", State: "ready", Version: 3, Reference: "renamed_role", Binding: &currentDatabase}, {Kind: "role", Name: "created_after_point", State: "ready", Version: 2, Reference: after.CredentialRef, Binding: &after}}
	snapshot := catalogNativeSnapshot{Scope: target, Roles: []catalogNativeRole{{OID: 10, Name: "cloud_admin", Flags: catalogRoleFlags{Login: true}}, {OID: 42, Name: "original_role", Marker: "neon-control/branch/" + branch, Flags: catalogRoleFlags{Login: true}}}, Databases: []catalogNativeDatabase{{OID: 1, OwnerOID: 10, Name: "template1"}, {OID: 84, OwnerOID: 42, Name: "original_database", Marker: "neon-control/branch/" + branch}}}
	return source, snapshot, history, current
}
func TestBranchResetCatalogHistoricalOIDNamesAndCurrentCredential(t *testing.T) {
	source, snapshot, history, current := resetCatalogFixture()
	before, _ := json.Marshal([]any{snapshot, history, current})
	plan, err := planBranchResetCatalog(source, snapshot, history, current)
	if err != nil || len(plan.Changes) != 5 || len(plan.Unmanaged) != 0 {
		t.Fatal("historical catalog planning", len(plan.Changes), plan.Unmanaged, err)
	}
	roles, databases, retiredAfter := 0, 0, false
	for _, change := range plan.Changes {
		if change.State == "ready" && change.Kind == "role" {
			roles++
			if change.Name != "original_role" || change.OID != 42 || change.CredentialAction != "retain_current_generation" || change.CredentialRef != "current-kept-credential" || change.CredentialUID != "22222222-2222-4222-8222-222222222222" {
				t.Fatal("historical role replayed revoked or name-based credentials", change)
			}
		}
		if change.State == "ready" && change.Kind == "database" {
			databases++
			if change.Name != "original_database" || change.OID != 84 || change.OwnerOID != 42 || change.Owner != "original_role" {
				t.Fatal("database original OID or historical owner lost", change)
			}
		}
		if change.Name == "created_after_point" {
			if change.State != "deleted" || change.Version != 3 {
				t.Fatal("post-point role was recreated")
			}
			retiredAfter = true
		}
	}
	if roles != 1 || databases != 1 || !retiredAfter {
		t.Fatal("historical identities not restored")
	}
	after, _ := json.Marshal([]any{snapshot, history, current})
	if string(before) != string(after) {
		t.Fatal("planner changed retained source snapshot or original metadata")
	}
}
func TestBranchResetCatalogRevokedRolesGetFreshGenerationAndAmbiguousOIDsStayUnmanaged(t *testing.T) {
	source, snapshot, history, _ := resetCatalogFixture()
	current := []branchResetCatalogIntent{{Kind: "role", Name: "original_role", State: "deleted", Version: 7, Reference: history[0].Ref.CredentialRef}}
	plan, err := planBranchResetCatalog(source, snapshot, history, current)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range plan.Changes {
		if change.Kind == "role" && change.State == "ready" {
			found = true
			if change.CredentialAction != "new_generation" || change.CredentialRef != "" || change.CredentialUID != "" || change.Version != 8 {
				t.Fatal("revoked historical credential generation was reactivated", change)
			}
		}
	}
	if !found {
		t.Fatal("known historical role was not planned")
	}
	collision := history[0]
	collision.Ref.ID = "reused-oid-binding"
	collision.Ref.IdentityID = "other-logical-role"
	collision.Ref.Version = 2
	history = append(history, collision)
	plan, err = planBranchResetCatalog(source, snapshot, history, current)
	if err != nil || len(plan.Unmanaged) != 2 {
		t.Fatal("ambiguous identity was hidden or adopted", plan.Unmanaged, err)
	}
	for _, change := range plan.Changes {
		if change.State == "ready" {
			t.Fatal("ambiguous native role or dependent owner was adopted", change)
		}
	}
	// A matching SQL name without any original OID witness is observation only.
	source, snapshot, _, current = resetCatalogFixture()
	plan, err = planBranchResetCatalog(source, snapshot, nil, current)
	if err != nil || len(plan.Unmanaged) != 2 {
		t.Fatal("name-only historical identity accepted", err)
	}
}
func TestBranchResetCatalogRejectsChangedScopeAndInFlightIntent(t *testing.T) {
	for _, scenario := range []string{"cross_tenant", "same_timeline", "foreign_history", "missing_credential_uid", "inflight", "changed_binding_version", "duplicate_current", "duplicate_active_identity", "unobserved_owner", "duplicate_native_name", "empty_identity"} {
		t.Run(scenario, func(t *testing.T) {
			source, snapshot, history, current := resetCatalogFixture()
			switch scenario {
			case "cross_tenant":
				snapshot.Scope.TenantID = strings.Repeat("d", 32)
			case "same_timeline":
				snapshot.Scope = source
			case "foreign_history":
				history[0].Scope.TimelineID = strings.Repeat("d", 32)
			case "missing_credential_uid":
				history[0].Ref.CredentialUID = ""
			case "inflight":
				current[0].State = "updating"
			case "changed_binding_version":
				current[0].Binding.Version++
			case "duplicate_current":
				current = append(current, current[0])
			case "duplicate_active_identity":
				other := current[0]
				other.Name = "other_active_name"
				current = append(current, other)
			case "unobserved_owner":
				snapshot.Databases[1].OwnerOID = 777
			case "duplicate_native_name":
				snapshot.Roles = append(snapshot.Roles, snapshot.Roles[1])
			case "empty_identity":
				history[0].Ref.IdentityID = ""
			}
			if _, err := planBranchResetCatalog(source, snapshot, history, current); err == nil {
				t.Fatal("changed original history was accepted")
			}
		})
	}
}

func TestBranchResetCatalogSnapshotDigestAndExactSQLPostcondition(t *testing.T) {
	source, snapshot, history, current := resetCatalogFixture()
	plan, err := planBranchResetCatalog(source, snapshot, history, current)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ObservedSHA256 != branchResetNativeDigest(snapshot) {
		t.Fatal("exact native observation was not bound")
	}
	reordered := snapshot
	reordered.Roles = append([]catalogNativeRole(nil), snapshot.Roles...)
	reordered.Roles[0], reordered.Roles[1] = reordered.Roles[1], reordered.Roles[0]
	if branchResetNativeDigest(reordered) != plan.ObservedSHA256 {
		t.Fatal("inventory order changed exact observation")
	}
	changed := snapshot
	changed.Roles = append([]catalogNativeRole(nil), snapshot.Roles...)
	changed.Roles[1].Marker = "unrelated user comment"
	if branchResetNativeDigest(changed) == plan.ObservedSHA256 {
		t.Fatal("raw comment mutation was excluded from observation hash")
	}
	branch := "br_" + strings.Repeat("f", 16)
	after, err := branchResetCatalogPostSnapshot(plan, snapshot, branch)
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range after.Roles {
		if native.OID == 42 && native.Marker != "neon-control/branch/"+branch {
			t.Fatal("managed historical role marker not rebound")
		}
	}
	if branchResetNativeDigest(after) == plan.ObservedSHA256 {
		t.Fatal("source branch marker change was not reflected in postcondition")
	}
	if _, err = branchResetCatalogPostSnapshot(plan, changed, branch); err == nil {
		t.Fatal("an edited original native snapshot was adopted")
	}
	forged := plan
	forged.Changes = append([]branchResetCatalogChange(nil), plan.Changes...)
	for i, change := range forged.Changes {
		if change.Kind == "role" && change.State == "ready" {
			forged.Changes[i].OID = 10
		}
	}
	if _, err = branchResetCatalogPostSnapshot(forged, snapshot, branch); err == nil {
		t.Fatal("different or protected native identity was adopted")
	}
}
