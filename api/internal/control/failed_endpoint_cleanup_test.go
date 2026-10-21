package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFailedEndpointUnpublishedRouteIsExplicitlyScoped(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		allow, present, foreign, recovery bool
		wantOK                            bool
	}{
		{name: "normal_missing_route_rejected"},
		{name: "verified_unpublished_route", allow: true, wantOK: true},
		{name: "foreign_published_route_rejected", allow: true, present: true, foreign: true},
		{name: "original_published_route_closed", allow: true, present: true, wantOK: true},
		{name: "absence_never_authorizes_recovery", allow: true, recovery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					writes.Add(1)
					var secret record
					_ = json.NewDecoder(r.Body).Decode(&secret)
					text, _ := secretText(secret, "routes.json")
					var result map[string]record
					_ = json.Unmarshal([]byte(text), &result)
					if result[selector("ep_failed")]["state"] != "deleted" || result["other"]["state"] != "active" {
						t.Error("route closure escaped the exact failed endpoint")
					}
					jsonResponse(w, 200, secret)
					return
				}
				routes := map[string]record{"other": {"project_id": "foreign", "branch_id": "foreign", "state": "active"}}
				if tc.present {
					project := "prj_test"
					if tc.foreign {
						project = "foreign"
					}
					routes[selector("ep_failed")] = record{"project_id": project, "branch_id": "br_test", "state": "active"}
				}
				data, _ := json.Marshal(routes)
				jsonResponse(w, 200, record{"metadata": record{"resourceVersion": "42"}, "data": record{"routes.json": base64.StdEncoding.EncodeToString(data)}})
			}))
			defer peer.Close()
			k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
			var allowed map[string]bool
			if tc.allow {
				allowed = map[string]bool{selector("ep_failed"): true}
			}
			err := k.setDeletionRoutesAllowUnpublished(context.Background(), deletionPayload{ProjectID: "prj_test", BranchIDs: []string{"br_test"}, EndpointIDs: []string{"ep_failed"}}, "op_cleanup", tc.recovery, allowed)
			if (err == nil) != tc.wantOK {
				t.Fatal("incorrect missing-route authority", err)
			}
			wantWrites := int32(0)
			if tc.wantOK && tc.present {
				wantWrites = 1
			}
			if writes.Load() != wantWrites {
				t.Fatal("unexpected route write", writes.Load())
			}
		})
	}
}
