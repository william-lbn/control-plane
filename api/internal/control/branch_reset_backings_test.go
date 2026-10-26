package control

import (
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

func resetBackingFixture(t *testing.T) (branchResetBacking, map[string]any, map[string]any) {
	t.Helper()
	source, original, target := resetConfigFixture(t, "read_write")
	original.ConfigUID = "11111111-1111-4111-8111-111111111111"
	original.CredentialUID = "22222222-2222-4222-8222-222222222222"
	roots := map[string]string{}
	for _, raw := range nested(source, "spec", "cluster", "roles").([]any) {
		item := raw.(map[string]any)
		roots[stringVal(item["name"])] = stringVal(item["encrypted_password"])
	}
	encoded, _ := json.Marshal(source)
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	config := map[string]any{"metadata": map[string]any{"uid": original.ConfigUID, "resourceVersion": "1", "labels": resetDataAny(credentialLabels(original.Project, original.Endpoint))}, "data": map[string]any{"config.json": encode(string(encoded)), "probePassword": encode("synthetic-reset-probe-password"), "control.key": encode("synthetic-original-key"), "future-private-file": encode("synthetic-opaque-file")}}
	credential := map[string]any{"metadata": map[string]any{"uid": original.CredentialUID, "resourceVersion": "1", "labels": resetDataAny(credentialLabels(original.Project, original.Endpoint))}, "data": map[string]any{"probePassword": encode("synthetic-reset-probe-password"), "adminVerifier": encode(roots["cloud_admin"]), "probeVerifier": encode(roots["control_probe"]), "future-private-credential": encode("synthetic-kept-credential")}}
	value, err := prepareBranchResetBacking("op_"+strings.Repeat("a", 24), original, target, config, credential)
	if err != nil {
		t.Fatal(err)
	}
	return value, config, credential
}
func resetJSONCopy(value map[string]any) map[string]any {
	raw, _ := json.Marshal(value)
	var copy map[string]any
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func TestBranchResetBackingRetainsCompleteFilesAndRejectsForgedHistory(t *testing.T) {
	value, config, credential := resetBackingFixture(t)
	before, _ := json.Marshal(config)
	if err := validateBranchResetBacking(value); err != nil {
		t.Fatal(err)
	}
	for key, original := range value.ConfigData {
		if key != "config.json" && value.PreparedData[key] != original {
			t.Fatal("private file changed", key)
		}
	}
	if value.PreparedData["config.json"] == value.ConfigData["config.json"] {
		t.Fatal("target config not isolated")
	}
	after, _ := json.Marshal(config)
	if string(before) != string(after) {
		t.Fatal("source files changed")
	}
	for _, scenario := range []string{"replacement_config", "replacement_credential", "password_disagrees", "probe_disagrees", "bad_encoding", "wrong_owner", "cross_tenant", "unknown_witness"} {
		t.Run(scenario, func(t *testing.T) {
			c, k := resetJSONCopy(config), resetJSONCopy(credential)
			original, target := value.Original, value.Target
			switch scenario {
			case "replacement_config":
				c["metadata"].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
			case "replacement_credential":
				k["metadata"].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
			case "password_disagrees":
				k["data"].(map[string]any)["probePassword"] = base64.StdEncoding.EncodeToString([]byte("other"))
			case "probe_disagrees":
				k["data"].(map[string]any)["probeVerifier"] = base64.StdEncoding.EncodeToString([]byte("SCRAM-SHA-256$invalid"))
			case "bad_encoding":
				c["data"].(map[string]any)["future-private-file"] = "!"
			case "wrong_owner":
				c["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/endpoint-id"] = "ep_ffffffffffffffff"
			case "cross_tenant":
				target.TenantID = strings.Repeat("f", 32)
			case "unknown_witness":
				original.Witness = "forged"
			}
			if _, err := prepareBranchResetBacking(value.Operation, original, target, c, k); err == nil {
				t.Fatal("untrusted rollback backing accepted")
			}
		})
	}
	forged := value
	forged.PreparedData = map[string]string{}
	for k, v := range value.PreparedData {
		forged.PreparedData[k] = v
	}
	forged.PreparedData["control.key"] = base64.StdEncoding.EncodeToString([]byte("replacement"))
	if validateBranchResetBacking(forged) == nil {
		t.Fatal("prepared forged private file accepted")
	}
}

func TestBranchResetBackingUnknownWritesCASReplayAndRollback(t *testing.T) {
	value, config, credential := resetBackingFixture(t)
	var mu sync.Mutex
	var snapshot map[string]any
	var puts, posts int
	lostPost, lostPut, conflict := true, true, true
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/secrets"):
			posts++
			if snapshot != nil {
				w.WriteHeader(409)
				return
			}
			if json.NewDecoder(r.Body).Decode(&snapshot) != nil {
				t.Error("invalid snapshot request")
				w.WriteHeader(422)
				return
			}
			snapshot["metadata"].(map[string]any)["uid"] = "44444444-4444-4444-8444-444444444444"
			if lostPost {
				lostPost = false
				w.WriteHeader(503)
				return
			}
			jsonResponse(w, 201, snapshot)
		case strings.HasSuffix(path, "/"+value.secretName()):
			if snapshot == nil {
				w.WriteHeader(404)
				return
			}
			jsonResponse(w, 200, snapshot)
		case strings.HasSuffix(path, "-credentials"):
			jsonResponse(w, 200, credential)
		case strings.HasSuffix(path, "-config"):
			if r.Method == http.MethodPut {
				puts++
				if conflict {
					conflict = false
					w.WriteHeader(409)
					return
				}
				var next map[string]any
				if json.NewDecoder(r.Body).Decode(&next) != nil {
					t.Error("invalid CAS body")
					w.WriteHeader(422)
					return
				}
				if nested(next, "metadata", "uid") != value.Original.ConfigUID || nested(next, "metadata", "resourceVersion") != nested(config, "metadata", "resourceVersion") {
					t.Error("CAS lost original object identity")
				}
				config = next
				config["metadata"].(map[string]any)["resourceVersion"] = "2"
				if lostPut {
					lostPut = false
					w.WriteHeader(503)
					return
				}
			}
			jsonResponse(w, 200, config)
		default:
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	ctx := context.Background()
	ref, uid, err := k.reserveBranchResetBacking(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = k.reserveBranchResetBacking(ctx, value); err != nil {
		t.Fatal("same immutable reservation failed", err)
	}
	restored, err := k.readBranchResetBacking(ctx, value.Operation, value.Original.Project, value.Original.Endpoint, ref, uid, value.digest())
	if err != nil || restored.digest() != value.digest() {
		t.Fatal("original immutable snapshot could not be read", err)
	}
	if err = k.transitionBranchResetBacking(ctx, restored, false); err != nil {
		t.Fatal("lost CAS did not recover", err)
	}
	if err = k.transitionBranchResetBacking(ctx, restored, false); err != nil {
		t.Fatal("same preparation replay failed", err)
	}
	mu.Lock()
	got, _ := resetSecretData(config)
	count := puts
	mu.Unlock()
	if count != 2 || !reflect.DeepEqual(got, value.PreparedData) {
		t.Fatal("preparation repeated a committed write", count)
	}
	if err = k.transitionBranchResetBacking(ctx, restored, true); err != nil {
		t.Fatal("rollback failed", err)
	}
	if err = k.transitionBranchResetBacking(ctx, restored, true); err != nil {
		t.Fatal("same rollback replay failed", err)
	}
	mu.Lock()
	got, _ = resetSecretData(config)
	count, postCount := puts, posts
	mu.Unlock()
	if count != 3 || postCount != 2 || !reflect.DeepEqual(got, value.ConfigData) {
		t.Fatal("rollback did not retain exact original bytes")
	}
	mu.Lock()
	config["metadata"].(map[string]any)["uid"] = "55555555-5555-4555-8555-555555555555"
	mu.Unlock()
	if k.transitionBranchResetBacking(ctx, restored, false) == nil {
		t.Fatal("replacement config was adopted")
	}
	mu.Lock()
	config["metadata"].(map[string]any)["uid"] = value.Original.ConfigUID
	config["data"].(map[string]any)["unrelated-new-file"] = base64.StdEncoding.EncodeToString([]byte("keep"))
	mu.Unlock()
	if k.transitionBranchResetBacking(ctx, restored, false) == nil {
		t.Fatal("third-state original files were overwritten")
	}
	mu.Lock()
	config["data"] = resetDataAny(value.ConfigData)
	credential["data"].(map[string]any)["future-private-credential"] = base64.StdEncoding.EncodeToString([]byte("changed"))
	mu.Unlock()
	if k.transitionBranchResetBacking(ctx, restored, false) == nil {
		t.Fatal("same UID credential drift accepted")
	}
	mu.Lock()
	count = puts
	snapshot["metadata"].(map[string]any)["uid"] = "66666666-6666-4666-8666-666666666666"
	mu.Unlock()
	if count != 3 {
		t.Fatal("rejected successor was mutated")
	}
	if _, err = k.readBranchResetBacking(ctx, value.Operation, value.Original.Project, value.Original.Endpoint, ref, uid, value.digest()); err == nil {
		t.Fatal("replacement immutable rollback snapshot accepted")
	}
}
