package control

import "strings"

func branchResetBackupProbeEndpoint(operation string) string {
	return "ep_" + catalogMarkerDigest(operation + "/private-reset-backup-probe")[:16]
}
func branchResetHasNativeOverrides(spec map[string]any) bool {
	for _, key := range []string{"tenant_id", "timeline_id", "pageserver_connstring", "pageserver_connection_info", "shard_stripe_size", "safekeeper_connstrings", "safekeepers_generation"} {
		if _, present := spec[key]; present {
			return true
		}
	}
	return false
}
func validateBranchResetInspectionPayload(p createPayload) error {
	if p.ResetInspectionOperation == "" {
		if p.ResetInspectionLSN != "" {
			return errBranchResetProbe
		}
		return nil
	}
	if !branchResetOperationID.MatchString(p.ResetInspectionOperation) || p.CatalogPolicy != catalogPreserveInheritedSQL || p.CatalogInitializePlatform {
		return errBranchResetProbe
	}
	if p.ResetInspectionLSN == "" {
		if bootstrapEndpointType(p.EndpointType) != "read_write" || p.EndpointID != branchResetProbeEndpoint(p.ResetInspectionOperation) {
			return errBranchResetProbe
		}
		return nil
	}
	canonical, err := canonicalLSN(p.ResetInspectionLSN)
	point, parseErr := parseLSN(p.ResetInspectionLSN)
	if err != nil || parseErr != nil || point == 0 || point%8 != 0 || canonical != p.ResetInspectionLSN || p.EndpointType != "read_only" || p.EndpointID != branchResetBackupProbeEndpoint(p.ResetInspectionOperation) {
		return errBranchResetProbe
	}
	return nil
}

// A backup is inspected through a distinct native Static node pinned to the
// exact retained head. It cannot advance its physical backup timeline or run
// customer writes. The ordinary Writer and restored target remain absent.
func resetBackupInspectionConfig(source map[string]any, lsn string) (map[string]any, error) {
	canonical, err := canonicalLSN(lsn)
	point, parseErr := parseLSN(lsn)
	if err != nil || parseErr != nil || point == 0 || point%8 != 0 || canonical != lsn {
		return nil, errBranchResetProbe
	}
	next, err := catalogInheritanceConfig(source, true, nil)
	if err != nil {
		return nil, err
	}
	spec := next["spec"].(map[string]any)
	mode, ok := gatewayComputeMode(next)
	if !ok || mode != "Primary" && mode != "Replica" || spec["skip_pg_catalog_updates"] != true || branchResetHasNativeOverrides(spec) || spec["local_proxy_config"] != nil || spec["drop_subscriptions_before_start"] != nil && spec["drop_subscriptions_before_start"] != false {
		return nil, errBranchResetProbe
	}
	cluster := spec["cluster"].(map[string]any)
	rawSettings, ok := cluster["settings"].([]any)
	if !ok {
		return nil, errBranchResetProbe
	}
	settings := []any{}
	seen := map[string]bool{}
	for _, raw := range rawSettings {
		item, ok := raw.(map[string]any)
		name := stringVal(item["name"])
		if !ok || name == "" || seen[name] {
			return nil, errBranchResetProbe
		}
		seen[name] = true
		// Static recovery must never become a streaming standby after this point.
		if name == "primary_conninfo" || name == "primary_slot_name" {
			continue
		}
		if name == "cron.launch_active_jobs" {
			item["value"], item["vartype"] = "off", "bool"
		}
		if name == "default_transaction_read_only" || name == "hot_standby" {
			item["value"], item["vartype"] = "on", "bool"
		}
		settings = append(settings, item)
	}
	for _, setting := range []struct{ Name, Value string }{{"cron.launch_active_jobs", "off"}, {"default_transaction_read_only", "on"}, {"hot_standby", "on"}} {
		if !seen[setting.Name] {
			settings = append(settings, map[string]any{"name": setting.Name, "value": setting.Value, "vartype": "bool"})
		}
	}
	cluster["settings"] = settings
	spec["mode"] = map[string]any{"Static": lsn}
	return next, nil
}
func validatePersistedBranchResetInspection(p createPayload, doc map[string]any) error {
	if err := validateBranchResetInspectionPayload(p); err != nil {
		return err
	}
	if p.ResetInspectionOperation == "" {
		return nil
	}
	spec, ok := doc["spec"].(map[string]any)
	if !ok || spec["skip_pg_catalog_updates"] != true || branchResetHasNativeOverrides(spec) || nested(doc, "spec", "cluster", "cluster_id") != p.ProjectID {
		return errBranchResetProbe
	}
	mode, valid := gatewayComputeMode(doc)
	if !valid {
		return errBranchResetProbe
	}
	if p.ResetInspectionLSN == "" {
		if mode != "Primary" {
			return errBranchResetProbe
		}
	} else {
		tagged, ok := mode.(map[string]any)
		if !ok || tagged["Static"] != p.ResetInspectionLSN {
			return errBranchResetProbe
		}
	}
	if _, err := branchResetConfigRootRoles(doc); err != nil {
		return err
	}
	databases, ok := nested(doc, "spec", "cluster", "databases").([]any)
	if !ok || len(databases) != 0 {
		return errBranchResetProbe
	}
	deltas, ok := spec["delta_operations"].([]any)
	if !ok || len(deltas) != 0 {
		return errBranchResetProbe
	}
	raw, ok := nested(doc, "spec", "cluster", "settings").([]any)
	if !ok {
		return errBranchResetProbe
	}
	settings := map[string]string{}
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		name := stringVal(item["name"])
		text, textOK := item["value"].(string)
		if !ok || !textOK || name == "" {
			return errBranchResetProbe
		}
		if _, duplicate := settings[name]; duplicate {
			return errBranchResetProbe
		}
		settings[name] = text
	}
	if settings["neon.tenant_id"] != p.TenantID || settings["neon.timeline_id"] != p.TimelineID || settings["cron.launch_active_jobs"] != "off" {
		return errBranchResetProbe
	}
	if p.ResetInspectionLSN != "" && (settings["default_transaction_read_only"] != "on" || settings["hot_standby"] != "on" || settings["primary_conninfo"] != "" || settings["primary_slot_name"] != "") {
		return errBranchResetProbe
	}
	if spec["local_proxy_config"] != nil || spec["drop_subscriptions_before_start"] != nil && spec["drop_subscriptions_before_start"] != false {
		return errBranchResetProbe
	}
	return nil
}

// This path has no management grant. In particular a Static backup must not
// receive the target Writer's protected-platform password authorization.
func isBranchResetBackupEndpoint(operation, endpoint string) bool {
	return branchResetOperationID.MatchString(operation) && strings.HasPrefix(endpoint, "ep_") && endpoint == branchResetBackupProbeEndpoint(operation)
}
