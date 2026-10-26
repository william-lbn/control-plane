package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func resetRouteFixture(t *testing.T) (branchResetRoutePlan, map[string]string) {
	t.Helper()
	backing, _, _ := resetBackingFixture(t)
	decode := func(s string) string { raw, _ := base64.StdEncoding.DecodeString(s); return string(raw) }
	roles := map[string]string{"cloud_admin": decode(backing.CredentialData["adminVerifier"]), "control_probe": decode(backing.CredentialData["probeVerifier"])}
	route := map[string]any{"project_id": backing.Original.Project, "branch_id": backing.Original.Branch, "tenant_id": backing.Original.Tenant, "kind": "neonvm", "workload": kubeName(backing.Original.Endpoint),
		"address": kubeName(backing.Original.Endpoint) + ".neon.svc.cluster.local:55433", "role": "cloud_admin", "verifier": roles["cloud_admin"], "roles": resetDataAny(roles), "allowed_ips": []any{"192.168.146.0/24"},
		"future_route_field": map[string]any{"private_material": "synthetic-original-route"},
		"vm_template": map[string]any{"apiVersion": "vm.neon.tech/v1", "kind": "VirtualMachine", "metadata": map[string]any{"name": kubeName(backing.Original.Endpoint), "labels": resetDataAny(credentialLabels(backing.Original.Project, backing.Original.Endpoint))},
			"spec": map[string]any{"guest": map[string]any{"env": []any{map[string]any{"name": "TENANT_ID", "value": backing.Original.Tenant}, map[string]any{"name": "TIMELINE_ID", "value": backing.Original.Timeline}, map[string]any{"name": "UNRELATED", "value": backing.Original.Timeline}}}}}}
	plan, err := prepareBranchResetRoute(backing, "77777777-7777-4777-8777-777777777777", "neon", route, roles)
	if err != nil {
		t.Fatal(err)
	}
	return plan, roles
}
func TestBranchResetRoutesPreserveSelectorAndDelayPublicAdmission(t *testing.T) {
	plan, roles := resetRouteFixture(t)
	before, _ := json.Marshal(plan.Original)
	if plan.Blocked["state"] != "restoring" || plan.Prepared["state"] != "restoring" || plan.Published["state"] != "suspended" {
		t.Fatal("historical route was exposed before metadata cutover")
	}
	for _, field := range []string{"project_id", "branch_id", "tenant_id", "workload", "address", "allowed_ips", "future_route_field"} {
		if !reflect.DeepEqual(plan.Original[field], plan.Published[field]) {
			t.Fatal("unrelated route or retained identity changed", field)
		}
	}
	env := nested(plan.Published, "vm_template", "spec", "guest", "env").([]any)
	values := map[string]string{}
	for _, raw := range env {
		item := raw.(map[string]any)
		values[stringVal(item["name"])] = stringVal(item["value"])
	}
	if values["TIMELINE_ID"] != plan.Backing.Target.TimelineID || values["TENANT_ID"] != plan.Backing.Original.Tenant || values["UNRELATED"] != plan.Backing.Original.Timeline {
		t.Fatal("new VM scope did not retain unrelated historical text")
	}
	after, _ := json.Marshal(plan.Original)
	if string(before) != string(after) {
		t.Fatal("rollback route changed")
	}
	for _, scenario := range []string{"namespace", "workload", "project", "tenant", "admin", "probe", "invalid_role", "template_uid", "template_owner", "template_env", "duplicate_env", "unready"} {
		t.Run(scenario, func(t *testing.T) {
			original := resetJSONCopy(plan.Original)
			nextRoles := map[string]string{}
			for k, v := range roles {
				nextRoles[k] = v
			}
			namespace := "neon"
			switch scenario {
			case "namespace":
				namespace = "other"
			case "workload":
				original["workload"] = "cp-other"
			case "project":
				original["project_id"] = "prj_other"
			case "tenant":
				original["tenant_id"] = strings.Repeat("e", 32)
			case "admin":
				nextRoles["cloud_admin"] = "changed"
			case "probe":
				delete(nextRoles, "control_probe")
			case "invalid_role":
				nextRoles["unquoted invalid role"] = "bad"
			case "template_uid":
				nested(original, "vm_template", "metadata").(map[string]any)["uid"] = "replacement"
			case "template_owner":
				nested(original, "vm_template", "metadata", "labels").(map[string]any)["neon-control/endpoint-id"] = "ep_ffffffffffffffff"
			case "template_env":
				nested(original, "vm_template", "spec", "guest", "env").([]any)[1].(map[string]any)["value"] = "foreign"
			case "duplicate_env":
				guest := nested(original, "vm_template", "spec", "guest").(map[string]any)
				guest["env"] = append(guest["env"].([]any), guest["env"].([]any)[1])
			case "unready":
				original["state"] = "deleting"
			}
			if _, err := prepareBranchResetRoute(plan.Backing, plan.RoutesUID, namespace, original, nextRoles); err == nil {
				t.Fatal("changed original route was adopted")
			}
		})
	}
}
func TestBranchResetProbeRoutePrivateCIDRsAndNoColdCreation(t *testing.T) {
	plan, roles := resetRouteFixture(t)
	alias, route, err := makeBranchResetProbeRoute(plan.Backing.Operation, plan.Backing.Original.Project, plan.Backing.Original.Branch, plan.Backing.Original.Tenant, "neon", roles["control_probe"], []string{"192.168.146.0/24", "10.42.0.0/16"})
	if err != nil || !validBranchResetProbeAlias(alias) {
		t.Fatal("private route rejected", err)
	}
	another, _, err := makeBranchResetProbeRoute(plan.Backing.Operation, plan.Backing.Original.Project, plan.Backing.Original.Branch, plan.Backing.Original.Tenant, "neon", roles["control_probe"], []string{"10.42.0.0/16"})
	if err != nil || alias == another || strings.Contains(alias, plan.Backing.Operation) {
		t.Fatal("private selector is predictable or reused")
	}
	if route["vm_template"] != nil || route["role"] != "control_probe" || len(route["roles"].(map[string]any)) != 1 || route["workload"] != kubeName(branchResetProbeEndpoint(plan.Backing.Operation)) {
		t.Fatal("private probe can expose a public Endpoint or create Compute")
	}
	for _, cidrs := range [][]string{nil, {"0.0.0.0/0"}, {"10.0.0.0/7"}, {"192.168.146.1/24"}, {"8.8.8.8/32"}, {"::/0"}, {"192.168.146.0/24", "invalid"}} {
		if _, _, err := makeBranchResetProbeRoute(plan.Backing.Operation, plan.Backing.Original.Project, plan.Backing.Original.Branch, plan.Backing.Original.Tenant, "neon", roles["control_probe"], cidrs); err == nil {
			t.Fatal("non-private or noncanonical CIDRs accepted", cidrs)
		}
	}
}
func TestBranchResetRouteCASUnknownResultRollbackAndOtherBranches(t *testing.T) {
	plan, _ := resetRouteFixture(t)
	key := selector(plan.Backing.Original.Endpoint)
	unrelated := map[string]any{"project_id": "other-project", "branch_id": "other-branch", "private_material": "synthetic-preserved-other-route"}
	document := map[string]map[string]any{key: plan.Original, "ep-other": unrelated}
	encoded, _ := json.Marshal(document)
	secret := map[string]any{"metadata": map[string]any{"uid": plan.RoutesUID, "resourceVersion": "1"}, "data": map[string]any{"routes.json": base64.StdEncoding.EncodeToString(encoded), "other.private": "cHJlc2VydmVk"}}
	var mu sync.Mutex
	puts := 0
	lost, conflict := true, true
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			puts++
			if conflict {
				conflict = false
				w.WriteHeader(409)
				return
			}
			var next map[string]any
			if json.NewDecoder(r.Body).Decode(&next) != nil {
				w.WriteHeader(422)
				return
			}
			if nested(next, "metadata", "uid") != plan.RoutesUID || nested(next, "data", "other.private") != "cHJlc2VydmVk" {
				t.Error("global registry identity or other private files changed")
			}
			secret = next
			if lost {
				lost = false
				w.WriteHeader(503)
				return
			}
		}
		jsonResponse(w, 200, secret)
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	for _, phase := range []string{"block", "block", "prepare", "prepare", "rollback", "rollback", "block", "prepare", "publish", "publish"} {
		if err := k.transitionBranchResetRoute(context.Background(), plan, phase); err != nil {
			t.Fatal(phase, err)
		}
	}
	mu.Lock()
	count := puts
	text, _ := secretText(secret, "routes.json")
	mu.Unlock()
	var got map[string]map[string]any
	_ = json.Unmarshal([]byte(text), &got)
	if count != 7 || !reflect.DeepEqual(got[key], plan.Published) || !reflect.DeepEqual(got["ep-other"], unrelated) {
		t.Fatal("route replay rewrote a committed effect or changed another branch", count)
	}
	if k.transitionBranchResetRoute(context.Background(), plan, "rollback") == nil {
		t.Fatal("already published restore was silently rewound")
	}
	mu.Lock()
	secret["metadata"].(map[string]any)["uid"] = "88888888-8888-4888-8888-888888888888"
	mu.Unlock()
	if k.transitionBranchResetRoute(context.Background(), plan, "publish") == nil {
		t.Fatal("same-name replacement registry accepted")
	}
}
