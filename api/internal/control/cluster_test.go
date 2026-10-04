package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStableCreateIdentityAndCredentialReplay(t *testing.T) {
	a := stableSuffix("actor", "POST:/projects", "same-request-key")
	if a != stableSuffix("actor", "POST:/projects", "same-request-key") ||
		a == stableSuffix("actor", "POST:/projects", "different-key") ||
		a == stableSuffix("other", "POST:/projects", "same-request-key") {
		t.Fatal("create IDs must be stable and scoped")
	}
	if len(stableHex(a, "tenant")) != 32 || stableHex(a, "tenant") == stableHex(a, "timeline") {
		t.Fatal("Neon IDs must be distinct 32 digit hex strings")
	}
	verifier, err := scramVerifier("the-strong-test-password")
	if err != nil || !scramMatches("the-strong-test-password", verifier) ||
		scramMatches("a-different-password", verifier) || strings.Contains(verifier, "strong-test") {
		t.Fatal("SCRAM verifier should authenticate only the original password without exposing it")
	}
}

func TestCreatePayloadAndComputeTemplate(t *testing.T) {
	p := createPayload{ProjectID: "prj_example", BranchID: "br_example", EndpointID: "ep_example",
		TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("b", 32),
		MinCPU: 1000, MaxCPU: 2000, MinMem: 1024, MaxMem: 3072}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var decoded createPayload
	if err := json.Unmarshal(b, &decoded); err != nil || decoded != p {
		t.Fatalf("operation payload must survive PostgreSQL JSON round trip: %v", err)
	}
	config, err := computeConfig(p.ProjectID, p.TenantID, p.TimelineID, "admin-verifier", "probe-verifier", "read_write")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(config), &doc); err != nil {
		t.Fatal(err)
	}
	if nested(doc, "spec", "cluster", "cluster_id") != p.ProjectID ||
		strings.Contains(config, "REPLACED_AT_RUNTIME") || strings.Contains(config, "docker_compose_test") {
		t.Fatal("compute configuration retained template identities")
	}
	if !strings.Contains(config, "admin-verifier") || !strings.Contains(config, "probe-verifier") {
		t.Fatal("compute configuration did not install SCRAM verifiers")
	}
}

func TestReadReplicaComputeMode(t *testing.T) {
	config, err := computeConfig("prj_test", strings.Repeat("a", 32), strings.Repeat("b", 32), "admin", "probe", "read_only")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(config), &doc); err != nil {
		t.Fatal(err)
	}
	if nested(doc, "spec", "mode") != "Replica" {
		t.Fatal("read replica must boot in Neon replica mode")
	}
	settings := nested(doc, "spec", "cluster", "settings").([]any)
	found := map[string]bool{}
	for _, raw := range settings {
		setting := raw.(map[string]any)
		if setting["name"] == "primary_conninfo" || setting["name"] == "primary_slot_name" {
			found[setting["name"].(string)] = true
		}
	}
	if !found["primary_conninfo"] || !found["primary_slot_name"] {
		t.Fatal("replica must stream WAL from Safekeepers")
	}
}

func TestCreateStepNames(t *testing.T) {
	if len(createSteps("create_project", true)) != 7 || len(createSteps("create_branch", false)) != 2 ||
		len(createSteps("create_endpoint", true)) != 5 {
		t.Fatal("create operation steps must match reconciler sequence")
	}
}

func TestVMWaitErrorIsSafeAndClassifiable(t *testing.T) {
	err := fmt.Errorf("wait_neonvm_running: %w", vmWaitError{phase: "Pending", cause: context.DeadlineExceeded})
	var wait vmWaitError
	if !errors.As(err, &wait) || wait.phase != "Pending" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("VM readiness timeout must retain safe phase and timeout classification")
	}
}

func TestCreateRequestHashUsesPersistentSecret(t *testing.T) {
	first := &server{idempotencyKey: []byte(strings.Repeat("a", 32))}
	second := &server{idempotencyKey: []byte(strings.Repeat("b", 32))}
	request := map[string]string{"name": "example", "password": "guessable-password"}
	got := first.createRequestHash("POST:/projects", request)
	if got != first.createRequestHash("POST:/projects", request) || got == second.createRequestHash("POST:/projects", request) ||
		got == requestHash("POST:/projects", request) || strings.Contains(got, request["password"]) || !strings.HasPrefix(got, "h1:") {
		t.Fatal("create idempotency hash must be stable, keyed and versioned")
	}
}
