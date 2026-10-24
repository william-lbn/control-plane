package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedAuthProtocolContractAndDocumentationHaveNoDatabaseEffect(t *testing.T) {
	raw, e := os.ReadFile("../../../contracts/application-auth-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		t.Fatal("Invalid application contract")
	}
	t.Setenv("NEON_AUTH_MAIL_ENABLED", "true")
	t.Setenv("NEON_AUTH_MAIL_SECRET", "auth-mail-ci")
	for path, entry := range doc["paths"].(map[string]any) {
		action := strings.TrimPrefix(path, "/")
		if action == "reset-password/{token}" {
			action = "reset-password/abcdefghijklmnopqrstuvwxyz"
		}
		if action == "callback/{provider}" {
			action = "callback/google"
		}
		for method := range entry.(map[string]any) {
			if !managedAuthAllowedAction(action, strings.ToUpper(method)) {
				t.Fatal("Contract describes unallowlisted route", path, method)
			}
		}
	}
	if _, exposed := doc["paths"].(map[string]any)["/email-otp/get-verification-otp"]; exposed {
		t.Fatal("Stored code retrieval documented")
	}
	directory := t.TempDir()
	file := filepath.Join(directory, "application-auth-v1.json")
	if os.WriteFile(file, raw, 0600) != nil {
		t.Fatal("Fixture failed")
	}
	t.Setenv("NEON_V2_OPENAPI", filepath.Join(directory, "openapi-v1.json"))
	s := &server{} // Nil DB/Kubernetes: documentation must never contact either.
	recorder := httptest.NewRecorder()
	s.applicationAuthOpenAPI(recorder, httptest.NewRequest(http.MethodGet, "/api/application-auth-openapi.json?branch_id=br_0123456789abcdef", nil))
	if recorder.Code != 200 {
		t.Fatal("Contract lookup failed")
	}
	var actual map[string]any
	if json.Unmarshal(recorder.Body.Bytes(), &actual) != nil {
		t.Fatal("Invalid response")
	}
	if nested(actual, "components", "securitySchemes", "applicationSession", "name") != "neon_app_br_0123456789abcdef.session_token" {
		t.Fatal("Cookie boundary incorrect")
	}
	recorder = httptest.NewRecorder()
	s.applicationAuthOpenAPI(recorder, httptest.NewRequest(http.MethodGet, "/api/application-auth-openapi.json?branch_id=../private", nil))
	if recorder.Code != 422 {
		t.Fatal("Invalid branch accepted")
	}
	if os.WriteFile(file, []byte(`{"servers":[{}]}`), 0600) != nil {
		t.Fatal("Fixture failed")
	}
	recorder = httptest.NewRecorder()
	s.applicationAuthOpenAPI(recorder, httptest.NewRequest(http.MethodGet, "/api/application-auth-openapi.json?branch_id=br_0123456789abcdef", nil))
	if recorder.Code != 503 {
		t.Fatal("Malformed packaged contract accepted")
	}
}
