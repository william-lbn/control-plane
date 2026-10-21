package control

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFunctionWebSocketMalformedBeforeWake(t *testing.T) {
	s := &server{}
	for _, method := range []string{"GET", "POST"} {
		r := httptest.NewRequest(method, "http://functions.test/functions/v1/br_0123456789abcdef/hello", nil)
		r.SetPathValue("slug", "hello")
		r.Header.Set("Upgrade", "websocket")
		w := httptest.NewRecorder()
		s.functionPublicInvoke(w, r)
		if w.Code != 400 {
			t.Fatal("malformed handshake reached cold wake", w.Code)
		}
	}
}

func TestFunctionWebSocketConsoleCSP(t *testing.T) {
	t.Setenv("NEON_FUNCTIONS_PUBLIC_LAB_HTTP", "true")
	for _, origin := range []string{"http://192.168.146.100:30892", "https://functions.example.test", "https://[::1]:8792"} {
		t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", origin)
		if !strings.Contains(functionConsoleCSP(), "connect-src 'self' "+origin+" "+strings.Replace(origin, "http", "ws", 1)+";") {
			t.Fatal("exact Functions origin missing")
		}
	}
	for _, origin := range []string{"", "https://bad/with/path", "https://bad;connect-src *", "https://bad'"} {
		t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", origin)
		if strings.Contains(functionConsoleCSP(), "connect-src 'self' ") {
			t.Fatal("unsafe CSP source admitted")
		}
	}
}
