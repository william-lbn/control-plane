package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogInheritanceInitializationRequiresOriginalWriterWitness(t *testing.T) {
	for _, tc := range []struct{ previous, initialized, failed, reader, want, allowed bool }{
		{false, false, false, false, true, true}, {true, true, false, false, false, true}, {true, true, true, false, false, true},
		{true, false, true, false, true, true}, {true, false, false, false, false, false},
		{true, true, false, true, false, true}, {true, false, true, true, false, false}, {false, false, false, true, false, false},
	} {
		initialize, basis, err := catalogInheritanceInitialization(tc.previous, tc.initialized, tc.failed, tc.reader)
		if (err == nil) != tc.allowed || err == nil && (initialize != tc.want || basis == "") {
			t.Fatalf("wrong initialization decision: %+v", tc)
		}
	}
}

func TestCatalogInheritancePlatformOnlyFirstApplyAndProtectedProjection(t *testing.T) {
	admin, e := scramVerifier("test-inheritance-platform-password")
	if e != nil {
		t.Fatal(e)
	}
	probe, e := scramVerifier("test-inheritance-probe-password")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := computeConfig("prj_0123456789abcdef", strings.Repeat("a", 32), strings.Repeat("b", 32), admin, probe, "read_write")
	if e != nil {
		t.Fatal(e)
	}
	var config map[string]any
	if json.Unmarshal([]byte(raw), &config) != nil {
		t.Fatal("config")
	}
	config["future_control"] = map[string]any{"retain": "original"}
	spec := config["spec"].(map[string]any)
	spec["future_topology"] = []any{"original"}
	cluster := spec["cluster"].(map[string]any)
	cluster["roles"] = append(cluster["roles"].([]any), map[string]any{"name": "retained_customer", "encrypted_password": admin, "options": nil}, map[string]any{"name": "replaced_customer", "encrypted_password": probe, "options": nil})
	cluster["databases"] = []any{map[string]any{"name": "retained_database", "owner": "retained_customer", "options": nil}, map[string]any{"name": "replaced_database", "owner": "replaced_customer", "options": nil}}
	spec["delta_operations"] = []any{map[string]any{"action": "delete_role", "name": "replaced_customer", "new_name": nil}, map[string]any{"action": "delete_db", "name": "unrelated_tombstone", "new_name": nil}}
	before, _ := json.Marshal(config)
	first, e := catalogInheritanceConfig(config, true, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(nested(first, "spec", "cluster", "roles").([]any)) != 2 || len(nested(first, "spec", "cluster", "databases").([]any)) != 0 || len(nested(first, "spec", "delta_operations").([]any)) != 0 {
		t.Fatal("first native apply contains customer catalog")
	}
	excluded := map[string]bool{"role/replaced_customer": true, "database/replaced_database": true}
	protected, e := catalogInheritanceConfig(config, false, excluded)
	if e != nil {
		t.Fatal(e)
	}
	if nested(protected, "spec", "skip_pg_catalog_updates") != true || len(nested(protected, "spec", "cluster", "roles").([]any)) != 3 || len(nested(protected, "spec", "cluster", "databases").([]any)) != 1 || len(nested(protected, "spec", "delta_operations").([]any)) != 1 {
		t.Fatal("protected publication adopted an excluded catalog entry")
	}
	for _, result := range []map[string]any{first, protected} {
		if nested(result, "future_control", "retain") != "original" || nested(result, "spec", "future_topology").([]any)[0] != "original" {
			t.Fatal("unrelated identity/configuration changed")
		}
	}
	after, _ := json.Marshal(config)
	if string(before) != string(after) {
		t.Fatal("source configuration was mutated")
	}
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { delete(v, "spec") },
		func(v map[string]any) { v["spec"].(map[string]any)["cluster"] = nil },
		func(v map[string]any) { nested(v, "spec", "cluster").(map[string]any)["roles"] = []any{} },
		func(v map[string]any) {
			c := nested(v, "spec", "cluster").(map[string]any)
			c["roles"] = append(c["roles"].([]any), c["roles"].([]any)[0])
		},
		func(v map[string]any) { nested(v, "spec", "cluster").(map[string]any)["databases"] = []any{"invalid"} },
	} {
		var invalid map[string]any
		json.Unmarshal(before, &invalid)
		mutate(invalid)
		if _, e = catalogInheritanceConfig(invalid, true, nil); e == nil {
			t.Fatal("invalid first apply accepted")
		}
	}
	if _, e = catalogInheritanceConfig(config, false, map[string]bool{"role/control_probe": true}); e == nil {
		t.Fatal("platform probe excluded")
	}
}

func TestCatalogInheritanceOriginalOIDOwnerAndLegacyBoundaries(t *testing.T) {
	scope := catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	parent := catalogNativeScope{scope.TenantID, strings.Repeat("c", 32)}
	source := catalogInheritanceSource{Project: "prj_0123456789abcdef", Branch: "br_0123456789abcdef", SourceBranch: "br_fedcba9876543210", Scope: scope, SourceScope: parent}
	source.Items = []catalogInheritanceItem{
		{Kind: "role", Name: "owned_role", Version: 1, Binding: &catalogObjectRef{OID: 1000, Version: 5}},
		{Kind: "role", Name: "replaced_role", Version: 1, Binding: &catalogObjectRef{OID: 2000, Version: 2}},
		{Kind: "role", Name: "legacy_role", Version: 1},
		{Kind: "database", Name: "owned_database", Owner: "owned_role", Version: 1, Binding: &catalogObjectRef{OID: 4000, OwnerOID: 1000, Version: 6}},
		{Kind: "database", Name: "replaced_database", Owner: "owned_role", Version: 1, Binding: &catalogObjectRef{OID: 5000, OwnerOID: 1000, Version: 2}},
		{Kind: "database", Name: "renamed_owner_database", Owner: "replaced_role", Version: 1, Binding: &catalogObjectRef{OID: 6000, OwnerOID: 2000, Version: 2}},
		{Kind: "database", Name: "legacy_database", Owner: "legacy_role", Version: 1},
		{Kind: "role", Name: "external_role", Version: 1, External: true, ExpectedOID: 8000, ObservedOID: 8001},
	}
	marker := "neon-control/branch/" + source.SourceBranch
	snapshot := catalogNativeSnapshot{Scope: scope, Roles: []catalogNativeRole{{OID: 1000, Name: "owned_role", Marker: marker}, {OID: 2001, Name: "replaced_role", Marker: marker}, {OID: 2000, Name: "retained_original_role", Marker: marker}, {OID: 3000, Name: "legacy_role", Marker: marker}, {OID: 8001, Name: "external_role", Marker: marker}}, Databases: []catalogNativeDatabase{{OID: 4000, OwnerOID: 1000, Name: "owned_database", Marker: marker}, {OID: 5001, OwnerOID: 1000, Name: "replaced_database", Marker: marker}, {OID: 6000, OwnerOID: 2000, Name: "renamed_owner_database", Marker: marker}, {OID: 7000, OwnerOID: 3000, Name: "legacy_database", Marker: marker}}}
	result, e := observeCatalogInheritance(source, snapshot)
	if e != nil {
		t.Fatal(e)
	}
	expected := []string{"inherited_identity", "external_identity", "legacy_unattested", "inherited_identity", "external_identity", "external_identity", "legacy_unattested", "external_identity"}
	for i, v := range result {
		if v.Status != expected[i] {
			t.Fatalf("wrong status for %s: %s", v.Name, v.Status)
		}
	}
	if result[1].OID != 2001 || result[4].OID != 5001 || result[5].OwnerOID != 2000 {
		t.Fatal("replacement/owner observation lost")
	}
	// A carried external role is never upgraded to managed, even if its marker
	// and options happen to look like an original service-owned object.
	changed := snapshot
	changed.Roles = append([]catalogNativeRole{}, snapshot.Roles...)
	changed.Roles[4].OID = 8002
	if _, e = observeCatalogInheritance(source, changed); e == nil {
		t.Fatal("a third identity replaced a retained exclusion")
	}
	wrong := source
	wrong.Scope = parent
	if _, e = observeCatalogInheritance(wrong, snapshot); e == nil {
		t.Fatal("foreign target timeline accepted")
	}
	wrong = source
	wrong.Items = append(append([]catalogInheritanceItem{}, source.Items...), source.Items[0])
	if _, e = observeCatalogInheritance(wrong, snapshot); e == nil {
		t.Fatal("ambiguous intent accepted")
	}
	missing := snapshot
	missing.Roles = append([]catalogNativeRole{}, snapshot.Roles[1:]...)
	missing.Databases = append([]catalogNativeDatabase{}, snapshot.Databases...)
	// Native snapshots must remain complete and internally consistent; this
	// missing role also owns a database and therefore cannot be a valid snapshot.
	if _, e = observeCatalogInheritance(source, missing); e == nil {
		t.Fatal("incomplete native catalog accepted")
	}
}
