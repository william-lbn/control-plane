package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCatalogRestoreBackingsRejectReplacementAndDriftWithoutWrites(t *testing.T) {
	for _, scenario := range []string{"original", "config_uid", "routes_uid", "foreign_config", "external_config_name", "external_route_name", "reader_credential", "database_owner"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, p := restoreFixture()
			const reader = "ep_c0123456789abcde"
			p.ConfigUIDs[reader] = "44444444-4444-4444-8444-444444444444"
			verifier := "owned-test-verifier"
			configs := map[string]record{}
			routes := map[string]map[string]any{}
			for id, uid := range p.ConfigUIDs {
				name := p.NewName
				credential := verifier
				project := p.ProjectID
				if scenario == "external_config_name" {
					name = p.Name
				}
				if scenario == "reader_credential" && id == reader {
					credential = "changed"
				}
				if scenario == "foreign_config" {
					project = "prj_c0123456789abcde"
				}
				if scenario == "config_uid" {
					uid = "55555555-5555-4555-8555-555555555555"
				}
				value := record{"retained": "unrelated-config", "spec": record{"cluster": record{"roles": []any{record{"name": name, "encrypted_password": credential}}, "databases": []any{record{"name": "owned_database", "owner": p.NewName}}}}}
				raw, _ := json.Marshal(value)
				configs[id] = record{"metadata": record{"uid": uid, "labels": credentialLabels(project, id)}, "data": record{"config.json": base64.StdEncoding.EncodeToString(raw)}}
				routeName := p.NewName
				if scenario == "external_route_name" {
					routeName = p.Name
				}
				routes[selector(id)] = map[string]any{"project_id": p.ProjectID, "branch_id": p.BranchID, "roles": map[string]any{routeName: verifier}, "retained": "unrelated-route"}
			}
			if scenario == "database_owner" {
				p.Kind = "database"
				p.Name = "external_database"
				p.NewName = "owned_database"
				p.Owner = "expected_other_owner"
				p.CredentialRef = ""
				p.CredentialUID = ""
				p.ResetCredential = false
			}
			routes["unrelated"] = map[string]any{"retained": true}
			raw, _ := json.Marshal(routes)
			uid := p.RoutesUID
			if scenario == "routes_uid" {
				uid = "55555555-5555-4555-8555-555555555555"
			}
			routesSecret := record{"metadata": record{"uid": uid}, "data": record{"routes.json": base64.StdEncoding.EncodeToString(raw)}}
			before, _ := json.Marshal(record{"configs": configs, "routes": routesSecret})
			writes := 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					w.WriteHeader(405)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/"+routesSecretNameForRestoreTest()) {
					jsonResponse(w, 200, routesSecret)
					return
				}
				for id, secret := range configs {
					if strings.HasSuffix(r.URL.Path, "/"+kubeName(id)+"-config") {
						jsonResponse(w, 200, secret)
						return
					}
				}
				w.WriteHeader(404)
			}))
			defer peer.Close()
			s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}}
			err := s.verifyCatalogRestoreBackings(context.Background(), p, verifier)
			if (scenario == "original") != (err == nil) {
				t.Fatal("incorrect original backing decision", err)
			}
			after, _ := json.Marshal(record{"configs": configs, "routes": routesSecret})
			if writes != 0 || string(before) != string(after) {
				t.Fatal("observation changed retained stores")
			}
		})
	}
}
func routesSecretNameForRestoreTest() string { return routesSecret }
