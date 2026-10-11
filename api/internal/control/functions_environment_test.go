package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestFunctionEnvironmentImmutableRecoveryAndOwnership(t *testing.T) {
	key := bytes.Repeat([]byte{17}, 32)
	environment := map[string]string{"SECRET_TOKEN": "disposable-ci-private-value", "GREETING": "hello"}
	var mu sync.Mutex
	var stored map[string]any
	var posts int
	var denied bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/neon-ci/secrets") || r.URL.RawQuery != "" {
			w.WriteHeader(400)
			return
		}
		if r.Method == "GET" {
			if denied {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"message":"denied"}`))
				return
			}
			if stored == nil {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"message":"absent"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(stored)
			return
		}
		if r.Method != "POST" || stored != nil {
			w.WriteHeader(409)
			return
		}
		posts++
		if json.NewDecoder(r.Body).Decode(&stored) != nil {
			t.Error("invalid Secret creation")
		}
		stored["metadata"].(map[string]any)["uid"] = "11111111-2222-3333-4444-555555555555"
		// The server commits, then the reply disappears. Only a GET may resolve
		// the unknown outcome; a blind POST replay would duplicate a mutation.
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}
	ref, err := k.reserveFunctionEnvironment(context.Background(), key, "prj_0123456789abcdef", "br_0123456789abcdef", "fdp_0123456789abcdef", environment)
	if err != nil || ref.UID == "" {
		t.Fatal("lost reply did not recover an actual matching Secret")
	}
	restarted := &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}
	replay, err := restarted.reserveFunctionEnvironment(context.Background(), key, ref.ProjectID, ref.BranchID, ref.DeploymentID, environment)
	if err != nil || replay != ref {
		t.Fatal("restart did not reuse exact immutable backing")
	}
	values, err := restarted.readFunctionEnvironment(context.Background(), key, ref)
	if err != nil || !reflect.DeepEqual(values, environment) {
		t.Fatal("original canonical values unavailable")
	}
	values["GREETING"] = "detached"
	values, err = restarted.readFunctionEnvironment(context.Background(), key, ref)
	if err != nil || values["GREETING"] != "hello" {
		t.Fatal("caller altered immutable backing")
	}
	serialized, _ := json.Marshal(ref)
	if string(serialized) != "{}" {
		t.Fatal("private backing reference entered public JSON")
	}
	if _, err = k.reserveFunctionEnvironment(context.Background(), key, ref.ProjectID, ref.BranchID, ref.DeploymentID, map[string]string{"GREETING": "other"}); err == nil {
		t.Fatal("existing deployment Secret replaced or adopted with changed content")
	}

	mu.Lock()
	original, _ := json.Marshal(stored)
	mu.Unlock()
	for name, change := range map[string]func(map[string]any){
		"uid":       func(v map[string]any) { v["metadata"].(map[string]any)["uid"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" },
		"namespace": func(v map[string]any) { v["metadata"].(map[string]any)["namespace"] = "another" },
		"branch": func(v map[string]any) {
			v["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/branch-id"] = "br_ffffffffffffffff"
		},
		"mutable":     func(v map[string]any) { v["immutable"] = false },
		"terminating": func(v map[string]any) { v["metadata"].(map[string]any)["deletionTimestamp"] = "2026-01-01T00:00:00Z" },
		"extra-key":   func(v map[string]any) { v["data"].(map[string]any)["other"] = "eA==" },
		"value": func(v map[string]any) {
			v["data"].(map[string]any)["environment.json"] = base64.StdEncoding.EncodeToString([]byte(`{"GREETING":"changed","SECRET_TOKEN":"disposable-ci-private-value"}`))
		},
		"duplicate-json": func(v map[string]any) {
			v["data"].(map[string]any)["environment.json"] = base64.StdEncoding.EncodeToString([]byte(`{"GREETING":"changed","GREETING":"hello","SECRET_TOKEN":"disposable-ci-private-value"}`))
		},
		"mac": func(v map[string]any) {
			v["metadata"].(map[string]any)["annotations"].(map[string]any)["neon-control/environment-mac"] = strings.Repeat("a", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			_ = json.Unmarshal(original, &stored)
			change(stored)
			mu.Unlock()
			if _, err := k.readFunctionEnvironment(context.Background(), key, ref); err != errFunctionEnvironment {
				t.Fatal("changed backing accepted or secret error text leaked")
			}
		})
	}
	mu.Lock()
	_ = json.Unmarshal(original, &stored)
	denied = true
	mu.Unlock()
	if _, err = k.reserveFunctionEnvironment(context.Background(), key, ref.ProjectID, ref.BranchID, ref.DeploymentID, environment); err != errFunctionEnvironment {
		t.Fatal("authorization read failure admitted a create")
	}
	if _, err = k.reserveFunctionEnvironment(context.Background(), key[:8], ref.ProjectID, ref.BranchID, ref.DeploymentID, environment); err == nil {
		t.Fatal("weak fingerprint key admitted")
	}
	if _, err = k.reserveFunctionEnvironment(context.Background(), key, ref.ProjectID, ref.BranchID, ref.DeploymentID, nil); err == nil {
		t.Fatal("null environment admitted")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatal("unknown outcome or changed content repeated a mutation")
	}
}
