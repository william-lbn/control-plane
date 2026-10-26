package control

import (
	"errors"
	"fmt"
)

var errBranchResetConfig = errors.New("original sealed Compute scope or explicit reset target rejected")

// Prepare an isolated historical startup view. This does not publish it or
// change a VM. Admission and the Worker must separately retain the original
// Secret UID, exact bytes and bootstrap receipt for compare-and-swap/rollback.
// Current ordinary catalog intent must never run against historical SQL.
func branchResetComputeConfig(source map[string]any, original computeCatalogBootstrap, target catalogNativeScope) (map[string]any, error) {
	current := catalogNativeScope{TenantID: original.Tenant, TimelineID: original.Timeline}
	if !current.valid() || !target.valid() || current.TenantID != target.TenantID || current.TimelineID == target.TimelineID || original.ConfigUID == "" || original.CredentialUID == "" || original.Operation == "" || original.Witness == "" {
		return nil, errBranchResetConfig
	}
	if original.Witness != "original_sql_probe" && original.Witness != "legacy_successful_probe_initial_backing_observation" && original.Witness != "branch_reset_sql_probe_v1" {
		return nil, errBranchResetConfig
	}
	spec, ok := source["spec"].(map[string]any)
	if !ok || spec["skip_pg_catalog_updates"] != true || spec["local_proxy_config"] != nil || spec["drop_subscriptions_before_start"] != nil && spec["drop_subscriptions_before_start"] != false {
		return nil, errBranchResetConfig
	}
	// The pinned native spec also accepts modern identity overrides. This
	// restore path handles one canonical legacy representation; never retain an
	// override that could boot the old timeline while its GUC says the new one.
	for _, key := range []string{"tenant_id", "timeline_id", "pageserver_connstring", "pageserver_connection_info", "shard_stripe_size", "safekeeper_connstrings", "safekeepers_generation"} {
		if _, present := spec[key]; present {
			return nil, errBranchResetConfig
		}
	}
	mode := stringVal(spec["mode"])
	reader := original.EndpointType == "read_only"
	if reader && mode != "Replica" || !reader && (bootstrapEndpointType(original.EndpointType) != "read_write" || mode != "" && mode != "Primary") {
		return nil, errBranchResetConfig
	}
	cluster, ok := spec["cluster"].(map[string]any)
	if !ok || cluster["cluster_id"] != original.Project {
		return nil, errBranchResetConfig
	}
	// The existing inheritance transform clones unknown fields and credentials,
	// retains the two reserved platform intents, and clears ordinary DDL/deltas.
	prepared, err := catalogInheritanceConfig(source, true, nil)
	if err != nil {
		return nil, err
	}
	nextSpec := prepared["spec"].(map[string]any)
	nextCluster := nextSpec["cluster"].(map[string]any)
	settings, ok := nextCluster["settings"].([]any)
	if !ok {
		return nil, errBranchResetConfig
	}
	seen := map[string]bool{}
	for _, raw := range settings {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errBranchResetConfig
		}
		name := stringVal(item["name"])
		if name != "neon.tenant_id" && name != "neon.timeline_id" && name != "primary_conninfo" && name != "primary_slot_name" {
			continue
		}
		if seen[name] {
			return nil, errBranchResetConfig
		}
		seen[name] = true
		value := stringVal(item["value"])
		switch name {
		case "neon.tenant_id":
			if value != original.Tenant {
				return nil, errBranchResetConfig
			}
		case "neon.timeline_id":
			if value != original.Timeline {
				return nil, errBranchResetConfig
			}
			item["value"] = target.TimelineID
		case "primary_conninfo":
			if !reader || value != branchResetReplicaConnection(original.Tenant, original.Timeline) {
				return nil, errBranchResetConfig
			}
			item["value"] = branchResetReplicaConnection(target.TenantID, target.TimelineID)
		case "primary_slot_name":
			if !reader || value != "repl_"+original.Timeline+"_" {
				return nil, errBranchResetConfig
			}
			item["value"] = "repl_" + target.TimelineID + "_"
		}
	}
	if !seen["neon.tenant_id"] || !seen["neon.timeline_id"] || reader && (!seen["primary_conninfo"] || !seen["primary_slot_name"]) {
		return nil, errBranchResetConfig
	}
	nextSpec["skip_pg_catalog_updates"] = true
	return prepared, nil
}

func branchResetReplicaConnection(tenant, timeline string) string {
	return fmt.Sprintf("host=safekeeper1,safekeeper2,safekeeper3 port=5454,5454,5454 options='-c timeline_id=%s tenant_id=%s' application_name=replica replication=true", timeline, tenant)
}
