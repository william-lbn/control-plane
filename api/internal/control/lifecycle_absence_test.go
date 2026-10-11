package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Exercise the actual kube HTTP observer, including normal Pod GC and a
// replacement appearing after the first VM 404. No delete/patch/wake is allowed.
func TestSuspensionAbsentVMRequiresRunnerRetirement(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		pods                         int
		vmStatus                     int
		foreign, malformed, terminal bool
		wantError                    bool
	}{
		{name: "already_absent", vmStatus: 404},
		{name: "normal_runner_gc", pods: 1, vmStatus: 404},
		{name: "owned_successor_preserved", vmStatus: 200, wantError: true},
		{name: "vm_read_unavailable", vmStatus: 503, wantError: true},
		{name: "runner_read_unavailable", vmStatus: 404, malformed: true, wantError: true},
		{name: "foreign_runner_rejected", pods: 1, vmStatus: 404, foreign: true, wantError: true},
		{name: "terminal_runner_still_present", pods: 1, vmStatus: 404, terminal: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("absence recovery mutated resource: %s", r.Method)
					w.WriteHeader(400)
					return
				}
				if r.URL.Path == "/api/v1/namespaces/neon/pods" {
					if r.URL.Query().Get("labelSelector") != "vm.neon.tech/name=cp-0123456789abcdef" {
						t.Error("unbounded Runner query")
					}
					reads++
					if tc.malformed {
						jsonResponse(w, 503, record{})
						return
					}
					items := []any{}
					if tc.pods > 0 && (reads == 1 || tc.terminal) {
						project := "project"
						if tc.foreign {
							project = "foreign"
						}
						items = append(items, record{"metadata": record{"labels": record{"neon-control/project-id": project, "neon-control/endpoint-id": "endpoint", "vm.neon.tech/name": "cp-0123456789abcdef"}}, "status": record{"phase": "Succeeded"}})
					}
					jsonResponse(w, 200, record{"items": items})
					return
				}
				jsonResponse(w, tc.vmStatus, record{"metadata": record{"uid": "successor"}})
			}))
			defer peer.Close()
			k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
			err := k.observeSuspendedComputeAbsence(context.Background(), suspendPayload{ProjectID: "project", EndpointID: "endpoint", WorkloadName: "cp-0123456789abcdef"}, 40*time.Millisecond, time.Millisecond)
			if (err != nil) != tc.wantError {
				t.Fatalf("absence boundary: %v", err)
			}
			if tc.name == "normal_runner_gc" && reads < 2 {
				t.Fatal("Runner absence not observed")
			}
		})
	}
}
