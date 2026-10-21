package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCatalogCurrentInventoryClassifiesIdentityWithoutInventingOptionHistory(t *testing.T) {
	const branch = "br_0123456789abcdef"
	for _, tc := range []struct {
		name, kind, status string
		edit               func(*catalogNativeSnapshot)
	}{
		{"role", "role", "catalog_matches", func(*catalogNativeSnapshot) {}},
		{"options-not-recorded", "role", "catalog_matches", func(s *catalogNativeSnapshot) { s.Roles[0].Flags.Login = false; s.Roles[0].Flags.ConnectionLimit = 7 }},
		{"superuser-conflict", "role", "privilege_conflict", func(s *catalogNativeSnapshot) { s.Roles[0].Flags.Superuser = true }},
		{"renamed", "role", "renamed", func(s *catalogNativeSnapshot) { s.Roles[0].Name = "old_name" }},
		{"collision", "role", "name_collision", func(s *catalogNativeSnapshot) {
			s.Roles[0].Name = "old_name"
			s.Roles = append(s.Roles, catalogNativeRole{OID: 43, Name: "customer"})
		}},
		{"replacement", "role", "replaced", func(s *catalogNativeSnapshot) { s.Roles[0].OID = 43; s.Databases[0].OwnerOID = 43 }},
		{"provenance", "role", "provenance_changed", func(s *catalogNativeSnapshot) { s.Roles[0].Marker = "SQL user comment" }},
		{"database", "database", "catalog_matches", func(*catalogNativeSnapshot) {}},
		{"database-options-not-recorded", "database", "catalog_matches", func(s *catalogNativeSnapshot) { s.Databases[0].Flags.ConnectionLimit = 17 }},
		{"owner-replaced", "database", "owner_changed", func(s *catalogNativeSnapshot) { s.Databases[0].OwnerOID = 42 }},
		{"owner-renamed", "database", "owner_role_renamed", func(s *catalogNativeSnapshot) { s.Roles[0].Name = "new_owner_name" }},
		{"wrong-scope", "role", "scope_conflict", func(s *catalogNativeSnapshot) { s.Scope.TimelineID = strings.Repeat("3", 32) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, snapshot := inventoryFixture()
			scope := snapshot.Scope
			snapshot.Roles[0].Marker, snapshot.Databases[0].Marker = "neon-control/branch/"+branch, "neon-control/branch/"+branch
			ref, name, owner := catalogObjectRef{OID: 41, Version: 2}, "customer", ""
			if tc.kind == "database" {
				ref.OID, ref.OwnerOID, name, owner = 51, 41, "customer_db", "customer"
			}
			tc.edit(&snapshot)
			value := compareCatalogCurrentIdentity(scope, branch, tc.kind, name, owner, ref, snapshot)
			if value.Status != tc.status || value.Credentials != "not_observed" {
				t.Fatalf("current identity classification: %+v", value)
			}
			for _, change := range value.Changes {
				if change == "role_options" && tc.status != "privilege_conflict" || change == "database_options" {
					t.Fatal("invented historical options comparison")
				}
			}
		})
	}
}

func TestCatalogCurrentInventoryMetadataIsReadOnlyAndCannotRebaselineKnownIdentity(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	const project, branch = "prj_0123456789abcdef", "br_0123456789abcdef"
	_, snapshot := inventoryFixture()
	snapshot.Scope = bindingPayload("customer", 2).NativeIdentity.Scope
	snapshot.Roles[0].Marker, snapshot.Databases[0].Marker = "neon-control/branch/"+branch, "neon-control/branch/"+branch
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,'customer','ready','private-credential',2)`, branch)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state,version) VALUES($1,'customer_db','customer','ready',3)`, branch)
	insert := func(kind, name, source string, ref catalogObjectRef) {
		t.Helper()
		tx, e := s.db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = insertCatalogObjectBinding(ctx, tx, project, branch, kind, name, "ready", source, "original_source", snapshot.Scope, ref); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	role := catalogObjectRef{ID: "current_role_v1", IdentityID: "current_role_identity", OID: 41, Version: 1, CredentialRef: "private-old-credential", CredentialUID: "11111111-1111-4111-8111-111111111111"}
	insert("role", "customer", "create_result", role)
	role.ID, role.Version, role.CredentialRef = "current_role_v2", 2, "private-credential"
	insert("role", "customer", "mutation_result", role)
	insert("database", "customer_db", "inherited_identity", catalogObjectRef{ID: "current_database", IdentityID: "current_database_identity", OID: 51, OwnerOID: 41, Version: 3})
	read := func() record {
		t.Helper()
		value, e := s.catalogIdentityReport(ctx, project, branch, snapshot)
		if e != nil {
			t.Fatal(e)
		}
		return value
	}
	value := read()
	if len(value["items"].([]record)) != 0 || len(value["current_bindings"].([]record)) != 2 {
		t.Fatal("current bindings require an unrelated 049 baseline")
	}
	for _, row := range value["current_bindings"].([]record) {
		if row["status"] != "catalog_matches" || row["options"] != "not_recorded" || row["basis"] != "current_native_identity" {
			t.Fatalf("unexpected current view: %+v", row)
		}
	}
	for _, row := range value["native_objects"].([]record) {
		if row["name"] == "customer" || row["name"] == "customer_db" {
			if row["can_initialize"] != false || row["managed"] != true {
				t.Fatal("known identity offered for reinitialization")
			}
		}
	}
	encoded, _ := json.Marshal(value)
	for _, private := range []string{role.CredentialRef, role.CredentialUID, "neon-control/branch/" + branch} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("public current identity leaked private backing")
		}
	}
	// Original-OID and same-name successor attempts both reject before any new
	// baseline or audit can commit, even with a valid current Admin and intent.
	for _, oid := range []uint32{41, 43} {
		input := catalogBaselineInput{Kind: "role", Name: "customer", OID: oid, Acknowledge: true}
		intent, e := readCatalogBaselineIntent(ctx, s.db, project, branch, "role", "customer")
		if e != nil {
			t.Fatal(e)
		}
		proof := catalogBaselineProof{Intent: intent, Expected: catalogIdentityExpectation{Kind: "role", Name: "customer", OID: oid, Scope: intent.Scope}, CredentialUID: role.CredentialUID}
		tx, e := s.db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		_, e = commitCatalogBaselineTx(ctx, tx, project, branch, "binding_admin", "current-request", "current-key", "current-hash", input, proof)
		tx.Rollback(ctx)
		var fault *catalogRenameFault
		if !errors.As(e, &fault) || fault.Code != "catalog_baseline_exists" {
			t.Fatal("known OID/name accepted as first observation", e)
		}
	}
	// Metadata generation changed but no result has committed. Keep its prior
	// original head visible; never silently upgrade it into a new generation.
	exec(`UPDATE branch_roles SET version=3 WHERE branch_id=$1 AND name='customer'`, branch)
	value = read()
	for _, row := range value["current_bindings"].([]record) {
		if row["kind"] == "role" && (row["status"] != "intent_changed" || row["intent_version"] != "2" || row["current_intent_version"] != "3") {
			t.Fatal("uncommitted generation claimed", row)
		}
	}
	snapshot.Roles[0].Name = "retained_original"
	snapshot.Roles = append(snapshot.Roles, catalogNativeRole{OID: 43, Name: "customer", Marker: "neon-control/branch/" + branch})
	value = read()
	for _, row := range value["native_objects"].([]record) {
		if row["name"] == "customer" && (row["managed"] != false || row["can_initialize"] != false) {
			t.Fatal("same-name successor adopted")
		}
	}
	// Both exclusion generations are visible for roles and databases. They also
	// suppress a first-observation offer when no current head exists.
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('current_exclusion_source',$1,'branch',$2,'create_branch','succeeded','binding_admin','current-exclusion','{}')`, project, branch)
	exec(`INSERT INTO catalog_inheritance_sources(operation_id,project_id,branch_id,proof) VALUES('current_exclusion_source',$1,$2,'{}')`, project, branch)
	exec(`INSERT INTO catalog_inheritance_exclusions(project_id,branch_id,kind,name,operation_id,expected_oid,observed_oid,reason) VALUES($1,$2,'database','customer_db','current_exclusion_source',51,51,'owner_identity_changed')`, project, branch)
	value = read()
	for _, row := range value["current_bindings"].([]record) {
		if row["kind"] == "database" && (row["status"] != "management_excluded" || row["management_excluded"] != true) {
			t.Fatal("database exclusion hidden")
		}
	}
	for _, row := range value["native_objects"].([]record) {
		if row["name"] == "customer_db" && (row["managed"] != false || row["management_excluded"] != true || row["can_initialize"] != false) {
			t.Fatal("database exclusion offered for adoption")
		}
	}
	var bindings, baselines, audits int
	if e := s.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_object_bindings),(SELECT count(*) FROM catalog_identity_baselines),(SELECT count(*) FROM audit_events)`).Scan(&bindings, &baselines, &audits); e != nil || bindings != 3 || baselines != 0 || audits != 0 {
		t.Fatal("read changed durable identity history", e)
	}
}
