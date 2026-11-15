package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestSuspendedEndpointDeletionRouteCAS(t *testing.T) {
	f, original, previous := managedSQLReplacementFixture(t)
	previous["state"] = json.RawMessage(`"active"`)
	f.mutate(func(objects map[string]map[string]any) {
		registry := objects[f.k.path("secret", routesSecret)]
		raw, err := json.Marshal(map[string]map[string]json.RawMessage{selector(original.EndpointID): previous})
		if err != nil {
			t.Fatal(err)
		}
		registry["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(raw)
	})
	scope := createPayload{ProjectID: original.ProjectID, EndpointID: original.EndpointID,
		BranchID: original.BranchID, TenantID: original.TenantID, TimelineID: original.TimelineID}
	proof := suspendedEndpointDeletionSource{
		dsgaResumeSource: dsgaResumeSource{VMUID: original.VMUID, ConfigUID: original.ConfigSecretUID,
			ServiceUID: original.ServiceUID, Incarnation: original.Incarnation, ComputeInstanceID: original.ComputeInstanceID},
		Scope: scope, Namespace: original.Namespace,
	}
	p := deletionPayload{ProjectID: scope.ProjectID, BranchIDs: []string{scope.BranchID}, EndpointIDs: []string{scope.EndpointID}}
	run := func(operation string, source suspendedEndpointDeletionSource) error {
		return f.k.setDeletionRoutesWithProof(context.Background(), p, operation, false, nil, source.verifyRoute)
	}
	expect := func(t *testing.T, operation string, source suspendedEndpointDeletionSource, success bool, wantWrites int) {
		t.Helper()
		err := run(operation, source)
		writes, _ := f.counts()
		if (err == nil) != success || writes != wantWrites {
			t.Fatalf("operation %s: expected success=%t and %d writes, got %d writes: %v", operation, success, wantWrites, writes, err)
		}
	}
	for name, change := range map[string]func(*suspendedEndpointDeletionSource){
		"runner incarnation": func(p *suspendedEndpointDeletionSource) { p.Incarnation = "unrelated" },
		"logical timeline":   func(p *suspendedEndpointDeletionSource) { p.Scope.TimelineID = "another-timeline" },
		"postgres Service":   func(p *suspendedEndpointDeletionSource) { p.ServiceUID = "replacement-service" },
	} {
		t.Run(name, func(t *testing.T) {
			mismatch := proof
			change(&mismatch)
			expect(t, "delete-test", mismatch, false, 0)
		})
	}
	expect(t, "delete-test", proof, true, 1)
	expect(t, "another-operation", proof, false, 1)
	expect(t, "delete-test", proof, true, 2)
}
