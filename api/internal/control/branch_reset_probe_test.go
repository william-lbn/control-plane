package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetGrantFixture(t *testing.T) (branchResetPlatformGrant, map[string]any, map[string]any, map[string]any, ed25519.PrivateKey) {
	t.Helper()
	source, original, target := resetConfigFixture(t, "read_write")
	original.Endpoint = branchResetProbeEndpoint("op_" + strings.Repeat("a", 24))
	original.ConfigUID = "11111111-1111-4111-8111-111111111111"
	original.CredentialUID = "22222222-2222-4222-8222-222222222222"
	stored, err := branchResetComputeConfig(source, original, target)
	if err != nil {
		t.Fatal(err)
	}
	stored, err = resetInspectionConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	installComputeJWK(stored, key)
	encoded, _ := json.Marshal(stored)
	_ = json.Unmarshal(encoded, &stored)
	roots := nested(resetJSONCopy(stored), "spec", "cluster", "roles").([]any)
	for _, raw := range roots {
		item := raw.(map[string]any)
		verifier, e := scramVerifier("synthetic-new-platform-password-" + stringVal(item["name"]))
		if e != nil {
			t.Fatal(e)
		}
		item["encrypted_password"] = verifier
	}
	snapshot := catalogNativeSnapshot{Scope: target, Roles: []catalogNativeRole{{OID: 10, Name: "cloud_admin", Flags: catalogRoleFlags{Login: true}}, {OID: 16387, Name: "control_probe", Flags: catalogRoleFlags{Login: true, Superuser: true}}}, Databases: []catalogNativeDatabase{{OID: 1, Name: "template1", OwnerOID: 10}}}
	op := "op_" + strings.Repeat("a", 24)
	grant, apply, err := makeBranchResetPlatformGrant(op, original.Branch, original.Endpoint, original.ConfigUID, stored, roots, snapshot, 10, 16387, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	labels := credentialLabels(original.Project, original.Endpoint)
	labels["neon-control/reset-operation"] = op
	secret := map[string]any{"metadata": map[string]any{"uid": original.ConfigUID, "labels": resetDataAny(labels), "annotations": map[string]any{resetInspectionAnnotation: op}}, "data": map[string]any{"config.json": base64.StdEncoding.EncodeToString(encoded)}}
	return grant, secret, stored, apply, key
}
func TestBranchResetProbeGrantRequiresExactNativeObservationAndProjection(t *testing.T) {
	grant, secret, stored, request, _ := resetGrantFixture(t)
	now := time.Now()
	if !validBranchResetPlatformGrant(grant, secret, stored, request, now) {
		t.Fatal("exact private restoration denied")
	}
	if validComputeCatalogRequest(stored, request) {
		t.Fatal("ordinary gateway accepted protected restore without grant")
	}
	for _, scenario := range []string{"expiry", "long_expiry", "config_uid", "config_owner", "public_config", "operation", "observation", "root_oid", "root_collision", "scope", "hash", "third_role", "ordinary_db", "delete_delta", "root_options", "skip", "tenant", "future_override", "private_field"} {
		t.Run(scenario, func(t *testing.T) {
			value := grant
			c, s, r := resetJSONCopy(secret), resetJSONCopy(stored), resetJSONCopy(request)
			switch scenario {
			case "expiry":
				value.ExpiresAt = now.Add(-time.Second)
			case "long_expiry":
				value.ExpiresAt = now.Add(2 * time.Minute)
			case "config_uid":
				c["metadata"].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
			case "config_owner":
				c["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/endpoint-id"] = "ep_ffffffffffffffff"
			case "public_config":
				delete(c["metadata"].(map[string]any)["annotations"].(map[string]any), resetInspectionAnnotation)
			case "operation":
				value.Operation = "op_" + strings.Repeat("b", 24)
			case "observation":
				value.ObservationSHA256 = "not-a-native-observation"
			case "root_oid":
				value.AdminOID = 0
			case "root_collision":
				value.AdminOID = value.ProbeOID
			case "scope":
				value.Scope.TimelineID = strings.Repeat("e", 32)
			case "hash":
				value.RequestedSHA256 = strings.Repeat("e", 64)
			case "third_role":
				nested(r, "spec", "cluster").(map[string]any)["roles"] = append(nested(r, "spec", "cluster", "roles").([]any), map[string]any{"name": "ordinary_user"})
			case "ordinary_db":
				nested(r, "spec", "cluster").(map[string]any)["databases"] = []any{map[string]any{"name": "historical_data"}}
			case "delete_delta":
				r["spec"].(map[string]any)["delta_operations"] = []any{map[string]any{"action": "delete_db", "name": "historical_data"}}
			case "root_options":
				nested(r, "spec", "cluster", "roles").([]any)[0].(map[string]any)["options"] = []any{map[string]any{"name": "SUPERUSER"}}
			case "skip":
				r["spec"].(map[string]any)["skip_pg_catalog_updates"] = true
			case "tenant":
				r["spec"].(map[string]any)["tenant_id"] = strings.Repeat("e", 32)
			case "future_override":
				r["spec"].(map[string]any)["future_startup_override"] = true
			case "private_field":
				r["unrelated_private_extension"].(map[string]any)["opaque"] = "changed"
			}
			// An attacker cannot broaden the projection even if a request hash is
			// recomputed. The exact two unchanged reserved role shapes remain enforced.
			if scenario != "hash" {
				value.RequestedSHA256 = branchResetConfigDigest(r)
			}
			if validBranchResetPlatformGrant(value, c, s, r, now) {
				t.Fatal("broadened private restore grant accepted")
			}
		})
	}
	roots := nested(request, "spec", "cluster", "roles").([]any)
	wrong := catalogNativeSnapshot{Scope: grant.Scope, Roles: []catalogNativeRole{{OID: 11, Name: "cloud_admin", Flags: catalogRoleFlags{Login: true}}, {OID: 16387, Name: "control_probe", Flags: catalogRoleFlags{Login: true}}}}
	if _, _, err := makeBranchResetPlatformGrant(grant.Operation, grant.Branch, grant.Endpoint, grant.ConfigUID, stored, roots, wrong, 10, 16387, now); err == nil {
		t.Fatal("wrong original platform OID authorized")
	}
	// Internal startup flags never become a caller-controlled Operation field.
	raw, _ := json.Marshal(createPayload{ResetInspectionOperation: grant.Operation})
	if strings.Contains(string(raw), grant.Operation) {
		t.Fatal("private inspection startup leaked to Operation")
	}
	for _, item := range nested(stored, "spec", "cluster", "settings").([]any) {
		setting := item.(map[string]any)
		if setting["name"] == "cron.launch_active_jobs" && setting["value"] != "off" {
			t.Fatal("historical scheduled jobs could launch")
		}
	}
}

func TestBranchResetProbeGatewayRequiresTLSJWTAndFixedImmutableGrant(t *testing.T) {
	grant, config, stored, apply, key := resetGrantFixture(t)
	ref := "branch-reset-grant-" + strings.Repeat("d", 32)
	uid := "44444444-4444-4444-8444-444444444444"
	config["metadata"].(map[string]any)["annotations"].(map[string]any)[resetGrantAnnotation] = ref
	config["metadata"].(map[string]any)["annotations"].(map[string]any)[resetGrantUIDAnnotation] = uid
	raw, _ := json.Marshal(grant)
	labels := credentialLabels(grant.Project, grant.Endpoint)
	labels["neon-control/reset-operation"] = grant.Operation
	permission := map[string]any{"immutable": true, "metadata": map[string]any{"uid": uid, "labels": resetDataAny(labels)}, "data": map[string]any{"grant.json": base64.StdEncoding.EncodeToString(raw)}}
	var mu sync.Mutex
	forwards := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+ref):
			jsonResponse(w, 200, permission)
		case strings.HasSuffix(r.URL.Path, "-config"):
			jsonResponse(w, 200, config)
		case strings.Contains(r.URL.Path, "/services/"):
			jsonResponse(w, 200, map[string]any{"metadata": map[string]any{"labels": resetDataAny(labels)}, "spec": map[string]any{"clusterIP": "127.0.0.1"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	gateway := &computeGateway{kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}, http: &http.Client{Transport: catalogGatewayTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		forwards++
		mu.Unlock()
		var got map[string]any
		if json.NewDecoder(r.Body).Decode(&got) != nil || branchResetConfigDigest(got) != branchResetConfigDigest(apply) || r.Header.Get("Authorization") == "" || r.Header.Get(resetGrantHeader) != "" {
			t.Error("private native forwarding broadened authority")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"running"}`))}, nil
	})}}
	send := func(ref, token string, withTLS bool) int {
		raw, _ := json.Marshal(apply)
		r := httptest.NewRequest(http.MethodPost, "/computes/"+kubeName(grant.Endpoint)+"/configure", strings.NewReader(string(raw)))
		r.SetPathValue("compute", kubeName(grant.Endpoint))
		if withTLS {
			r.TLS = &tls.ConnectionState{ServerName: kubeName(grant.Endpoint) + "-management.neon.svc"}
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set(resetGrantHeader, ref)
		w := httptest.NewRecorder()
		gateway.configure(w, r)
		return w.Code
	}
	token := computeJWT(key, grant.Endpoint, time.Now())
	if send("", token, true) != 403 || send(ref, "invalid", true) != 401 || send(ref, token, false) != 403 {
		t.Fatal("private grant bypassed mandatory TLS/JWT or missing grant")
	}
	if send(ref, token, true) != 200 || send(ref, token, true) != 200 {
		t.Fatal("exact idempotent native restore request rejected")
	}
	mu.Lock()
	count := forwards
	permission["metadata"].(map[string]any)["uid"] = "55555555-5555-4555-8555-555555555555"
	mu.Unlock()
	if send(ref, token, true) != 403 {
		t.Fatal("replacement grant UID accepted")
	}
	mu.Lock()
	permission["metadata"].(map[string]any)["uid"] = uid
	permission["immutable"] = false
	mu.Unlock()
	if send(ref, token, true) != 403 {
		t.Fatal("mutable grant accepted")
	}
	mu.Lock()
	permission["immutable"] = true
	delete(config["metadata"].(map[string]any)["annotations"].(map[string]any), resetInspectionAnnotation)
	mu.Unlock()
	if send(ref, token, true) != 403 {
		t.Fatal("ordinary public Compute accepted reset grant")
	}
	mu.Lock()
	after := forwards
	mu.Unlock()
	if count != 2 || after != 2 {
		t.Fatal("rejected private grant reached native configure")
	}
	if validComputeCatalogRequest(stored, apply) {
		t.Fatal("ordinary protected role boundary weakened")
	}
}
