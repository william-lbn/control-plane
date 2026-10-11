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

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

func functionSQLSecretFixture(t *testing.T) (functionSQLReference, functionSQLCredential, []byte) {
	t.Helper()
	ref := functionSQLReference{Namespace: "neon-ci", Name: "fn-0123456789abcdef-sql-1", WriterID: "ep_0123456789abcdef",
		Generation: 1, MACKeyVersion: "ci-v1", Scope: functionsql.Scope{FunctionID: "fnc_0123456789abcdef",
			ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", Database: "neondb", Schema: "app", ProvisioningRole: "control_probe"}}
	c := functionSQLCredential{Password: strings.Repeat("p", 48)}
	var err error
	c.Verifier, err = scramVerifier(c.Password)
	if err != nil {
		t.Fatal(err)
	}
	return ref, c, bytes.Repeat([]byte{42}, 32)
}

func TestFunctionSQLSecretUnknownCreateReplyAndOriginalRecovery(t *testing.T) {
	ref, c, key := functionSQLSecretFixture(t)
	var mu sync.Mutex
	var stored map[string]any
	posts := 0
	denied := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
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
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/neon-ci/secrets" {
			w.WriteHeader(400)
			return
		}
		posts++
		if posts > 1 {
			t.Error("unexpected second creation mutation")
		}
		if json.NewDecoder(r.Body).Decode(&stored) != nil {
			w.WriteHeader(400)
			return
		}
		stored["metadata"].(map[string]any)["uid"] = "11111111-2222-3333-4444-555555555555"
		// The server committed the object, but the original caller saw a failure.
		w.WriteHeader(503)
	}))
	defer peer.Close()
	postCount := func() int { mu.Lock(); defer mu.Unlock(); return posts }
	k := &kubeClient{base: peer.URL, namespace: ref.Namespace, http: peer.Client()}
	original, err := k.reserveFunctionSQLSecret(context.Background(), key, ref, c)
	if err != nil || original.Reference.UID == "" || postCount() != 1 {
		t.Fatal("unknown create result was not resolved")
	}
	if _, err = k.reserveFunctionSQLSecret(context.Background(), key, ref, c); err != nil || postCount() != 1 {
		t.Fatal("same proposal was not idempotent")
	}
	changed := c
	changed.Password = strings.Repeat("q", 48)
	if _, err = k.reserveFunctionSQLSecret(context.Background(), key, ref, changed); err == nil || postCount() != 1 {
		t.Fatal("different same-name credential adopted/replaced")
	}
	// Explicit read-only original-result recovery may recover the lost private
	// credential; a persisted receipt must subsequently use its exact UID/MAC.
	restarted := &kubeClient{base: peer.URL, namespace: ref.Namespace, http: peer.Client()}
	recovered, err := restarted.findFunctionSQLSecret(context.Background(), key, ref)
	if err != nil || recovered != original || postCount() != 1 {
		t.Fatal("original credential identity not recovered")
	}
	if _, err = restarted.readFunctionSQLSecret(context.Background(), key, original.Reference); err != nil {
		t.Fatal("persisted original receipt rejected")
	}
	if _, err = restarted.readFunctionSQLSecret(context.Background(), key, ref); err == nil {
		t.Fatal("missing durable identity treated as a receipt")
	}
	mu.Lock()
	stored["metadata"].(map[string]any)["uid"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	mu.Unlock()
	if _, err = restarted.readFunctionSQLSecret(context.Background(), key, original.Reference); err == nil {
		t.Fatal("replacement UID adopted")
	}
	mu.Lock()
	denied = true
	mu.Unlock()
	if _, err = restarted.reserveFunctionSQLSecret(context.Background(), key, ref, c); err == nil || postCount() != 1 {
		t.Fatal("unavailable read caused a creation mutation")
	}
	serialized, _ := json.Marshal(original)
	if string(serialized) != "{}" {
		t.Fatal("private SQL record serialized")
	}
}

func TestFunctionSQLSecretBoundScopeAndContent(t *testing.T) {
	ref, c, key := functionSQLSecretFixture(t)
	mac, data, err := functionSQLContent(key, ref, c)
	if err != nil {
		t.Fatal(err)
	}
	ref.MAC = mac
	ref.UID = "11111111-2222-3333-4444-555555555555"
	item := map[string]any{"immutable": true, "type": "Opaque", "metadata": map[string]any{"name": ref.Name, "namespace": ref.Namespace, "uid": ref.UID,
		"labels": functionSQLLabels(ref), "annotations": map[string]string{"neon-control/sql-key-version": ref.MACKeyVersion, "neon-control/sql-content-mac": mac}},
		"data": map[string]string{"sql.json": base64.StdEncoding.EncodeToString(data)}}
	clone := func() map[string]any {
		raw, _ := json.Marshal(item)
		var value map[string]any
		_ = json.Unmarshal(raw, &value)
		return value
	}
	if _, err = verifyFunctionSQLSecret(clone(), key, ref); err != nil {
		t.Fatal("valid immutable SQL credential rejected")
	}
	cases := map[string]func(map[string]any, *functionSQLReference){
		"mutable":    func(v map[string]any, r *functionSQLReference) { v["immutable"] = false },
		"wrong_type": func(v map[string]any, r *functionSQLReference) { v["type"] = "kubernetes.io/basic-auth" },
		"deleting": func(v map[string]any, r *functionSQLReference) {
			v["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-11T00:00:00Z"
		},
		"replaced_uid": func(v map[string]any, r *functionSQLReference) {
			v["metadata"].(map[string]any)["uid"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		},
		"foreign_namespace":   func(v map[string]any, r *functionSQLReference) { r.Namespace = "foreign" },
		"foreign_project":     func(v map[string]any, r *functionSQLReference) { r.Scope.ProjectID = "prj_ffffffffffffffff" },
		"foreign_branch":      func(v map[string]any, r *functionSQLReference) { r.Scope.BranchID = "br_ffffffffffffffff" },
		"foreign_function":    func(v map[string]any, r *functionSQLReference) { r.Scope.FunctionID = "fnc_ffffffffffffffff" },
		"foreign_writer":      func(v map[string]any, r *functionSQLReference) { r.WriterID = "ep_ffffffffffffffff" },
		"foreign_database":    func(v map[string]any, r *functionSQLReference) { r.Scope.Database = "foreign" },
		"foreign_schema":      func(v map[string]any, r *functionSQLReference) { r.Scope.Schema = "foreign" },
		"foreign_provisioner": func(v map[string]any, r *functionSQLReference) { r.Scope.ProvisioningRole = "foreign" },
		"no_provisioner":      func(v map[string]any, r *functionSQLReference) { r.Scope.ProvisioningRole = "" },
		"foreign_key_version": func(v map[string]any, r *functionSQLReference) { r.MACKeyVersion = "ci-v2" },
		"foreign_generation": func(v map[string]any, r *functionSQLReference) {
			r.Generation = 2
			r.Name = "fn-0123456789abcdef-sql-2"
		},
		"wrong_annotation": func(v map[string]any, r *functionSQLReference) {
			v["metadata"].(map[string]any)["annotations"].(map[string]any)["neon-control/sql-content-mac"] = strings.Repeat("a", 64)
		},
		"extra_secret_key": func(v map[string]any, r *functionSQLReference) { v["data"].(map[string]any)["other"] = "c2VjcmV0" },
		"noncanonical_json": func(v map[string]any, r *functionSQLReference) {
			v["data"].(map[string]any)["sql.json"] = base64.StdEncoding.EncodeToString(append([]byte(" "), data...))
		},
		"credential_tamper": func(v map[string]any, r *functionSQLReference) {
			changed := c
			changed.Password = strings.Repeat("q", 48)
			raw, _ := json.Marshal(changed)
			v["data"].(map[string]any)["sql.json"] = base64.StdEncoding.EncodeToString(raw)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			v := clone()
			r := ref
			change(v, &r)
			if _, err := verifyFunctionSQLSecret(v, key, r); err == nil {
				t.Fatal("foreign credential contract admitted")
			} else if strings.Contains(err.Error(), c.Password) || strings.Contains(err.Error(), c.Verifier) {
				t.Fatal("credential leaked into error")
			}
		})
	}
	t.Run("wrong_mac_key", func(t *testing.T) {
		if _, err := verifyFunctionSQLSecret(clone(), bytes.Repeat([]byte{43}, 32), ref); err == nil {
			t.Fatal("foreign key admitted")
		}
	})
	t.Run("short_mac_key", func(t *testing.T) {
		if _, err := verifyFunctionSQLSecret(clone(), key[:31], ref); err == nil {
			t.Fatal("short key admitted")
		}
	})
}
