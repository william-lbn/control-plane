package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func functionRuntimeFixture(t *testing.T, requested ...functions.Scope) (functionRuntimePlan, []byte, map[string]any) {
	t.Helper()
	ref, config := functionBootstrapFixture(t, requested...)
	key := bytes.Repeat([]byte{31}, 32)
	mac, data, err := functionBootstrapContent(key, ref, config)
	if err != nil {
		t.Fatal(err)
	}
	ref.UID = "11111111-2222-3333-4444-555555555555"
	ref.MAC = mac
	secret := map[string]any{"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "Opaque",
		"metadata": map[string]any{"name": ref.Name, "namespace": ref.Namespace, "uid": ref.UID, "labels": functionBootstrapLabels(ref), "annotations": map[string]string{"neon-control/bootstrap-key-version": ref.MACKeyVersion, "neon-control/bootstrap-mac": mac}},
		"data":     map[string]any{"config.json": base64.StdEncoding.EncodeToString(data)}}
	name := "fn-" + strings.TrimPrefix(ref.Scope.InstanceID, "fni_")
	plan := functionRuntimePlan{Target: functionInstanceTarget{ref.FunctionID, ref.Scope.ProjectID, ref.Scope.BranchID, ref.Scope.DeploymentID, ref.Scope.Generation, ref.Scope.Slug},
		Instance: functionInstanceRecord{Scope: ref.Scope, VMName: name, ServiceName: name, SecretName: ref.Name, State: "provisioning"}, Bootstrap: ref,
		Image: "docker.io/example/functions-vm@sha256:" + strings.Repeat("a", 64), NodeName: "neon-ci-node"}
	return plan, key, secret
}

func TestFunctionRuntimeLostReplyRestartAndNoReplacement(t *testing.T) {
	for _, kind := range []string{"service", "vm"} {
		t.Run(kind, func(t *testing.T) {
			p, key, secret := functionRuntimeFixture(t)
			var mu sync.Mutex
			var stored map[string]any
			posts := 0
			denied := false
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" && r.URL.Path == "/api/v1/namespaces/neon-ci/secrets/"+p.Bootstrap.Name {
					_ = json.NewEncoder(w).Encode(secret)
					return
				}
				expected := (&kubeClient{namespace: "neon-ci"}).path(kind, p.Instance.VMName)
				if r.Method == "GET" && r.URL.Path == expected {
					if denied {
						w.WriteHeader(503)
						return
					}
					if stored == nil {
						w.WriteHeader(404)
						return
					}
					_ = json.NewEncoder(w).Encode(stored)
					return
				}
				if r.Method != "POST" || r.URL.Path != (&kubeClient{namespace: "neon-ci"}).path(kind, "") || stored != nil {
					w.WriteHeader(409)
					return
				}
				if json.NewDecoder(r.Body).Decode(&stored) != nil {
					t.Error("invalid runtime create")
				}
				posts++
				stored["metadata"].(map[string]any)["uid"] = "22222222-3333-4444-5555-666666666666"
				if kind == "service" {
					stored["spec"].(map[string]any)["clusterIP"] = "10.43.12.34"
				}
				// Successful API-side creation with an unreadable acknowledgement.
				_, _ = w.Write([]byte(`{"truncated":`))
			}))
			defer peer.Close()
			k := &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}
			original, err := k.reserveFunctionRuntimeResource(context.Background(), key, p, kind)
			if err != nil || original.UID == "" || posts != 1 {
				t.Fatal("lost create did not recover original identity", err)
			}
			if kind == "service" {
				p.Instance.ServiceUID = &original.UID
			} else {
				p.Instance.VMUID = &original.UID
			}
			restarted := &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}
			again, err := restarted.reserveFunctionRuntimeResource(context.Background(), key, p, kind)
			if err != nil || again != original || posts != 1 {
				t.Fatal("restart replaced original resource", err)
			}
			mu.Lock()
			stored["metadata"].(map[string]any)["uid"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			mu.Unlock()
			if _, err = restarted.reserveFunctionRuntimeResource(context.Background(), key, p, kind); err == nil {
				t.Fatal("same-name replacement adopted")
			}
			mu.Lock()
			stored = nil
			mu.Unlock()
			if _, err = restarted.reserveFunctionRuntimeResource(context.Background(), key, p, kind); err == nil || posts != 1 {
				t.Fatal("persisted resource loss authorized replacement")
			}
			p.Instance.ServiceUID = nil
			p.Instance.VMUID = nil
			mu.Lock()
			denied = true
			mu.Unlock()
			if _, err = restarted.reserveFunctionRuntimeResource(context.Background(), key, p, kind); err == nil || posts != 1 {
				t.Fatal("failed read authorized create")
			}
		})
	}
}

func TestFunctionRuntimeRejectsOverridesAndForeignOwnership(t *testing.T) {
	for _, entry := range []struct {
		name, kind string
		change     func(map[string]any)
	}{
		{"foreign_branch", "vm", func(m map[string]any) {
			m["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/branch-id"] = "br_aaaaaaaaaaaaaaaa"
		}},
		{"wrong_bootstrap_uid", "vm", func(m map[string]any) {
			m["metadata"].(map[string]any)["annotations"].(map[string]any)["neon-control/bootstrap-uid"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		}},
		{"deleting_vm", "vm", func(m map[string]any) { m["metadata"].(map[string]any)["deletionTimestamp"] = "2026-01-01T00:00:00Z" }},
		{"command_override", "vm", func(m map[string]any) {
			m["spec"].(map[string]any)["guest"].(map[string]any)["command"] = []string{"sh", "-c", "exit 0"}
		}},
		{"untrusted_env", "vm", func(m map[string]any) {
			m["spec"].(map[string]any)["guest"].(map[string]any)["env"] = []any{map[string]any{"name": "NEON_BOOTSTRAP", "value": "override"}}
		}},
		{"tagged_image", "vm", func(m map[string]any) {
			m["spec"].(map[string]any)["guest"].(map[string]any)["rootDisk"].(map[string]any)["image"] = "example/vm:latest"
		}},
		{"extra_init", "vm", func(m map[string]any) {
			m["spec"].(map[string]any)["extraInitContainers"] = []any{map[string]any{"image": "example/unsafe"}}
		}},
		{"ssh_enabled", "vm", func(m map[string]any) { m["spec"].(map[string]any)["enableSSH"] = true }},
		{"foreign_cpu_mode", "vm", func(m map[string]any) { m["spec"].(map[string]any)["cpuScalingMode"] = "SysfsScaling" }},
		{"missing_cpu_mode", "vm", func(m map[string]any) { delete(m["spec"].(map[string]any), "cpuScalingMode") }},
		{"wrong_memory", "vm", func(m map[string]any) { m["spec"].(map[string]any)["guest"].(map[string]any)["memorySlotSize"] = "4Gi" }},
		{"extra_network", "vm", func(m map[string]any) { m["spec"].(map[string]any)["extraNetwork"].(map[string]any)["enable"] = true }},
		{"wrong_selector", "service", func(m map[string]any) {
			m["spec"].(map[string]any)["selector"] = map[string]string{"vm.neon.tech/name": "another-vm"}
		}},
		{"nodeport", "service", func(m map[string]any) { m["spec"].(map[string]any)["type"] = "NodePort" }},
		{"external_service", "service", func(m map[string]any) { m["spec"].(map[string]any)["externalIPs"] = []string{"192.0.2.1"} }},
		{"ipv6_service", "service", func(m map[string]any) { m["spec"].(map[string]any)["clusterIP"] = "fd00::12" }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			p, _, _ := functionRuntimeFixture(t)
			m := functionRuntimeBody(p, entry.kind)
			m["metadata"].(map[string]any)["uid"] = "22222222-3333-4444-5555-666666666666"
			if entry.kind == "service" {
				m["spec"].(map[string]any)["clusterIP"] = "10.43.12.34"
			}
			// Match a real Kubernetes HTTP response: nested maps and numbers are
			// decoded JSON, not the typed maps used to construct a POST body.
			wire, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(wire, &m); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyFunctionRuntimeResource(m, p, entry.kind); err != nil {
				t.Fatal("valid immutable spec rejected", err)
			}
			entry.change(m)
			if _, err := verifyFunctionRuntimeResource(m, p, entry.kind); err == nil {
				t.Fatal("foreign execution spec admitted")
			}
		})
	}
}
