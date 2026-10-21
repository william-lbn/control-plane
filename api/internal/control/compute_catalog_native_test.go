package control

import (
	"encoding/json"
	"testing"
)

func TestComputeCatalogBootstrapNativeTargetOnlyRole(t *testing.T) {
	p := catalogPayload{ProjectID: "prj_0123456789abcdef", Name: "new_customer"}
	raw, err := computeConfig(p.ProjectID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "admin-intent", "probe-intent", "read_write")
	if err != nil {
		t.Fatal(err)
	}
	var source map[string]any
	_ = json.Unmarshal([]byte(raw), &source)
	spec := source["spec"].(map[string]any)
	cluster := spec["cluster"].(map[string]any)
	spec["skip_pg_catalog_updates"] = true
	spec["delta_operations"] = []any{map[string]any{"action": "delete_role", "name": "retained_role"}, map[string]any{"action": "delete_db", "name": "retained_database"}}
	cluster["roles"] = append(cluster["roles"].([]any), map[string]any{"name": "same_name_external_replacement", "encrypted_password": "old-intent-must-not-apply"})
	cluster["databases"] = []any{map[string]any{"name": "retained_database", "owner": "old-owner-must-not-apply"}}
	source["retained"] = map[string]any{"unknown": true}
	before, _ := json.Marshal(source)
	apply, restore, err := computeCatalogBootstrapNativeRoleConfigs(source, p, "new-intent")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(source)
	if string(before) != string(after) {
		t.Fatal("projection modified persisted cold config input")
	}
	if nested(apply, "spec", "skip_pg_catalog_updates") != false || nested(restore, "spec", "skip_pg_catalog_updates") != true {
		t.Fatal("explicit native role apply or restoration skipped")
	}
	roles := nested(apply, "spec", "cluster", "roles").([]any)
	if len(roles) != 1 || nested(roles[0].(map[string]any), "name") != p.Name || nested(roles[0].(map[string]any), "encrypted_password") != "new-intent" || len(nested(apply, "spec", "cluster", "databases").([]any)) != 0 || len(nested(apply, "spec", "delta_operations").([]any)) != 0 {
		t.Fatal("native request may replay unrelated catalog")
	}
	options := roles[0].(map[string]any)["options"].([]any)
	if len(options) != 1 || options[0].(map[string]any)["name"] != "ADMIN" || options[0].(map[string]any)["value"] != "control_probe" {
		t.Fatal("native CREATE lacks exact ADMIN target")
	}
	expected := map[string]any{}
	_ = json.Unmarshal(before, &expected)
	if err = catalogDelta(expected, p, "create_role", "new-intent"); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(expected)
	got, _ := json.Marshal(restore)
	if string(want) != string(got) {
		t.Fatal("runtime restoration lost cold intents, settings or tombstones")
	}
	for _, scenario := range []string{"uninitialized", "replica", "foreign_project", "protected_target", "existing_intent", "duplicate_role", "missing_platform_role", "optional_proxy", "subscriptions"} {
		t.Run(scenario, func(t *testing.T) {
			var changed map[string]any
			_ = json.Unmarshal(before, &changed)
			candidate := p
			changedSpec := changed["spec"].(map[string]any)
			changedCluster := changedSpec["cluster"].(map[string]any)
			switch scenario {
			case "uninitialized":
				delete(changedSpec, "skip_pg_catalog_updates")
			case "replica":
				changedSpec["mode"] = "Replica"
			case "foreign_project":
				changedCluster["cluster_id"] = "other-project"
			case "protected_target":
				candidate.Name = "control_probe"
			case "existing_intent":
				candidate.Name = "same_name_external_replacement"
			case "duplicate_role":
				changedCluster["roles"] = append(changedCluster["roles"].([]any), changedCluster["roles"].([]any)[0])
			case "missing_platform_role":
				changedCluster["roles"] = []any{}
			case "optional_proxy":
				changedSpec["local_proxy_config"] = map[string]any{}
			case "subscriptions":
				changedSpec["drop_subscriptions_before_start"] = true
			}
			original, _ := json.Marshal(changed)
			if _, _, err = computeCatalogBootstrapNativeRoleConfigs(changed, candidate, "new-intent"); err == nil {
				t.Fatal("unsafe native role projection accepted")
			}
			now, _ := json.Marshal(changed)
			if string(original) != string(now) {
				t.Fatal("rejected projection modified config")
			}
		})
	}
}
