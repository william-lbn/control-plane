package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

// 从真实createCompute/waitCompute/publishRoute入口观察API调用，而非只断言
// 辅助函数返回值。所有拒绝应发生在读取私钥或写入任何对象之前；HTTP夹具
// 不宣称具有真实apiserver的CAS、RBAC或CRD准入能力。
func TestLegacyComputeDriverRejectsDSGAWithoutCredentialReadOrWrite(t *testing.T) {
	for _, scenario := range []string{"held", "ready", "null_bootstrap", "empty_mode", "bad_journal", "bad_annotations", "dynamic_config", "null_dynamic_config", "bad_config", "foreign_vm", "foreign_config", "vm_api_error", "config_api_error"} {
		t.Run(scenario, func(t *testing.T) {
			p := createPayload{ProjectID: "prj_test", EndpointID: "ep_test"}
			metadata := func() map[string]any {
				return map[string]any{"name": kubeName(p.EndpointID), "labels": credentialLabels(p.ProjectID, p.EndpointID)}
			}
			vm := map[string]any{"metadata": metadata(), "spec": map[string]any{}, "status": map[string]any{"phase": "Running"}}
			config := map[string]any{"metadata": metadata(), "data": map[string]string{"config.json": base64.StdEncoding.EncodeToString([]byte(`{"spec":{"cluster":{}}}`))}}
			vmStatus, configStatus := http.StatusOK, http.StatusOK
			switch scenario {
			case "held", "ready":
				vm["spec"].(map[string]any)["dsgaBootstrap"] = map[string]any{"ready": scenario == "ready"}
			case "null_bootstrap":
				vm["spec"].(map[string]any)["dsgaBootstrap"] = nil
			case "empty_mode":
				vm["metadata"].(map[string]any)["annotations"] = map[string]string{dsgapi.ModeAnnotation: ""}
			case "bad_journal":
				vm["metadata"].(map[string]any)["annotations"] = map[string]string{dsgapi.JournalAnnotation: "bad"}
			case "bad_annotations":
				vm["metadata"].(map[string]any)["annotations"] = "not-an-object"
			case "dynamic_config", "null_dynamic_config", "bad_config":
				text := `{"spec":{"dynamic_shared_buffers":{"protocol_version":2}}}`
				if scenario == "null_dynamic_config" {
					text = `{"spec":{"dynamic_shared_buffers":null}}`
				} else if scenario == "bad_config" {
					text = `{"spec":`
				}
				config["data"].(map[string]string)["config.json"] = base64.StdEncoding.EncodeToString([]byte(text))
			case "foreign_vm":
				vm["metadata"].(map[string]any)["labels"] = map[string]string{}
			case "foreign_config":
				config["metadata"].(map[string]any)["labels"] = map[string]string{}
			case "vm_api_error":
				vmStatus = http.StatusServiceUnavailable
			case "config_api_error":
				configStatus = http.StatusForbidden
			}
			var otherCalls atomic.Int32
			k := &kubeClient{namespace: "testing"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == k.path("vm", kubeName(p.EndpointID)) {
					w.WriteHeader(vmStatus)
					_ = json.NewEncoder(w).Encode(vm)
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == k.path("secret", kubeName(p.EndpointID)+"-config") {
					w.WriteHeader(configStatus)
					_ = json.NewEncoder(w).Encode(config)
					return
				}
				otherCalls.Add(1)
				http.Error(w, "unexpected credential read or mutation", http.StatusInternalServerError)
			}))
			defer server.Close()
			k.base, k.http = server.URL, server.Client()
			result, err := k.createCompute(context.Background(), p)
			if err == nil || result != nil || otherCalls.Load() != 0 {
				t.Fatalf("legacy creation reached credentials or writes for %s: calls=%d err=%v", scenario, otherCalls.Load(), err)
			}
			if scenario == "held" || scenario == "ready" || scenario == "empty_mode" || scenario == "bad_journal" || scenario == "null_bootstrap" || scenario == "bad_annotations" {
				if _, err := k.waitCompute(context.Background(), p.ProjectID, p.EndpointID); !errors.Is(err, errDSGALegacyDriver) {
					t.Fatalf("legacy readiness adopted DSGA VM: %v", err)
				}
				if err := k.publishRoute(context.Background(), p, vm, map[string]string{"cloud_admin": "verifier"}); !errors.Is(err, errDSGALegacyDriver) || otherCalls.Load() != 0 {
					t.Fatalf("legacy route published DSGA management role: calls=%d err=%v", otherCalls.Load(), err)
				}
			}
		})
	}
}

func TestLegacyComputePreflightAllowsAbsentAndStaticObjects(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			p := createPayload{ProjectID: "prj_test", EndpointID: "ep_test"}
			k := &kubeClient{namespace: "testing"}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet {
					t.Error("preflight mutated an object")
				}
				if !existing {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				item := map[string]any{"metadata": map[string]any{"labels": credentialLabels(p.ProjectID, p.EndpointID)}}
				if r.URL.Path == k.path("secret", kubeName(p.EndpointID)+"-config") {
					item["data"] = map[string]string{"config.json": base64.StdEncoding.EncodeToString([]byte(`{"spec":{"cluster":{"settings":[]}}}`))}
				} else if r.URL.Path != k.path("vm", kubeName(p.EndpointID)) {
					t.Error("unexpected object read")
				}
				_ = json.NewEncoder(w).Encode(item)
			}))
			defer server.Close()
			k.base, k.http = server.URL, server.Client()
			if err := k.legacyComputePreflight(context.Background(), p); err != nil || calls.Load() != 2 {
				t.Fatalf("legacy static/new compute rejected: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
