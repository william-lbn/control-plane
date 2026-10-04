package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBackendCredentialKeyring(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, 32))
	ring, err := parseBackendKeyring([]byte(`{"active":"v1","keys":{"v1":"` + encoded + `"}}`))
	if err != nil || len(ring.decoded["v1"]) != 32 {
		t.Fatal("valid keyring rejected")
	}
	public, _ := json.Marshal(ring)
	if strings.Contains(string(public), encoded) {
		t.Fatal("keyring secret remained serializable")
	}
	for _, raw := range []string{`{}`, `{"active":"v1","keys":{"v1":"short"}}`, `{"active":"v2","keys":{"v1":"` + encoded + `"}}`, `{"active":"v1","keys":{"v1":"` + encoded + `"},"unknown":1}`, `{"active":"v1","keys":{"v1":"` + encoded + `"}} {}`} {
		if _, err := parseBackendKeyring([]byte(raw)); err == nil {
			t.Fatal("invalid keyring accepted")
		}
	}
	s := &server{backendKeys: ring}
	if s.backendHash("token", "same", "v1") == s.backendHash("request", "same", "v1") {
		t.Fatal("HMAC domains overlap")
	}
}

func TestBackendCredentialPolicyValidation(t *testing.T) {
	now := time.Now().UTC()
	base := backendCredentialSpec{Name: "server", Scopes: []string{"ai_gateway:invoke"}, BranchScope: "self", ExpiresAt: now.Add(time.Hour)}
	if err := validateBackendCredentialSpec(&base, now); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*backendCredentialSpec){
		func(v *backendCredentialSpec) { v.Scopes = []string{"control:admin"} },
		func(v *backendCredentialSpec) { v.ExpiresAt = now.Add(-time.Hour) },
		func(v *backendCredentialSpec) { v.ExpiresAt = now.Add(31 * 24 * time.Hour) },
		func(v *backendCredentialSpec) { v.BranchScope = "all_projects" },
		func(v *backendCredentialSpec) { v.AllowedModels = []string{"model-a", "model-a"} },
	} {
		copy := base
		change(&copy)
		if err := validateBackendCredentialSpec(&copy, now); err == nil {
			t.Fatal("unsafe credential policy accepted")
		}
	}
}
