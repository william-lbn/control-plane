package control

import (
	"strings"
	"testing"
)

// Management clients must preserve unknown measurements and must never gain
// backing-store identifiers or additional GC authentication mechanisms.
func TestOpenAPIStorageGCAndUsageContracts(t *testing.T) {
	spec := loadOpenAPI(t)
	paths := spec["paths"].(map[string]any)
	components := spec["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	gc := "/api/v1/platform/storage-gc/projects/{project}/runs"
	branch := "/api/v1/projects/{project}/branches/{branch}"
	routes := []struct {
		path, method, schema string
		sessionOnly, csrf    bool
	}{
		{gc, "post", "StorageGCRun", true, true},
		{gc + "/{gcRun}", "get", "StorageGCRun", true, false},
		{gc + "/{gcRun}/execute", "post", "StorageGCRun", true, true},
		{branch + "/storage/usage", "get", "StorageGatewayUsage", false, false},
		{"/api/v1/projects/{project}/storage-gateway", "get", "StorageGatewayLimits", false, false},
		{branch + "/functions/{slug}/usage", "get", "FunctionUsage", false, false},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			path, ok := paths[route.path].(map[string]any)
			if !ok {
				t.Fatal("missing public route")
			}
			op, ok := path[route.method].(map[string]any)
			if !ok {
				t.Fatal("missing public operation")
			}
			security, ok := op["security"].([]any)
			if !ok {
				t.Fatal("operation must explicitly state its authentication")
			}
			allowed := map[string]bool{}
			for _, raw := range security {
				for name := range raw.(map[string]any) {
					allowed[name] = true
				}
			}
			if !allowed["sessionCookie"] || allowed["backendCredential"] || allowed["bearerToken"] != !route.sessionOnly {
				t.Fatal("wrong management authentication boundary", allowed)
			}
			if route.sessionOnly && len(allowed) != 1 {
				t.Fatal("GC accepts a non-Console credential")
			}
			response := op["responses"].(map[string]any)["200"].(map[string]any)
			body := response["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
			if body["$ref"] != "#/components/schemas/"+route.schema {
				t.Fatal("untyped/wrong usage or cleanup response")
			}
			csrfPresent, idempotencyPresent := false, false
			for _, raw := range op["parameters"].([]any) {
				p := raw.(map[string]any)
				if target, ok := p["$ref"].(string); ok {
					p = components["parameters"].(map[string]any)[strings.TrimPrefix(target, "#/components/parameters/")].(map[string]any)
				}
				if p["name"] == "X-CSRF-Token" {
					csrfPresent = p["in"] == "header" && p["required"] == true
				}
				if p["name"] == "Idempotency-Key" {
					idempotencyPresent = p["required"] == true
				}
			}
			if csrfPresent != route.csrf {
				t.Fatal("Console mutation CSRF contract is missing or required on a read")
			}
			if route.path == gc && !idempotencyPresent {
				t.Fatal("GC plan must bind its original keyed receipt")
			}
			if strings.HasSuffix(route.path, "/execute") && idempotencyPresent {
				t.Fatal("execution replays the original run without a new key")
			}
		})
	}
	for _, name := range []string{"StorageGCRun", "StorageGCItem", "StorageGCAttempt"} {
		schema := schemas[name].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Fatal("GC responses must enumerate their safe public properties", name)
		}
		for property := range schema["properties"].(map[string]any) {
			for _, private := range []string{"blob", "nonce", "secret", "private", "store_id", "metadata", "key_hash", "request_hash"} {
				if strings.Contains(property, private) {
					t.Fatal("private backing identity in public cleanup response", name, property)
				}
			}
		}
	}
	properties := func(name string) map[string]any { return schemas[name].(map[string]any)["properties"].(map[string]any) }
	assertConst := func(name, key string, want any) {
		t.Helper()
		if properties(name)[key].(map[string]any)["const"] != want {
			t.Fatalf("%s.%s lost its observed/unknown boundary", name, key)
		}
	}
	assertConst("StorageGCRun", "scope", "never_published_only")
	assertConst("StorageGCRun", "published_history_gc", "held_native_proof_unavailable")
	assertConst("StorageGatewayUsage", "billing_enabled", false)
	assertConst("StorageGatewayUsage", "physical_capacity_measured", false)
	assertConst("StorageGatewayUsage", "unknown_usage_is_zero", false)
	assertConst("StorageGatewayLimits", "limits_managed_by", "operator")
	assertConst("FunctionUsage", "period", "retained_lifetime")
	assertConst("FunctionUsage", "cpu_basis", "cumulative_per_original_boot_not_per_request")
	assertConst("FunctionUsage", "memory_basis", "observed_current_and_peak_not_integrated_gb_hours")
	assertConst("FunctionUsage", "billing", "usage_observation_no_automatic_credit_debit")
	for _, field := range []string{"finished_at", "http_status", "ingress_bytes", "egress_bytes", "duration_ms"} {
		types, ok := properties("StorageGatewayUsageRequest")[field].(map[string]any)["type"].([]any)
		nullable := false
		for _, kind := range types {
			nullable = nullable || kind == "null"
		}
		if !ok || !nullable {
			t.Fatal("unfinished observation was contracted as a measured zero", field)
		}
	}
	if properties("StorageGatewayUsage")["items"].(map[string]any)["maxItems"] != float64(50) {
		t.Fatal("usage receipt bound differs from handler")
	}
	if properties("StorageGCPlanInput")["limit"].(map[string]any)["maximum"] != float64(100) {
		t.Fatal("cleanup plan bound differs from handler")
	}
	if properties("StorageGCExecuteInput")["confirm_run"].(map[string]any)["pattern"] != "^sgr_[a-f0-9]{24}$" {
		t.Fatal("execution confirmation lost exact run identity")
	}
}
