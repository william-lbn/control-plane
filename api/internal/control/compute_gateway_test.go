package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayJWTRejectsReplayAndConfusedIdentity(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	endpoint := "ep_0123456789abcdef"
	workload := kubeName(endpoint)
	now := time.Unix(1790985000, 0)
	config := map[string]any{}
	installComputeJWK(config, key)
	signed := func(alg string, claims map[string]any) string {
		header, _ := json.Marshal(map[string]any{"alg": alg, "kid": computeKeyID(key)})
		payload, _ := json.Marshal(claims)
		input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
	}
	claims := func() map[string]any {
		return map[string]any{"compute_id": workload, "aud": []string{"compute"}, "exp": now.Add(time.Minute).Unix(), "iat": now.Unix()}
	}
	if !validComputeJWT(computeJWT(key, endpoint, now), workload, config, now) {
		t.Fatal("valid exact-endpoint JWT rejected")
	}
	cases := map[string]string{"wrong_signer": computeJWT(other, endpoint, now), "expired": computeJWT(key, endpoint, now.Add(-time.Minute)), "wrong_endpoint": computeJWT(key, "ep_fedcba9876543210", now), "malformed": "x.y.z"}
	for field, value := range map[string]any{"scope": "admin", "aud": []string{"admin"}, "exp": now.Add(91 * time.Second).Unix(), "iat": now.Add(31 * time.Second).Unix()} {
		c := claims()
		c[field] = value
		cases[field] = signed("EdDSA", c)
	}
	for name, issued := range map[string]int64{"missing_issued_at": 0, "stale_issued_at": now.Add(-91 * time.Second).Unix(), "expiry_before_issued": now.Add(61 * time.Second).Unix()} {
		c := claims()
		c["iat"] = issued
		cases[name] = signed("EdDSA", c)
	}
	cases["algorithm_confusion"] = signed("HS256", claims())
	token := computeJWT(key, endpoint, now)
	parts := strings.Split(token, ".")
	cases["tampered_payload"] = parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"compute_id":"cp-f"}`)) + "." + parts[2]
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if validComputeJWT(token, workload, config, now) {
				t.Fatal("unsafe JWT accepted")
			}
		})
	}
}

func TestGatewayPreservesNativePrimaryDefaultAndRestrictsTopology(t *testing.T) {
	config := map[string]any{"compute_ctl_config": map[string]any{}, "spec": map[string]any{
		"cluster": map[string]any{"cluster_id": "cp-0123456789abcdef", "settings": []any{
			map[string]any{"name": "neon.tenant_id", "value": "tenant"},
			map[string]any{"name": "neon.timeline_id", "value": "timeline"},
			map[string]any{"name": "neon.pageserver_connstring", "value": "pageserver:6400"},
		}}}}
	clone := func() map[string]any {
		b, _ := json.Marshal(config)
		var result map[string]any
		json.Unmarshal(b, &result)
		return result
	}
	if !sameComputeIdentity(config, clone()) {
		t.Fatal("native omitted Primary mode rejected")
	}
	primary := clone()
	primary["spec"].(map[string]any)["mode"] = "Primary"
	if !sameComputeIdentity(config, primary) {
		t.Fatal("explicit native Primary not equivalent to default")
	}
	catalog := clone()
	nested(catalog, "spec", "cluster").(map[string]any)["roles"] = []any{map[string]any{"name": "application"}}
	if !sameComputeIdentity(config, catalog) {
		t.Fatal("catalog-only change rejected")
	}
	for _, field := range []string{"tenant_id", "timeline_id", "pageserver_connstring", "pageserver_connection_info", "safekeeper_connstrings", "future_topology_override"} {
		t.Run(field, func(t *testing.T) {
			changed := clone()
			changed["spec"].(map[string]any)[field] = "foreign"
			if sameComputeIdentity(config, changed) {
				t.Fatal("non-catalog topology override accepted")
			}
		})
	}
	null := clone()
	null["spec"].(map[string]any)["mode"] = nil
	if sameComputeIdentity(config, null) {
		t.Fatal("explicit null mode accepted")
	}
}

func TestKubeClientObservesProjectedTokenRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	observed := []string{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = append(observed, r.Header.Get("Authorization"))
		w.Write([]byte(`{}`))
	}))
	defer api.Close()
	kube := &kubeClient{base: api.URL, token: "stale", tokenFile: path, http: api.Client()}
	if _, err := kube.request(context.Background(), "GET", "/", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.request(context.Background(), "GET", "/", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(observed, ",") != "Bearer initial,Bearer rotated" {
		t.Fatal("client retained stale projected token")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.request(context.Background(), "GET", "/", nil); err == nil {
		t.Fatal("missing projected credential did not fail closed")
	}
}

func TestGatewayComputeIdentityFailsClosedOnMalformedSpec(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{"spec": map[string]any{"mode": map[string]any{"Static": "0/123"},
			"cluster": map[string]any{"cluster_id": "cp-0123456789abcdef", "settings": []any{
				map[string]any{"name": "neon.tenant_id", "value": "tenant"},
				map[string]any{"name": "neon.timeline_id", "value": "timeline"},
				map[string]any{"name": "neon.pageserver_connstring", "value": "pageserver:6400"},
			}}}, "compute_ctl_config": map[string]any{"jwks": map[string]any{"keys": []any{}}}}
	}
	if !sameComputeIdentity(valid(), valid()) {
		t.Fatal("equal tagged mode rejected")
	}
	cases := map[string]func(map[string]any){
		"mode_map_changed":   func(v map[string]any) { v["spec"].(map[string]any)["mode"] = map[string]any{"Static": "0/456"} },
		"missing_mode":       func(v map[string]any) { delete(v["spec"].(map[string]any), "mode") },
		"mode_wrong_type":    func(v map[string]any) { v["spec"].(map[string]any)["mode"] = []any{} },
		"cluster_id_object":  func(v map[string]any) { nested(v, "spec", "cluster").(map[string]any)["cluster_id"] = map[string]any{} },
		"missing_settings":   func(v map[string]any) { delete(nested(v, "spec", "cluster").(map[string]any), "settings") },
		"non_object_setting": func(v map[string]any) { nested(v, "spec", "cluster").(map[string]any)["settings"] = []any{"bad"} },
		"duplicate_identity": func(v map[string]any) {
			cluster := nested(v, "spec", "cluster").(map[string]any)
			cluster["settings"] = append(cluster["settings"].([]any), map[string]any{"name": "neon.tenant_id", "value": "other"})
		},
		"changed_tls": func(v map[string]any) { v["compute_ctl_config"] = map[string]any{"tls": "other"} },
		"null_spec":   func(v map[string]any) { v["spec"] = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := valid()
			change(candidate)
			if sameComputeIdentity(valid(), candidate) {
				t.Fatal("unsafe native configuration accepted")
			}
		})
	}
	if sameComputeIdentity(nil, nil) {
		t.Fatal("missing stored identity accepted")
	}
}
