package control

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestApplicationFactorChallengeCookieContract(t *testing.T) {
	t.Setenv("NEON_V2_OPENAPI", filepath.Join("..", "..", "..", "contracts", "openapi-v1.json"))
	r := httptest.NewRecorder()
	s := &server{}
	s.applicationAuthOpenAPI(r, httptest.NewRequest("GET", "/api/application-auth-openapi.json?branch_id=br_ffffffffffffffff", nil))
	if r.Code != 200 {
		t.Fatal("protocol unavailable")
	}
	var doc map[string]any
	if json.Unmarshal(r.Body.Bytes(), &doc) != nil {
		t.Fatal("invalid protocol")
	}
	if nested(doc, "components", "securitySchemes", "applicationFactorChallenge", "name") != "neon_app_br_ffffffffffffffff.two_factor" {
		t.Fatal("pending-factor cookie was not bound to requested branch")
	}
	path := nested(doc, "paths", "/two-factor/verify-totp", "post").(map[string]any)
	security := path["security"].([]any)
	if len(security) != 2 {
		t.Fatal("session and pending challenge alternatives required")
	}
	if nested(doc, "info", "version") != "0.28.0" {
		t.Fatal("contract correction version missing")
	}
}
