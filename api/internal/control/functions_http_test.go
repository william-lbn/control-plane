package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFunctionHTTPOriginAndHeaderBoundary(t *testing.T) {
	t.Setenv("NEON_FUNCTIONS_PUBLIC_LAB_HTTP", "true")
	t.Setenv("NEON_AUTH_PUBLIC_ORIGIN", "http://console.invalid:8788")
	for _, v := range []string{"http://functions.invalid:8792/", "http://functions.invalid:8792/path", "http://user@functions.invalid:8792", "http://console.invalid:8788", "file:///tmp/foo", "https://functions.invalid?q=1"} {
		t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", v)
		if validateFunctionPublicOrigin() == nil {
			t.Fatalf("bad origin admitted: %s", v)
		}
	}
	t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", "http://functions.invalid:8792")
	if validateFunctionPublicOrigin() != nil {
		t.Fatal("separate explicit lab origin rejected")
	}
	t.Setenv("NEON_FUNCTIONS_PUBLIC_LAB_HTTP", "false")
	if validateFunctionPublicOrigin() == nil {
		t.Fatal("implicit cleartext admitted")
	}
	in := http.Header{"Cookie": {"neon_v2_session=private"}, "Authorization": {"Bearer application-token"}, "X-Neon-Manager": {"private"}, "X-Forwarded-Host": {"console.invalid"}, "Set-Cookie": {"neon_v2_session=evil"}, "Content-Type": {"text/event-stream"}}
	out := functionIngressHeaders(in)
	if out["Cookie"] != nil || out["X-Neon-Manager"] != nil || out["X-Forwarded-Host"] != nil || out["Authorization"][0] != "Bearer application-token" {
		t.Fatal("request trust boundary failed")
	}
	response := http.Header{}
	copyFunctionPublicHeaders(response, in)
	if response.Get("Set-Cookie") != "" || response.Get("Content-Type") != "text/event-stream" || response.Get("Cache-Control") != "no-store" {
		t.Fatal("response cookie/stream boundary failed")
	}
}
func TestFunctionPublicListenerCannotServeConsole(t *testing.T) {
	t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", "http://functions.invalid:8792")
	s := &server{}
	for _, path := range []string{"/api/v1/session", "/api/v1/projects", "/api/docs", "/", "/auth/v1/branch"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://functions.invalid:8792"+path, nil)
		s.functionPublicHandler().ServeHTTP(w, r)
		if w.Code != 404 || strings.Contains(w.Body.String(), "csrf") {
			t.Fatal("Console route leaked through public listener")
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://console.invalid/api/v1/session", nil)
	s.functionPublicHandler().ServeHTTP(w, r)
	if w.Code != 421 {
		t.Fatal("wrong authority accepted")
	}
}
