package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func resetConfigFixture(t *testing.T, endpointType string) (map[string]any, computeCatalogBootstrap, catalogNativeScope) {
	t.Helper()
	admin, err := scramVerifier("synthetic-reset-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := scramVerifier("synthetic-reset-probe-password")
	if err != nil {
		t.Fatal(err)
	}
	original := computeCatalogBootstrap{Project: "prj_0123456789abcdef", Branch: "br_0123456789abcdef", Endpoint: "ep_0123456789abcdef", EndpointType: endpointType, Tenant: strings.Repeat("a", 32), Timeline: strings.Repeat("b", 32), ConfigUID: "original-config", CredentialUID: "original-credential", Operation: "op_original", Witness: "original_sql_probe"}
	raw, err := computeConfig(original.Project, original.Tenant, original.Timeline, admin, probe, endpointType)
	if err != nil {
		t.Fatal(err)
	}
	var source map[string]any
	if json.Unmarshal([]byte(raw), &source) != nil {
		t.Fatal("fixture configuration invalid")
	}
	spec := source["spec"].(map[string]any)
	spec["skip_pg_catalog_updates"] = true
	spec["delta_operations"] = []any{record{"action": "delete_db", "name": "created_after_restore"}}
	cluster := spec["cluster"].(map[string]any)
	cluster["roles"] = append(cluster["roles"].([]any), record{"name": "created_after_restore", "encrypted_password": admin})
	cluster["databases"] = append(cluster["databases"].([]any), record{"name": "created_after_restore", "owner": "cloud_admin"})
	source["unrelated_private_extension"] = record{"opaque": original.Timeline, "private_material": "synthetic-original-material"}
	return source, original, catalogNativeScope{TenantID: original.Tenant, TimelineID: strings.Repeat("c", 32)}
}

func TestBranchResetStartupIsolatedScopeAndOriginalPrivateMaterial(t *testing.T) {
	for _, endpointType := range []string{"read_write", "read_only"} {
		source, original, target := resetConfigFixture(t, endpointType)
		before, _ := json.Marshal(source)
		next, err := branchResetComputeConfig(source, original, target)
		if err != nil {
			t.Fatal(endpointType, err)
		}
		after, _ := json.Marshal(source)
		if string(before) != string(after) {
			t.Fatal("rollback source was mutated")
		}
		oldExtension, _ := json.Marshal(source["unrelated_private_extension"])
		newExtension, _ := json.Marshal(next["unrelated_private_extension"])
		if string(oldExtension) != string(newExtension) {
			t.Fatal("unrelated private file or incidental old timeline text changed")
		}
		spec := next["spec"].(map[string]any)
		cluster := spec["cluster"].(map[string]any)
		if spec["skip_pg_catalog_updates"] != true || len(spec["delta_operations"].([]any)) != 0 || len(cluster["databases"].([]any)) != 0 || len(cluster["roles"].([]any)) != 2 {
			t.Fatal("current catalog intent could execute on historical SQL")
		}
		oldRoles := source["spec"].(map[string]any)["cluster"].(map[string]any)["roles"].([]any)
		for i, role := range cluster["roles"].([]any) {
			a, _ := json.Marshal(role)
			b, _ := json.Marshal(oldRoles[i])
			if string(a) != string(b) {
				t.Fatal("reserved platform identity changed")
			}
		}
		settings := map[string]string{}
		for _, raw := range cluster["settings"].([]any) {
			item := raw.(map[string]any)
			settings[stringVal(item["name"])] = stringVal(item["value"])
		}
		if settings["neon.timeline_id"] != target.TimelineID || settings["neon.tenant_id"] != original.Tenant {
			t.Fatal("new native scope not selected")
		}
		if endpointType == "read_only" && (settings["primary_conninfo"] != branchResetReplicaConnection(target.TenantID, target.TimelineID) || settings["primary_slot_name"] != "repl_"+target.TimelineID+"_") {
			t.Fatal("Reader replication retained the old timeline")
		}
	}
}

func TestBranchResetStartupRejectsUnattestedAndAmbiguousSources(t *testing.T) {
	for _, key := range []string{"tenant_id", "timeline_id", "pageserver_connstring", "pageserver_connection_info", "shard_stripe_size", "safekeeper_connstrings", "safekeepers_generation"} {
		source, original, target := resetConfigFixture(t, "read_write")
		source["spec"].(map[string]any)[key] = original.Timeline
		if _, err := branchResetComputeConfig(source, original, target); err == nil {
			t.Fatal("modern override could boot a different native timeline", key)
		}
	}
	for _, mutate := range []func(map[string]any, *computeCatalogBootstrap, *catalogNativeScope){
		func(_ map[string]any, v *computeCatalogBootstrap, _ *catalogNativeScope) { v.ConfigUID = "" },
		func(_ map[string]any, v *computeCatalogBootstrap, _ *catalogNativeScope) { v.Witness = "" },
		func(_ map[string]any, v *computeCatalogBootstrap, _ *catalogNativeScope) {
			v.Witness = "unknown_receipt"
		},
		func(_ map[string]any, v *computeCatalogBootstrap, target *catalogNativeScope) {
			target.TimelineID = v.Timeline
		},
		func(_ map[string]any, _ *computeCatalogBootstrap, target *catalogNativeScope) {
			target.TenantID = strings.Repeat("d", 32)
		},
		func(s map[string]any, _ *computeCatalogBootstrap, _ *catalogNativeScope) {
			s["spec"].(map[string]any)["skip_pg_catalog_updates"] = false
		},
		func(s map[string]any, _ *computeCatalogBootstrap, _ *catalogNativeScope) {
			s["spec"].(map[string]any)["drop_subscriptions_before_start"] = true
		},
		func(s map[string]any, _ *computeCatalogBootstrap, _ *catalogNativeScope) {
			s["spec"].(map[string]any)["cluster"].(map[string]any)["cluster_id"] = "other-project"
		},
		func(s map[string]any, _ *computeCatalogBootstrap, _ *catalogNativeScope) {
			s["spec"].(map[string]any)["mode"] = "Primary"
		},
		func(s map[string]any, v *computeCatalogBootstrap, _ *catalogNativeScope) {
			cluster := s["spec"].(map[string]any)["cluster"].(map[string]any)
			cluster["settings"] = append(cluster["settings"].([]any), record{"name": "neon.timeline_id", "value": v.Timeline})
		},
		func(s map[string]any, _ *computeCatalogBootstrap, _ *catalogNativeScope) {
			for _, raw := range s["spec"].(map[string]any)["cluster"].(map[string]any)["settings"].([]any) {
				item := raw.(map[string]any)
				if item["name"] == "primary_conninfo" {
					item["value"] = "host=other-replica-source"
				}
			}
		},
	} {
		source, original, target := resetConfigFixture(t, "read_only")
		mutate(source, &original, &target)
		before, _ := json.Marshal(source)
		if _, err := branchResetComputeConfig(source, original, target); err == nil {
			t.Fatal("unattested or ambiguous source accepted")
		}
		after, _ := json.Marshal(source)
		if string(before) != string(after) {
			t.Fatal("rejected reset mutated original configuration")
		}
	}
}
