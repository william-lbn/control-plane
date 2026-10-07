package control

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInvitationTokenAndOriginContract(t *testing.T) {
	valid := "ncp_inv_" + strings.Repeat("a", 64)
	if !validInvitationToken(valid) {
		t.Fatal("Valid token rejected")
	}
	for _, token := range []string{valid + "0", strings.ToUpper(valid), "ncp_inv_" + strings.Repeat("g", 64), ""} {
		if validInvitationToken(token) {
			t.Fatal("Malformed token accepted")
		}
	}
	r := httptest.NewRequest("POST", "https://console.example/auth/signup", nil)
	for _, origin := range []string{"https://evil.example", "null", "https://console.example/path", "https://user@console.example", "https://console.example?token=x"} {
		r.Header.Set("Origin", origin)
		if sameOriginRegistration(r) {
			t.Fatal("Untrusted origin accepted")
		}
	}
	r.Header.Set("Origin", "https://console.example")
	if !sameOriginRegistration(r) {
		t.Fatal("Same origin rejected")
	}
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if sameOriginRegistration(r) {
		t.Fatal("Cross-site fetch accepted")
	}
}
