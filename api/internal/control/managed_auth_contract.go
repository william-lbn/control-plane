package control

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// Protocol documentation has no SQL/runtime side effect. The file path is
// packaged configuration; request input only customizes a validated branch.
func (s *server) applicationAuthOpenAPI(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch_id")
	if branch != "" && !dataBranchID.MatchString(branch) {
		fail(w, r, 422, "invalid_request", "Valid branch ID required")
		return
	}
	file := filepath.Join(filepath.Dir(env("NEON_V2_OPENAPI", filepath.Join("..", "contracts", "openapi-v1.json"))), "application-auth-v1.json")
	raw, e := os.ReadFile(file)
	if e != nil {
		fail(w, r, 503, "contract_unavailable", "Application Auth contract unavailable")
		return
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil {
		fail(w, r, 503, "contract_unavailable", "Application Auth contract invalid")
		return
	}
	if branch != "" {
		servers, ok := document["servers"].([]any)
		if !ok || len(servers) != 1 {
			fail(w, r, 503, "contract_unavailable", "Application Auth server invalid")
			return
		}
		server, serverOK := servers[0].(map[string]any)
		variable, variableOK := nested(server, "variables", "branch_id").(map[string]any)
		session, sessionOK := nested(document, "components", "securitySchemes", "applicationSession").(map[string]any)
		if !serverOK || !variableOK || !sessionOK {
			fail(w, r, 503, "contract_unavailable", "Application Auth structure invalid")
			return
		}
		variable["default"] = branch
		session["name"] = "neon_app_" + branch + ".session_token"
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 200, document)
}
