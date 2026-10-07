package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Reproduce the missed-404 boundary: the same VM name has already cold-woken
// with a new UID. Only GET observation is allowed; the successor must survive.
func TestSuspensionObservesDeletedGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, uid, project string
		status             int
		wantError          bool
	}{
		{"not_found", "", "project", 404, false},
		{"owned_cold_wake", "successor", "project", 200, false},
		{"foreign_successor", "successor", "foreign", 200, true},
		{"missing_uid", "", "project", 200, true},
		{"old_generation_remains", "deleted", "project", 200, true},
		{"storage_read_failure", "", "project", 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("successor mutated: %s", r.Method)
				}
				jsonResponse(w, tc.status, record{"metadata": record{"uid": tc.uid,
					"labels": record{"neon-control/project-id": tc.project, "neon-control/endpoint-id": "endpoint"}}})
			}))
			defer peer.Close()
			k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
			err := k.waitVMGenerationDeletion(context.Background(), suspendPayload{ProjectID: "project", EndpointID: "endpoint", WorkloadName: "owned-vm"}, "deleted", 30*time.Millisecond, time.Millisecond)
			if (err != nil) != tc.wantError {
				t.Fatalf("deleted generation observation: %v", err)
			}
		})
	}
}
