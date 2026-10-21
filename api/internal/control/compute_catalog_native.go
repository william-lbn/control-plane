package control

import (
	"encoding/json"
	"errors"
)

// Native configure also honors skip_pg_catalog_updates. An explicitly authorized
// role birth must enable SQL in a transient target-only spec, never replay the
// retained full catalog. The second spec restores the protected runtime view.
func computeCatalogBootstrapNativeRoleConfigs(source map[string]any, p catalogPayload, verifier string) (map[string]any, map[string]any, error) {
	if !validPGIdentifier(p.Name) || protectedPGRole(p.Name) || verifier == "" {
		return nil, nil, errors.New("native role creation target rejected")
	}
	clone := func(value map[string]any) (map[string]any, error) {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var copy map[string]any
		err = json.Unmarshal(raw, &copy)
		return copy, err
	}
	restore, err := clone(source)
	if err != nil {
		return nil, nil, err
	}
	spec, ok := restore["spec"].(map[string]any)
	if !ok || spec["skip_pg_catalog_updates"] != true || (spec["mode"] != nil && spec["mode"] != "Primary") {
		return nil, nil, errors.New("native role creation requires initialized Primary catalog protection")
	}
	if spec["local_proxy_config"] != nil || (spec["drop_subscriptions_before_start"] != nil && spec["drop_subscriptions_before_start"] != false) {
		return nil, nil, errors.New("native role creation optional startup services require separate migration")
	}
	cluster, ok := spec["cluster"].(map[string]any)
	if !ok || cluster["cluster_id"] != p.ProjectID {
		return nil, nil, errors.New("native role creation project scope mismatch")
	}
	roles, ok := cluster["roles"].([]any)
	if !ok {
		return nil, nil, errors.New("native role creation role intent invalid")
	}
	names := map[string]bool{}
	for _, entry := range roles {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, nil, errors.New("native role creation role invalid")
		}
		name := stringVal(item["name"])
		if name == "" || names[name] || name == p.Name {
			return nil, nil, errors.New("native role creation requires one new absent intent")
		}
		names[name] = true
	}
	if !names["cloud_admin"] || !names["control_probe"] {
		return nil, nil, errors.New("native role creation platform intents missing")
	}
	if err = catalogDelta(restore, p, "create_role", verifier); err != nil {
		return nil, nil, err
	}
	apply, err := clone(restore)
	if err != nil {
		return nil, nil, err
	}
	applySpec := apply["spec"].(map[string]any)
	applyCluster := applySpec["cluster"].(map[string]any)
	applyCluster["roles"] = []any{map[string]any{"name": p.Name, "encrypted_password": verifier, "options": []any{map[string]any{"name": "ADMIN", "value": "control_probe", "vartype": "enum"}}}}
	applyCluster["databases"] = []any{}
	applySpec["delta_operations"] = []any{}
	applySpec["skip_pg_catalog_updates"] = false
	return apply, restore, nil
}
