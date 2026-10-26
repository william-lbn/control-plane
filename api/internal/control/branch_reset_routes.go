package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"reflect"
	"strings"

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

type branchResetRoutePlan struct {
	Backing   branchResetBacking `json:"backing"`
	RoutesUID string             `json:"routes_uid"`
	Original  map[string]any     `json:"original"`
	Blocked   map[string]any     `json:"blocked"`
	Prepared  map[string]any     `json:"prepared"`
	Published map[string]any     `json:"published"`
}

func copyResetDocument(value map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(raw, &result)
	return result, err
}

func prepareBranchResetRoute(backing branchResetBacking, routesUID, namespace string, original map[string]any, roles map[string]string) (branchResetRoutePlan, error) {
	value := branchResetRoutePlan{Backing: backing, RoutesUID: routesUID}
	if validateBranchResetBacking(backing) != nil || !functionResourceUID.MatchString(routesUID) || namespace == "" || len(roles) < 2 || len(roles) > catalogInventoryLimit {
		return value, errBranchResetBacking
	}
	p := backing.Original
	if original["project_id"] != p.Project || original["branch_id"] != p.Branch || original["tenant_id"] != p.Tenant || original["kind"] != "neonvm" || original["workload"] != kubeName(p.Endpoint) || original["address"] != kubeName(p.Endpoint)+"."+namespace+".svc.cluster.local:55433" || original["role"] != "cloud_admin" {
		return value, errBranchResetBacking
	}
	if state := stringVal(original["state"]); state != "" && state != "active" && state != "ready" && state != "suspended" {
		return value, errBranchResetBacking
	}
	decode := func(encoded string) string {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return ""
		}
		return string(raw)
	}
	if roles["cloud_admin"] != decode(backing.CredentialData["adminVerifier"]) || roles["control_probe"] != decode(backing.CredentialData["probeVerifier"]) {
		return value, errBranchResetBacking
	}
	for name, verifier := range roles {
		if !validPGIdentifier(name) || !functionsql.ValidVerifier(verifier) {
			return value, errBranchResetBacking
		}
	}
	var err error
	if value.Original, err = copyResetDocument(original); err != nil {
		return value, err
	}
	if value.Blocked, err = copyResetDocument(original); err != nil {
		return value, err
	}
	value.Blocked["state"] = "restoring"
	if value.Prepared, err = copyResetDocument(value.Blocked); err != nil {
		return value, err
	}
	template, ok := value.Prepared["vm_template"].(map[string]any)
	if !ok || !owned(template, p.Project, p.Endpoint) || nested(template, "metadata", "name") != kubeName(p.Endpoint) || template["kind"] != "VirtualMachine" || template["apiVersion"] != "vm.neon.tech/v1" {
		return value, errBranchResetBacking
	}
	for _, key := range []string{"uid", "resourceVersion", "deletionTimestamp"} {
		if nested(template, "metadata", key) != nil {
			return value, errBranchResetBacking
		}
	}
	if template["status"] != nil {
		return value, errBranchResetBacking
	}
	guest, ok := nested(template, "spec", "guest").(map[string]any)
	if !ok {
		return value, errBranchResetBacking
	}
	envs, ok := guest["env"].([]any)
	if !ok {
		return value, errBranchResetBacking
	}
	seen := map[string]bool{}
	for _, raw := range envs {
		item, ok := raw.(map[string]any)
		if !ok {
			return value, errBranchResetBacking
		}
		name := stringVal(item["name"])
		if name != "TENANT_ID" && name != "TIMELINE_ID" {
			continue
		}
		if seen[name] {
			return value, errBranchResetBacking
		}
		seen[name] = true
		expected := p.Tenant
		if name == "TIMELINE_ID" {
			expected = p.Timeline
		}
		if item["value"] != expected || item["valueFrom"] != nil {
			return value, errBranchResetBacking
		}
		if name == "TIMELINE_ID" {
			item["value"] = backing.Target.TimelineID
		}
	}
	if !seen["TENANT_ID"] || !seen["TIMELINE_ID"] {
		return value, errBranchResetBacking
	}
	value.Prepared["roles"], value.Prepared["verifier"] = resetDataAny(roles), roles["cloud_admin"]
	if value.Published, err = copyResetDocument(value.Prepared); err != nil {
		return value, err
	}
	value.Published["state"] = "suspended"
	return value, nil
}

// This updates only an exact retained selector in the fixed original registry.
// Unrelated branch routes and private registry files survive every CAS retry.
func (k *kubeClient) transitionBranchResetRoute(ctx context.Context, plan branchResetRoutePlan, phase string) error {
	p := plan.Backing.Original
	roles := map[string]string{}
	rawRoles, ok := plan.Prepared["roles"].(map[string]any)
	if !ok {
		return errBranchResetBacking
	}
	for key, raw := range rawRoles {
		value, ok := raw.(string)
		if !ok {
			return errBranchResetBacking
		}
		roles[key] = value
	}
	checked, err := prepareBranchResetRoute(plan.Backing, plan.RoutesUID, k.namespace, plan.Original, roles)
	if err != nil || !reflect.DeepEqual(checked, plan) {
		return errBranchResetBacking
	}
	var before []map[string]any
	var want map[string]any
	switch phase {
	case "block":
		before, want = []map[string]any{plan.Original}, plan.Blocked
	case "prepare":
		before, want = []map[string]any{plan.Blocked}, plan.Prepared
	case "publish":
		before, want = []map[string]any{plan.Prepared}, plan.Published
	case "rollback":
		before, want = []map[string]any{plan.Prepared, plan.Blocked}, plan.Original
	default:
		return errBranchResetBacking
	}
	return k.casBranchResetRoute(ctx, plan.RoutesUID, selector(p.Endpoint), before, want)
}
func (k *kubeClient) casBranchResetRoute(ctx context.Context, uid, key string, before []map[string]any, want map[string]any) error {
	if !functionResourceUID.MatchString(uid) || key == "" {
		return errBranchResetBacking
	}
	path := k.path("secret", routesSecret)
	for attempt := 0; attempt <= 6; attempt++ {
		secret, err := k.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if nested(secret, "metadata", "uid") != uid {
			return errBranchResetBacking
		}
		raw, err := secretText(secret, "routes.json")
		if err != nil {
			return err
		}
		routes := map[string]map[string]any{}
		if len(raw) > 2<<20 || json.Unmarshal([]byte(raw), &routes) != nil {
			return errBranchResetBacking
		}
		current := routes[key]
		if reflect.DeepEqual(current, want) {
			return nil
		}
		allowed := false
		for _, candidate := range before {
			if reflect.DeepEqual(current, candidate) {
				allowed = true
				break
			}
		}
		if !allowed || attempt == 6 {
			return errBranchResetBacking
		}
		if want == nil {
			delete(routes, key)
		} else {
			routes[key] = want
		}
		encoded, err := json.Marshal(routes)
		if err != nil {
			return err
		}
		if len(encoded) > 2<<20 {
			return errBranchResetBacking
		}
		secret["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		_, err = k.request(ctx, http.MethodPut, path, secret)
		if err != nil {
			originalErr := err
			// Confirm a possible committed response loss before returning its error.
			observed, readErr := k.request(ctx, http.MethodGet, path, nil)
			if readErr == nil && nested(observed, "metadata", "uid") == uid {
				text, e := secretText(observed, "routes.json")
				var got map[string]map[string]any
				if e == nil && json.Unmarshal([]byte(text), &got) == nil && reflect.DeepEqual(got[key], want) {
					return nil
				}
			}
			var conflict kubeError
			if errors.As(err, &conflict) && conflict.Status == 409 {
				continue
			}
			return originalErr
		}
	}
	return errBranchResetBacking
}

func makeBranchResetProbeRoute(operation, project, branch, tenant, namespace, verifier string, cidrs []string) (string, map[string]any, error) {
	if !branchResetOperationID.MatchString(operation) || !dataBranchID.MatchString(branch) || project == "" || namespace == "" || !functionsql.ValidVerifier(verifier) || len(cidrs) == 0 || len(cidrs) > 32 {
		return "", nil, errBranchResetProbe
	}
	scope := catalogNativeScope{tenant, tenant}
	if !scope.valid() {
		return "", nil, errBranchResetProbe
	}
	allowed := make([]string, 0, len(cidrs))
	for _, text := range cidrs {
		prefix, err := netip.ParsePrefix(text)
		if err != nil || prefix.Bits() == 0 || prefix != prefix.Masked() || prefix.String() != text {
			return "", nil, errBranchResetProbe
		}
		internal := false
		for _, rangeText := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "fc00::/7", "::1/128"} {
			bound := netip.MustParsePrefix(rangeText)
			if bound.Addr().BitLen() == prefix.Addr().BitLen() && prefix.Bits() >= bound.Bits() && bound.Contains(prefix.Addr()) {
				internal = true
				break
			}
		}
		if !internal {
			return "", nil, errBranchResetProbe
		}
		allowed = append(allowed, text)
	}
	endpoint := branchResetProbeEndpoint(operation)
	workload := kubeName(endpoint)
	alias := "reset_" + catalogMarkerDigest(randomToken(32))[:32]
	// An observe-only route has no VM template and cannot create a replacement.
	route := map[string]any{"project_id": project, "branch_id": branch, "tenant_id": tenant, "kind": "neonvm", "workload": workload,
		"address": workload + "." + namespace + ".svc.cluster.local:55433", "role": "control_probe", "verifier": verifier, "roles": map[string]any{"control_probe": verifier},
		"allowed_ips": allowed, "state": "active", "pageserver_node_id": 2}
	normalized, err := copyResetDocument(route)
	return alias, normalized, err
}
func validBranchResetProbeAlias(alias string) bool {
	return len(alias) == len("reset_")+32 && strings.HasPrefix(alias, "reset_") && strings.Trim(strings.TrimPrefix(alias, "reset_"), "abcdef0123456789") == ""
}
