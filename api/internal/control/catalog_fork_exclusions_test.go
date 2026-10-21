package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCatalogForkKnownReplacementPreservesUnmanagedIdentity(t *testing.T) {
	_, snapshot := inventoryFixture()
	candidate := catalogForkExclusion{Name: snapshot.Roles[0].Name, BaselineID: "cbi_111111111111111111111111", SourceBranchID: "br_0123456789abcdef", ExpectedOID: snapshot.Roles[0].OID, CredentialRef: "original-fork-role-credential", CredentialUID: "11111111-1111-4111-8111-111111111111"}
	if result, err := prepareCatalogForkExclusions([]catalogForkExclusion{candidate}, snapshot, nil); err != nil || len(result) != 0 {
		t.Fatal("unchanged original identity excluded", err)
	}
	copied := snapshot
	copied.Roles = append([]catalogNativeRole{}, snapshot.Roles...)
	copied.Roles[0].Name = "original_other_name"
	replacement := snapshot.Roles[0]
	replacement.OID += 100
	copied.Roles = append(copied.Roles, replacement)
	result, err := prepareCatalogForkExclusions([]catalogForkExclusion{candidate}, copied, nil)
	if err != nil || len(result) != 1 || result[0].ExpectedOID != candidate.ExpectedOID || result[0].ObservedOID != replacement.OID || result[0].CredentialUID != candidate.CredentialUID {
		t.Fatal("known replacement original history lost", err)
	}
	if _, err = prepareCatalogForkExclusions([]catalogForkExclusion{candidate}, copied, map[string]bool{candidate.Name: true}); err == nil {
		t.Fatal("critical Owner/default/Backend dependency silently excluded")
	}
	propagated := result[0]
	if next, e := prepareCatalogForkExclusions([]catalogForkExclusion{propagated}, copied, nil); e != nil || len(next) != 1 {
		t.Fatal("original exclusion not propagated", e)
	}
	changed := copied
	changed.Roles = append([]catalogNativeRole{}, copied.Roles...)
	changed.Roles[len(changed.Roles)-1].OID++
	if _, err = prepareCatalogForkExclusions([]catalogForkExclusion{propagated}, changed, nil); err == nil {
		t.Fatal("a new source replacement was adopted on retry")
	}
	missing := copied
	missing.Roles = missing.Roles[:len(missing.Roles)-1]
	if _, err = prepareCatalogForkExclusions([]catalogForkExclusion{candidate}, missing, nil); err == nil {
		t.Fatal("missing original role silently re-created")
	}
	malformed := candidate
	malformed.Name = "control_probe"
	if _, err = prepareCatalogForkExclusions([]catalogForkExclusion{malformed}, copied, nil); err == nil {
		t.Fatal("protected platform role exclusion accepted")
	}
}

func TestCatalogForkExclusionBoundCASAndNoNativeDeltas(t *testing.T) {
	for _, target := range []string{"config", "routes"} {
		scenarios := []string{"conflict", "unknown_commit", "replacement", "verifier_drift"}
		if target == "config" {
			scenarios = append(scenarios, "native_delta")
		}
		for _, scenario := range scenarios {
			t.Run(target+"/"+scenario, func(t *testing.T) {
				const verifier = "only-original-opaque-intent"
				b := catalogForkBacking{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", OperationID: "op_111111111111111111111111", ConfigUID: "11111111-1111-4111-8111-111111111111", RoutesUID: "22222222-2222-4222-8222-222222222222", Digests: map[string]string{"replaced_role": catalogMarkerDigest(verifier)}}
				config := map[string]any{"retain_top": "top-value", "spec": map[string]any{"cluster": map[string]any{"roles": []any{map[string]any{"name": "replaced_role", "encrypted_password": verifier, "options": nil}, map[string]any{"name": "cloud_admin", "encrypted_password": "retained-platform"}}, "databases": []any{map[string]any{"name": "retained_data", "owner": "another_owner", "options": []any{map[string]any{"name": "retained-option", "value": "retained-value"}}}}}, "delta_operations": []any{map[string]any{"action": "delete_role", "name": "unrelated_deleted_role", "new_name": nil}}, "retained_setting": true}}
				routes := map[string]map[string]any{selector(b.EndpointID): {"project_id": b.ProjectID, "branch_id": b.BranchID, "role": "cloud_admin", "roles": map[string]any{"cloud_admin": "retained-platform", "replaced_role": verifier}}, "foreign-route": {"project_id": "foreign-project", "retained": "foreign-value"}}
				if scenario == "verifier_drift" {
					config["spec"].(map[string]any)["cluster"].(map[string]any)["roles"].([]any)[0].(map[string]any)["encrypted_password"] = "unknown-new-credential"
					routes[selector(b.EndpointID)]["roles"].(map[string]any)["replaced_role"] = "unknown-new-credential"
				}
				if scenario == "native_delta" {
					config["spec"].(map[string]any)["delta_operations"] = []any{map[string]any{"action": "delete_role", "name": "replaced_role"}}
				}
				value := any(config)
				uid, key := b.ConfigUID, "config.json"
				if target == "routes" {
					value = routes
					uid, key = b.RoutesUID, "routes.json"
				}
				encode := func(v any) string { raw, _ := json.Marshal(v); return base64.StdEncoding.EncodeToString(raw) }
				secret := record{"metadata": record{"uid": uid, "resourceVersion": "1", "labels": credentialLabels(b.ProjectID, b.EndpointID)}, "data": record{key: encode(value), "unrelated": "retained"}}
				if scenario == "replacement" {
					secret["metadata"].(record)["uid"] = "33333333-3333-4333-8333-333333333333"
				}
				writes := 0
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						jsonResponse(w, 200, secret)
						return
					}
					if r.Method != http.MethodPut {
						t.Error("unexpected backing mutation")
						w.WriteHeader(405)
						return
					}
					writes++
					var proposed record
					if json.NewDecoder(r.Body).Decode(&proposed) != nil {
						t.Error("invalid CAS body")
						w.WriteHeader(400)
						return
					}
					if nested(proposed, "metadata", "uid") != uid || nested(proposed, "metadata", "resourceVersion") != nested(secret, "metadata", "resourceVersion") {
						t.Error("CAS omitted original UID/RV")
						w.WriteHeader(409)
						return
					}
					if scenario == "conflict" && writes == 1 {
						raw, _ := secretText(secret, key)
						var current map[string]any
						_ = json.Unmarshal([]byte(raw), &current)
						if target == "config" {
							current["concurrent_setting"] = "concurrent-value"
						} else {
							current["concurrent_foreign_route"] = map[string]any{"retained": "concurrent-value"}
						}
						secret["data"].(record)[key] = encode(current)
						secret["metadata"].(record)["resourceVersion"] = "2"
						jsonResponse(w, 409, record{"message": "conflict"})
						return
					}
					proposed["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(writes + 2)
					secret = proposed
					if scenario == "unknown_commit" && writes == 1 {
						connection, _, e := w.(http.Hijacker).Hijack()
						if e == nil {
							_ = connection.Close()
						}
						return
					}
					jsonResponse(w, 200, secret)
				}))
				defer peer.Close()
				s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}}
				apply := func() error { return s.applyCatalogForkBacking(context.Background(), b, target == "routes") }
				err := apply()
				if scenario == "replacement" || scenario == "verifier_drift" || scenario == "native_delta" {
					if err == nil || writes != 0 {
						t.Fatal("unproven credential, native delta or replaced UID changed", err, writes)
					}
					return
				}
				if scenario == "unknown_commit" {
					if err == nil {
						t.Fatal("lost committed response incorrectly confirmed")
					}
					err = apply()
				}
				if err != nil {
					t.Fatal("original exclusion CAS retry failed", err)
				}
				expectedWrites := 1
				if scenario == "conflict" {
					expectedWrites = 2
				}
				if writes != expectedWrites {
					t.Fatal("duplicate mutation", writes)
				}
				raw, _ := secretText(secret, key)
				var actual map[string]any
				_ = json.Unmarshal([]byte(raw), &actual)
				if target == "config" {
					if len(nested(actual, "spec", "cluster", "roles").([]any)) != 1 || !strings.Contains(raw, "retained-value") || !strings.Contains(raw, "unrelated_deleted_role") || nested(actual, "spec", "retained_setting") != true {
						t.Fatal("unrelated compute role/database/options/deltas changed")
					}
				} else {
					if _, present := nested(actual, selector(b.EndpointID), "roles").(map[string]any)["replaced_role"]; present || !strings.Contains(raw, "foreign-value") || nested(actual, selector(b.EndpointID), "roles", "cloud_admin") != "retained-platform" {
						t.Fatal("unrelated route or platform credential changed")
					}
				}
				if scenario == "conflict" && !strings.Contains(raw, "concurrent-value") {
					t.Fatal("concurrent unrelated change lost")
				}
				if err = apply(); err != nil || writes != expectedWrites {
					t.Fatal("confirmed original exclusion replay wrote again", err)
				}
			})
		}
	}
}
