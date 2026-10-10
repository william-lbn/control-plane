package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReplacementWriterPreservesBranchCredentials(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		foreign, wrongPassword bool
	}{
		{name: "preserve_role_and_probe"}, {name: "foreign_retained_secret", foreign: true}, {name: "password_is_not_rotated", wrongPassword: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			password := "existing-branch-password"
			adminVerifier, err := scramVerifier(password)
			if err != nil {
				t.Fatal(err)
			}
			probeVerifier, err := scramVerifier("existing-probe-password")
			if err != nil {
				t.Fatal(err)
			}
			encoded := func(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) }
			config, _ := json.Marshal(record{"spec": record{"cluster": record{"roles": []any{record{"name": "cloud_admin", "encrypted_password": adminVerifier}, record{"name": "control_probe", "encrypted_password": probeVerifier}}}}})
			writes := 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes++
					var secret record
					_ = json.NewDecoder(r.Body).Decode(&secret)
					if !owned(secret, "prj_test", "ep_new") || nested(secret, "data", "adminVerifier") != encoded(adminVerifier) || nested(secret, "data", "probeVerifier") != encoded(probeVerifier) || nested(secret, "data", "controlSigningSeed") != nil {
						t.Error("branch credential changed or endpoint signing identity copied")
					}
					jsonResponse(w, 201, secret)
					return
				}
				project := "prj_test"
				if tc.foreign {
					project = "foreign"
				}
				data := record{"adminVerifier": encoded(adminVerifier), "probePassword": encoded("existing-probe-password"), "probeVerifier": encoded(probeVerifier)}
				if strings.HasSuffix(r.URL.Path, "-config") {
					data = record{"config.json": encoded(string(config))}
				}
				jsonResponse(w, 200, record{"metadata": record{"labels": credentialLabels(project, "ep_old")}, "data": data})
			}))
			defer peer.Close()
			k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
			if tc.wrongPassword {
				password = "different-branch-password"
			}
			err = k.reserveRetainedWriterCredentials(context.Background(), createPayload{ProjectID: "prj_test", BranchID: "br_test", EndpointID: "ep_new"}, "ep_old", password)
			if tc.wrongPassword && !errors.Is(err, errBranchPasswordMismatch) || tc.foreign && err == nil || (tc.foreign || tc.wrongPassword) && writes != 0 || !tc.foreign && !tc.wrongPassword && (err != nil || writes != 1) {
				t.Fatal("unsafe replacement credential result", err, writes)
			}
		})
	}
}

func TestEndpointRunnerRetirementRequiresNormalGarbageCollection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		foreign bool
		gone    bool
	}{
		{name: "normal_collection", gone: true},
		{name: "foreign_pod", foreign: true},
		{name: "terminal_pod_is_not_collection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				if r.URL.Query().Get("labelSelector") != "vm.neon.tech/name=cp-test" {
					t.Error("runner observation scope differs from endpoint")
				}
				items := []any{}
				if !tc.gone {
					project := "prj_test"
					if tc.foreign {
						project = "foreign"
					}
					labels := credentialLabels(project, "ep_test")
					labels["vm.neon.tech/name"] = "cp-test"
					items = append(items, record{"metadata": record{"uid": "old-pod", "labels": labels}, "status": record{"phase": "Succeeded"}})
				}
				jsonResponse(w, 200, record{"items": items})
			}))
			defer peer.Close()
			s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := s.observeDeletedEndpointRunners(ctx, endpointDeletionPayload{ProjectID: "prj_test", BranchID: "br_test", EndpointID: "ep_test"})
			if writes != 0 {
				t.Fatal("observer mutated or forcefully deleted a Pod")
			}
			if tc.gone && err != nil || tc.foreign && err == nil || !tc.gone && !tc.foreign && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("incorrect retirement conclusion", err)
			}
		})
	}
}
