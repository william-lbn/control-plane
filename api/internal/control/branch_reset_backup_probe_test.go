package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBranchResetBackupPinnedReadonlyScopeAndNoOrdinaryReplay(t *testing.T) {
	source, original, target := resetConfigFixture(t, "read_only")
	source, err := branchResetComputeConfig(source, original, target)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(source)
	operation := "op_" + strings.Repeat("a", 24)
	p := createPayload{ProjectID: original.Project, BranchID: original.Branch, EndpointID: branchResetBackupProbeEndpoint(operation), EndpointType: "read_only", TenantID: target.TenantID, TimelineID: target.TimelineID, CatalogPolicy: catalogPreserveInheritedSQL, ResetInspectionOperation: operation, ResetInspectionLSN: "0/4000"}
	doc, err := resetBackupInspectionConfig(source, p.ResetInspectionLSN)
	if err != nil || validatePersistedBranchResetInspection(p, doc) != nil {
		t.Fatal("exact readonly backup rejected", err)
	}
	if nested(doc, "spec", "mode").(map[string]any)["Static"] != "0/4000" || nested(doc, "spec", "skip_pg_catalog_updates") != true || len(nested(doc, "spec", "cluster", "roles").([]any)) != 2 || len(nested(doc, "spec", "cluster", "databases").([]any)) != 0 || len(nested(doc, "spec", "delta_operations").([]any)) != 0 {
		t.Fatal("backup inspection could replay ordinary SQL or use another point")
	}
	values := map[string]string{}
	for _, raw := range nested(doc, "spec", "cluster", "settings").([]any) {
		item := raw.(map[string]any)
		values[stringVal(item["name"])] = stringVal(item["value"])
	}
	if values["cron.launch_active_jobs"] != "off" || values["default_transaction_read_only"] != "on" || values["hot_standby"] != "on" || values["primary_conninfo"] != "" || values["primary_slot_name"] != "" {
		t.Fatal("backup can launch jobs, write or become a streaming standby")
	}
	after, _ := json.Marshal(source)
	if string(before) != string(after) {
		t.Fatal("original private config changed")
	}
	roots, err := branchResetConfigRootRoles(doc)
	if err != nil {
		t.Fatal(err)
	}
	alias, route, err := makeBranchResetBackupProbeRoute(operation, original.Project, original.Branch, original.Tenant, "neon", roots["control_probe"], []string{"10.42.0.0/16"})
	if err != nil || !validBranchResetProbeAlias(alias) || route["vm_template"] != nil || route["workload"] != kubeName(p.EndpointID) || p.EndpointID == branchResetProbeEndpoint(operation) {
		t.Fatal("backup shared restored Writer identity or cold creation route", err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), operation) || strings.Contains(string(raw), p.ResetInspectionLSN) {
		t.Fatal("private inspection startup leaked into Operation payload")
	}
	// The backup's Static mode is ineligible for protected-platform mutation.
	if _, err = branchResetPlatformProjection(doc, nested(doc, "spec", "cluster", "roles").([]any)); err == nil {
		t.Fatal("backup accepted target Writer platform configure")
	}
}
func TestBranchResetBackupRejectsChangedPointPurposeAndPersistedConfig(t *testing.T) {
	source, original, target := resetConfigFixture(t, "read_write")
	source, err := branchResetComputeConfig(source, original, target)
	if err != nil {
		t.Fatal(err)
	}
	operation := "op_" + strings.Repeat("a", 24)
	p := createPayload{ProjectID: original.Project, BranchID: original.Branch, EndpointID: branchResetBackupProbeEndpoint(operation), EndpointType: "read_only", TenantID: target.TenantID, TimelineID: target.TimelineID, CatalogPolicy: catalogPreserveInheritedSQL, ResetInspectionOperation: operation, ResetInspectionLSN: "0/4000"}
	doc, err := resetBackupInspectionConfig(source, p.ResetInspectionLSN)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"public_endpoint", "writer_identity", "rw_backup", "bad_lsn", "different_point", "different_tenant", "different_timeline", "modern_override", "missing_skip", "ordinary_role", "ordinary_database", "delete_delta", "cron", "readonly", "streaming", "duplicate_setting", "uninitialized"} {
		t.Run(scenario, func(t *testing.T) {
			payload := p
			config := resetJSONCopy(doc)
			switch scenario {
			case "public_endpoint":
				payload.EndpointID = original.Endpoint
			case "writer_identity":
				payload.EndpointID = branchResetProbeEndpoint(operation)
			case "rw_backup":
				payload.EndpointType = "read_write"
			case "bad_lsn":
				payload.ResetInspectionLSN = "0/4001"
			case "different_point":
				config["spec"].(map[string]any)["mode"] = map[string]any{"Static": "0/5000"}
			case "different_tenant":
				payload.TenantID = strings.Repeat("e", 32)
			case "different_timeline":
				payload.TimelineID = strings.Repeat("e", 32)
			case "modern_override":
				config["spec"].(map[string]any)["timeline_id"] = original.Timeline
			case "missing_skip":
				config["spec"].(map[string]any)["skip_pg_catalog_updates"] = false
			case "ordinary_role":
				cluster := nested(config, "spec", "cluster").(map[string]any)
				cluster["roles"] = append(cluster["roles"].([]any), map[string]any{"name": "ordinary_user"})
			case "ordinary_database":
				nested(config, "spec", "cluster").(map[string]any)["databases"] = []any{map[string]any{"name": "ordinary_data"}}
			case "delete_delta":
				config["spec"].(map[string]any)["delta_operations"] = []any{map[string]any{"action": "delete_db", "name": "ordinary_data"}}
			case "cron", "readonly":
				for _, raw := range nested(config, "spec", "cluster", "settings").([]any) {
					item := raw.(map[string]any)
					if scenario == "cron" && item["name"] == "cron.launch_active_jobs" {
						item["value"] = "on"
					}
					if scenario == "readonly" && item["name"] == "default_transaction_read_only" {
						item["value"] = "off"
					}
				}
			case "streaming":
				cluster := nested(config, "spec", "cluster").(map[string]any)
				cluster["settings"] = append(cluster["settings"].([]any), map[string]any{"name": "primary_conninfo", "value": "host=other"})
			case "duplicate_setting":
				cluster := nested(config, "spec", "cluster").(map[string]any)
				cluster["settings"] = append(cluster["settings"].([]any), cluster["settings"].([]any)[0])
			case "uninitialized":
				payload.CatalogInitializePlatform = true
			}
			if validatePersistedBranchResetInspection(payload, config) == nil {
				t.Fatal("changed backup scope or purpose was accepted")
			}
		})
	}
	for _, lsn := range []string{"", "0/0", "0/4001", "0/00004000", "not-an-lsn"} {
		if _, err := resetBackupInspectionConfig(source, lsn); err == nil {
			t.Fatal("noncanonical backup point accepted", lsn)
		}
	}
}
