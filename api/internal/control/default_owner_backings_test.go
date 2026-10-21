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

func TestDefaultOwnerBoundCASPreservesUnrelatedIntent(t *testing.T) {
	for _, scenario := range []string{"conflict", "unknown_commit", "replacement", "credential_drift"} {
		t.Run(scenario, func(t *testing.T) {
			p := defaultOwnerPayloadForTest()
			const reader = "ep_1123456789abcdef"
			p.ConfigUIDs[reader] = "55555555-5555-4555-8555-555555555555"
			p.ColdOwners[reader] = "cloud_admin"
			secrets := map[string]record{}
			writes := []string{}
			attempts := map[string]int{}
			encode := func(v any) string { b, _ := json.Marshal(v); return base64.StdEncoding.EncodeToString(b) }
			for id, uid := range p.ConfigUIDs {
				databases := []any{record{"name": "unrelated_database", "owner": "another_owner", "options": []any{record{"name": "CONNECTION LIMIT", "value": "7"}}}}
				if id == reader {
					databases = append(databases, record{"name": "postgres", "owner": "cloud_admin", "options": []any{record{"name": "retained-option", "value": "retained-value"}}})
				}
				config := map[string]any{"unrelated_top": "retained", "spec": record{"cluster": record{"roles": []any{record{"name": "cloud_admin", "encrypted_password": "original-platform-verifier", "options": nil}, record{"name": "control_probe", "encrypted_password": "original-probe-verifier", "options": nil}, record{"name": "customer", "encrypted_password": "original-target-verifier", "options": nil}, record{"name": "unrelated_role", "encrypted_password": "unrelated-verifier"}}, "databases": databases}, "retained_spec": true}}
				var e error
				p.RoleHashes[id], e = defaultOwnerConfigRoleHash(config, p)
				if e != nil {
					t.Fatal(e)
				}
				secrets[kubeName(id)+"-config"] = record{"metadata": record{"uid": uid, "resourceVersion": "1", "labels": credentialLabels(p.ProjectID, id)}, "data": record{"config.json": encode(config), "unrelated_secret_key": "retained"}}
			}
			routes := map[string]record{}
			for id := range p.ConfigUIDs {
				routes[selector(id)] = record{"project_id": p.ProjectID, "branch_id": p.BranchID, "role": "cloud_admin", "roles": record{"cloud_admin": "original-platform-verifier", "control_probe": "original-probe-verifier", "customer": "original-target-verifier", "unrelated_role": "retained"}}
			}
			secrets[routesSecret] = record{"metadata": record{"uid": p.RoutesUID}, "data": record{"routes.json": encode(routes)}}
			routesBefore, _ := json.Marshal(secrets[routesSecret])
			if scenario == "replacement" {
				secrets[kubeName(reader)+"-config"]["metadata"].(record)["uid"] = "66666666-6666-4666-8666-666666666666"
			}
			if scenario == "credential_drift" {
				raw, _ := secretText(secrets[kubeName(reader)+"-config"], "config.json")
				raw = strings.Replace(raw, "original-target-verifier", "replacement-target-verifier", 1)
				secrets[kubeName(reader)+"-config"]["data"].(record)["config.json"] = base64.StdEncoding.EncodeToString([]byte(raw))
			}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				current := secrets[name]
				if current == nil {
					w.WriteHeader(404)
					return
				}
				if r.Method == http.MethodGet {
					jsonResponse(w, 200, current)
					return
				}
				if r.Method != http.MethodPut || name == routesSecret {
					t.Error("unexpected backing mutation")
					w.WriteHeader(405)
					return
				}
				attempts[name]++
				var candidate record
				if json.NewDecoder(r.Body).Decode(&candidate) != nil {
					t.Error("invalid CAS body")
					w.WriteHeader(400)
					return
				}
				if nested(candidate, "metadata", "uid") != nested(current, "metadata", "uid") || nested(candidate, "metadata", "resourceVersion") != nested(current, "metadata", "resourceVersion") {
					t.Error("CAS omitted original UID/RV")
					w.WriteHeader(409)
					return
				}
				if scenario == "conflict" && name == kubeName(reader)+"-config" && attempts[name] == 1 {
					current["metadata"].(record)["resourceVersion"] = "2"
					raw, _ := secretText(current, "config.json")
					var config map[string]any
					_ = json.Unmarshal([]byte(raw), &config)
					cluster := nested(config, "spec", "cluster").(map[string]any)
					cluster["roles"] = append(cluster["roles"].([]any), record{"name": "concurrent_role", "encrypted_password": "retained-concurrent-verifier"})
					cluster["databases"] = append(cluster["databases"].([]any), record{"name": "concurrent_db", "owner": "concurrent_role", "options": nil})
					current["data"].(record)["config.json"] = encode(config)
					jsonResponse(w, 409, record{"message": "conflict"})
					return
				}
				candidate["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(attempts[name] + 2)
				secrets[name] = candidate
				writes = append(writes, name)
				if scenario == "unknown_commit" && len(writes) == 1 {
					connection, _, e := w.(http.Hijacker).Hijack()
					if e == nil {
						_ = connection.Close()
					}
					return
				}
				jsonResponse(w, 200, candidate)
			}))
			defer peer.Close()
			s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}}
			err := s.defaultOwnerSpecs(context.Background(), p, true, true)
			if scenario == "replacement" || scenario == "credential_drift" {
				if err == nil || len(writes) != 0 {
					t.Fatal("conflicting backing was mutated", err, writes)
				}
				// Even if the earlier publication step succeeded, a retry must
				// reject cold identity/credential drift before metadata or SQL.
				// The nil database makes any premature native path fail this test.
				if err = s.defaultOwnerNative(context.Background(), "op_111111111111111111111111", p, true); err == nil {
					t.Fatal("native retry bypassed bound cold configuration")
				}
				return
			}
			if scenario == "unknown_commit" {
				if err == nil {
					t.Fatal("lost PUT reply must remain unknown")
				}
				err = s.defaultOwnerSpecs(context.Background(), p, true, true)
			}
			if err != nil {
				t.Fatal("original CAS retry failed", err)
			}
			if len(writes) != 2 || writes[0] != kubeName(reader)+"-config" || writes[1] != kubeName(p.EndpointID)+"-config" {
				t.Fatal("Reader first / Writer last or repeat writes wrong", writes)
			}
			for id := range p.ConfigUIDs {
				secret := secrets[kubeName(id)+"-config"]
				raw, _ := secretText(secret, "config.json")
				var config map[string]any
				_ = json.Unmarshal([]byte(raw), &config)
				owner, e := defaultOwnerConfiguredOwner(config, p.Name)
				if e != nil || owner != "customer" || !strings.Contains(raw, "retained-value") && id == reader || !strings.Contains(raw, "unrelated_database") || !strings.Contains(raw, "unrelated-verifier") || nested(config, "spec", "retained_spec") != true || secret["data"].(map[string]any)["unrelated_secret_key"] != "retained" {
					t.Fatal("Owner delta replaced unrelated content", e)
				}
				if scenario == "conflict" && id == reader && (!strings.Contains(raw, "concurrent_db") || !strings.Contains(raw, "concurrent_role")) {
					t.Fatal("concurrent unrelated intent lost")
				}
			}
			after, _ := json.Marshal(secrets[routesSecret])
			if string(after) != string(routesBefore) {
				t.Fatal("Owner transfer changed Proxy routes or verifiers")
			}
			if err = s.defaultOwnerSpecs(context.Background(), p, true, true); err != nil || len(writes) != 2 {
				t.Fatal("confirmed replay wrote again", err)
			}
		})
	}
}
