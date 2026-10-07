package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeletionRoutesCloseAndRecoveryDoesNotReviveDataAPIIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		foreign, conflict, unknown bool
	}{
		{name: "retained_owned_route"}, {name: "foreign_project", foreign: true}, {name: "resource_version_contention", conflict: true}, {name: "unknown_write_outcome", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := "prj_test"
			if tc.foreign {
				project = "foreign"
			}
			route := record{"project_id": project, "branch_id": "br_test", "state": "active", "roles": record{dataAPILogin("br_test"): "secret-verifier", "app": "app-verifier"}}
			routes := map[string]record{selector("ep_test"): route}
			writes := 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					b, _ := json.Marshal(routes)
					jsonResponse(w, 200, record{"metadata": record{"resourceVersion": "1"}, "data": record{"routes.json": base64.StdEncoding.EncodeToString(b)}})
					return
				}
				writes++
				if tc.conflict && writes == 1 {
					jsonResponse(w, 409, record{})
					return
				}
				if tc.unknown {
					jsonResponse(w, 503, record{})
					return
				}
				var secret record
				_ = json.NewDecoder(r.Body).Decode(&secret)
				raw, e := secretText(secret, "routes.json")
				if e != nil {
					t.Error(e)
				}
				_ = json.Unmarshal([]byte(raw), &routes)
				jsonResponse(w, 200, secret)
			}))
			defer peer.Close()
			k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
			p := deletionPayload{ProjectID: "prj_test", BranchIDs: []string{"br_test"}, EndpointIDs: []string{"ep_test"}}
			err := k.setDeletionRoutes(context.Background(), p, "op_delete", false)
			if tc.foreign || tc.unknown {
				if err == nil {
					t.Fatal("unsafe write accepted")
				}
				if tc.foreign && writes != 0 || tc.unknown && writes != 1 {
					t.Fatal("foreign or unknown write replayed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if routes[selector("ep_test")]["state"] != "deleted" {
				t.Fatal("admission remained open")
			}
			roles := routes[selector("ep_test")]["roles"].(map[string]any)
			if roles[dataAPILogin("br_test")] != nil || roles["app"] == nil {
				t.Fatal("Data API identity revived or application identity lost")
			}
			p.TombstoneID = "op_delete"
			if e := k.setDeletionRoutes(context.Background(), p, "op_recover", true); e != nil {
				t.Fatal(e)
			}
			if routes[selector("ep_test")]["state"] != "suspended" {
				t.Fatal("recovery must be cold")
			}
			if e := k.setDeletionRoutes(context.Background(), p, "op_recover", true); e != nil {
				t.Fatal("recovery replay failed", e)
			}
		})
	}
}

func TestDeleteComputeRejectsForeignVMAndUsesUIDAndVersionPreconditions(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		t.Run(map[bool]string{true: "foreign", false: "owned"}[foreign], func(t *testing.T) {
			project := "prj_test"
			if foreign {
				project = "foreign"
			}
			deleted := false
			writes := 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					if deleted {
						jsonResponse(w, 404, record{})
						return
					}
					jsonResponse(w, 200, record{"metadata": record{"uid": "old-uid", "resourceVersion": "42", "labels": record{"neon-control/project-id": project, "neon-control/endpoint-id": "ep_test"}}})
					return
				}
				writes++
				var body record
				_ = json.NewDecoder(r.Body).Decode(&body)
				v := body["preconditions"].(map[string]any)
				if v["uid"] != "old-uid" || v["resourceVersion"] != "42" {
					t.Error("missing UID/version precondition")
				}
				deleted = true
				jsonResponse(w, 200, record{})
			}))
			defer peer.Close()
			s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
			err := s.retireDeletedCompute(context.Background(), deletionPayload{ProjectID: "prj_test", EndpointIDs: []string{"ep_test"}})
			if foreign {
				if err == nil || writes != 0 {
					t.Fatal("foreign VM changed")
				}
			} else if err != nil || writes != 1 {
				t.Fatal("owned VM deletion did not complete", err)
			}
		})
	}
}

func TestDeletionDataAPIRetirementPreservesSecretAndTemplate(t *testing.T) {
	replicas := 1
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		item := record{"metadata": record{"resourceVersion": "17", "generation": 1, "labels": record{"neon-control/project-id": "prj_test", "neon-control/endpoint-id": "ep_test", "neon-control/branch-id": "br_test"}}, "spec": record{"replicas": replicas, "template": record{"spec": record{"volumes": []any{record{"secret": record{"secretName": "retained-secret"}}}}}}, "status": record{"observedGeneration": 1, "replicas": replicas}}
		if r.Method == http.MethodPut {
			var body record
			_ = json.NewDecoder(r.Body).Decode(&body)
			if nested(body, "metadata", "resourceVersion") != "17" || nested(body, "spec", "replicas") != float64(0) {
				t.Error("missing CAS or zero replica intent")
			}
			if nested(body, "spec", "template") == nil {
				t.Error("retained Pod template discarded")
			}
			replicas = 0
		}
		jsonResponse(w, 200, item)
	}))
	defer peer.Close()
	s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
	if err := s.retireDeletionDataAPI(context.Background(), dataAPIPayload{ProjectID: "prj_test", EndpointID: "ep_test", BranchID: "br_test"}); err != nil {
		t.Fatal(err)
	}
}
