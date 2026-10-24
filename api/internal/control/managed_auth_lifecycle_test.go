package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestManagedAuthRetainedDeletionClosesIdentityAndDoesNotRestoreIt(t *testing.T) {
	roles := record{managedAuthLogin("br_lifecycle"): "auth-verifier", dataAPILogin("br_lifecycle"): "data-verifier", "customer": "retained-verifier"}
	routes := map[string]record{selector("ep_lifecycle"): {"project_id": "prj_lifecycle", "branch_id": "br_lifecycle", "state": "active", "roles": roles}}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var secret record
			if err := json.NewDecoder(r.Body).Decode(&secret); err != nil {
				t.Fatal(err)
			}
			if nested(secret, "metadata", "resourceVersion") != "41" {
				t.Error("Route publication lost its resource version")
			}
			raw, err := secretText(secret, "routes.json")
			if err != nil || json.Unmarshal([]byte(raw), &routes) != nil {
				t.Error("Invalid route update")
			}
		}
		raw, _ := json.Marshal(routes)
		jsonResponse(w, 200, record{"metadata": record{"resourceVersion": "41"}, "data": record{"routes.json": base64.StdEncoding.EncodeToString(raw)}})
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	p := deletionPayload{ProjectID: "prj_lifecycle", BranchIDs: []string{"br_lifecycle"}, EndpointIDs: []string{"ep_lifecycle"}}
	if err := k.setDeletionRoutes(context.Background(), p, "op_delete", false); err != nil {
		t.Fatal(err)
	}
	if routes[selector("ep_lifecycle")]["state"] != "deleted" {
		t.Fatal("Deleted Auth admission remained open")
	}
	p.TombstoneID = "op_delete"
	if err := k.setDeletionRoutes(context.Background(), p, "op_restore", true); err != nil {
		t.Fatal(err)
	}
	for _, replay := range []bool{false, true} {
		if replay {
			if err := k.setDeletionRoutes(context.Background(), p, "op_restore", true); err != nil {
				t.Fatal(err)
			}
		}
		route := routes[selector("ep_lifecycle")]
		got := route["roles"].(map[string]any)
		if route["state"] != "suspended" || got[managedAuthLogin("br_lifecycle")] != nil || got[dataAPILogin("br_lifecycle")] != nil || got["customer"] != "retained-verifier" {
			t.Fatal("Recovery revived a service identity or lost customer access")
		}
	}
}

func TestManagedAuthDeletionRetirementPreservesConfigurationAndRejectsForeignWorkload(t *testing.T) {
	for _, scenario := range []string{"owned", "already_absent", "foreign_project", "foreign_branch", "foreign_endpoint", "unknown_write"} {
		t.Run(scenario, func(t *testing.T) {
			p := managedAuthPayload{ProjectID: "prj_lifecycle", BranchID: "br_lifecycle", EndpointID: "ep_lifecycle"}
			labels := managedAuthLabels(p)
			switch scenario {
			case "foreign_project":
				labels["neon-control/project-id"] = "prj_foreign"
			case "foreign_branch":
				labels["neon-control/branch-id"] = "br_foreign"
			case "foreign_endpoint":
				labels["neon-control/endpoint-id"] = "ep_foreign"
			}
			template := record{"spec": record{"containers": []any{record{"image": "retained-image"}}, "volumes": []any{record{"secret": record{"secretName": "retained-g1"}}}}}
			replicas, writes := 1, 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "already_absent" {
					jsonResponse(w, 404, record{})
					return
				}
				if r.Method == http.MethodPut {
					writes++
					var body record
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if nested(body, "metadata", "resourceVersion") != "42" || nested(body, "spec", "replicas") != float64(0) {
						t.Error("Missing original version or zero-replica intent")
					}
					original, _ := json.Marshal(template)
					updated, _ := json.Marshal(nested(body, "spec", "template"))
					if !reflect.DeepEqual(original, updated) {
						t.Error("Retirement changed retained configuration")
					}
					if scenario == "unknown_write" {
						jsonResponse(w, 503, record{})
						return
					}
					replicas = 0
				}
				jsonResponse(w, 200, record{"metadata": record{"uid": "retained-uid", "resourceVersion": "42", "generation": 1, "labels": labels}, "spec": record{"replicas": replicas, "template": template}, "status": record{"observedGeneration": 1, "replicas": replicas}})
			}))
			defer peer.Close()
			s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
			err := s.retireDeletionManagedAuth(context.Background(), p)
			success := scenario == "owned" || scenario == "already_absent"
			if success != (err == nil) || (scenario == "owned" || scenario == "unknown_write") && writes != 1 || !success && scenario != "unknown_write" && writes != 0 {
				t.Fatalf("Unexpected ownership/write result: success=%t writes=%d error=%v", success, writes, err)
			}
		})
	}
}
