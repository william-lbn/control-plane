package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func renamePayloadForTest() catalogRenamePayload {
	return catalogRenamePayload{catalogPayload: catalogPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", Name: "old_owner", CredentialRef: "catalog-original", ExpectedVersion: 1}, Kind: "role", NewName: "new_owner", ObjectOID: 12345, ConfigUIDs: map[string]string{"ep_0123456789abcdef": "01234567-89ab-cdef-0123-456789abcdef"}, RoutesUID: "11234567-89ab-cdef-0123-456789abcdef", CredentialUID: "21234567-89ab-cdef-0123-456789abcdef"}
}

// Exercise actual HTTP CAS behavior, including a committed PUT whose response
// disappears. Recovery may observe only the bound original Secret identity.
func TestCatalogRenameCASConflictUnknownCommitAndReplacedUID(t *testing.T) {
	for _, target := range []string{"config", "routes"} {
		for _, scenario := range []string{"conflict", "unknown_commit", "replacement"} {
			t.Run(target+"/"+scenario, func(t *testing.T) {
				p := renamePayloadForTest()
				verifier := "original-managed-verifier"
				var value any = map[string]any{"spec": map[string]any{"cluster": map[string]any{"roles": []any{map[string]any{"name": p.Name, "encrypted_password": verifier}}, "databases": []any{map[string]any{"name": "db", "owner": p.Name}}}}}
				uid, key := p.ConfigUIDs[p.EndpointID], "config.json"
				if target == "routes" {
					uid, key = p.RoutesUID, "routes.json"
					value = map[string]map[string]any{selector(p.EndpointID): {"project_id": p.ProjectID, "branch_id": p.BranchID, "roles": map[string]any{p.Name: verifier}}, "unrelated": {"retained": "original"}}
				}
				data, _ := json.Marshal(value)
				secret := record{"metadata": record{"uid": uid, "resourceVersion": "1", "labels": credentialLabels(p.ProjectID, p.EndpointID)}, "data": record{key: base64.StdEncoding.EncodeToString(data)}}
				if scenario == "replacement" {
					secret["metadata"].(record)["uid"] = "31234567-89ab-cdef-0123-456789abcdef"
				}
				writes := 0
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						jsonResponse(w, 200, secret)
						return
					}
					if r.Method != http.MethodPut {
						t.Error("unexpected mutation")
						w.WriteHeader(405)
						return
					}
					writes++
					var candidate record
					if json.NewDecoder(r.Body).Decode(&candidate) != nil {
						t.Error("invalid CAS body")
						w.WriteHeader(400)
						return
					}
					if nested(candidate, "metadata", "uid") != uid || nested(candidate, "metadata", "resourceVersion") != nested(secret, "metadata", "resourceVersion") {
						t.Error("CAS omitted original identity/version")
						w.WriteHeader(409)
						return
					}
					if scenario == "conflict" && writes == 1 {
						secret["metadata"].(record)["resourceVersion"] = "2"
						if target == "routes" {
							raw, _ := secretText(secret, key)
							var routes map[string]map[string]any
							_ = json.Unmarshal([]byte(raw), &routes)
							routes["concurrent-foreign"] = map[string]any{"retained": "concurrent"}
							rawBytes, _ := json.Marshal(routes)
							secret["data"].(record)[key] = base64.StdEncoding.EncodeToString(rawBytes)
						}
						jsonResponse(w, 409, record{"message": "version conflict"})
						return
					}
					candidate["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(writes + 2)
					secret = candidate
					if scenario == "unknown_commit" && writes == 1 {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err == nil {
							_ = conn.Close()
						}
						return
					}
					jsonResponse(w, 200, secret)
				}))
				defer peer.Close()
				s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}}
				apply := func() error {
					if target == "routes" {
						return s.renameCatalogRoutes(context.Background(), p, verifier, true)
					}
					return s.renameCatalogSpecs(context.Background(), p, verifier, true)
				}
				err := apply()
				if scenario == "replacement" {
					if err == nil || writes != 0 {
						t.Fatal("replacement identity was mutated", err, writes)
					}
					return
				}
				if scenario == "unknown_commit" {
					if err == nil {
						t.Fatal("lost committed response must remain unknown until observation")
					}
					err = apply()
				}
				if err != nil {
					t.Fatal("original CAS recovery failed", err)
				}
				want := 1
				if scenario == "conflict" {
					want = 2
				}
				if writes != want {
					t.Fatal("unexpected replay mutation count", writes)
				}
				raw, _ := secretText(secret, key)
				if target == "routes" {
					var routes map[string]map[string]any
					_ = json.Unmarshal([]byte(raw), &routes)
					if routes[selector(p.EndpointID)]["roles"].(map[string]any)[p.NewName] != verifier || routes["unrelated"]["retained"] != "original" || (scenario == "conflict" && routes["concurrent-foreign"]["retained"] != "concurrent") {
						t.Fatal("CAS lost rename or another project update")
					}
				} else {
					var config map[string]any
					_ = json.Unmarshal([]byte(raw), &config)
					if err := catalogRenameDelta(config, p, verifier); err != nil {
						t.Fatal("CAS effect absent", err)
					}
				}
			})
		}
	}
}

func TestCatalogRenamePreservesSpecAndRejectsForeignTarget(t *testing.T) {
	p := renamePayloadForTest()
	verifier := "synthetic-verifier"
	config := map[string]any{"spec": map[string]any{"cluster": map[string]any{"roles": []any{map[string]any{"name": p.Name, "encrypted_password": verifier, "options": []any{"custom"}}, map[string]any{"name": "unrelated", "encrypted_password": "kept"}}, "databases": []any{map[string]any{"name": "owned", "owner": p.Name}, map[string]any{"name": "other", "owner": "unrelated"}}}, "delta_operations": []any{map[string]any{"name": "independent", "action": "delete_role"}}}, "unrelated": map[string]any{"retained": true}}
	if err := catalogRenameDelta(config, p, verifier); err != nil {
		t.Fatal(err)
	}
	once, _ := json.Marshal(config)
	if err := catalogRenameDelta(config, p, verifier); err != nil {
		t.Fatal(err)
	}
	twice, _ := json.Marshal(config)
	if string(once) != string(twice) {
		t.Fatal("rename replay changed desired state")
	}
	cluster := nested(config, "spec", "cluster").(map[string]any)
	roles := cluster["roles"].([]any)
	if roles[0].(map[string]any)["name"] != p.NewName || len(roles[0].(map[string]any)["options"].([]any)) != 1 || cluster["databases"].([]any)[0].(map[string]any)["owner"] != p.NewName || len(nested(config, "spec", "delta_operations").([]any)) != 1 {
		t.Fatal("rename discarded options, owner references or unrelated deltas")
	}
	roles = append(roles, map[string]any{"name": p.Name, "encrypted_password": "foreign"})
	cluster["roles"] = roles
	if err := catalogRenameDelta(config, p, verifier); err == nil {
		t.Fatal("foreign source/target pair admitted")
	}
}

func TestCatalogRenameRouteIsolationAndUIDBindings(t *testing.T) {
	p := renamePayloadForTest()
	if !p.valid() || !p.bindingsValid() {
		t.Fatal("fixture invalid")
	}
	routes := map[string]map[string]any{selector(p.EndpointID): {"project_id": p.ProjectID, "branch_id": p.BranchID, "roles": map[string]any{p.Name: "original", "unrelated": "kept"}}, "foreign": {"roles": map[string]any{p.Name: "foreign-credential"}}}
	if err := renameCatalogRouteMap(routes, p, "original"); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(routes)
	if err := renameCatalogRouteMap(routes, p, "original"); err != nil {
		t.Fatal(err)
	}
	second, _ := json.Marshal(routes)
	if string(first) != string(second) || routes["foreign"]["roles"].(map[string]any)[p.Name] != "foreign-credential" {
		t.Fatal("route replay or project isolation")
	}
	routes[selector(p.EndpointID)]["roles"].(map[string]any)[p.Name] = "foreign"
	if err := renameCatalogRouteMap(routes, p, "original"); err == nil {
		t.Fatal("foreign Proxy credential adopted")
	}
	original := p.RoutesUID
	p.RoutesUID = ""
	if p.bindingsValid() {
		t.Fatal("unbound routes admitted")
	}
	p.RoutesUID = original
	p.ConfigUIDs[p.EndpointID] = "recreated-invalid"
	if p.bindingsValid() {
		t.Fatal("unbound compute config admitted")
	}
}
