package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAIProviderRelayModelsAndOriginalInferenceBoundary(t *testing.T) {
	const token = "disposable-relay-key-01234567890123456789"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("transport credential forwarded to Ollama")
		}
		if r.Method == "GET" && r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[{"id":"qwen2.5:0.5b"}]}`)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/chat/completions" {
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write(body)
			return
		}
		t.Error("unapproved upstream route")
		w.WriteHeader(404)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	handler, err := newRelayHandler(token, target)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, credential string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Cookie", "console=must-not-forward")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "/v1/models", "", token); w.Code != 200 || !strings.Contains(w.Body.String(), "qwen2.5:0.5b") {
		t.Fatal("actual model metadata unavailable")
	}
	if w := call("POST", "/v1/chat/completions", `{"messages":[]}`, token); w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" || w.Body.String() != `{"messages":[]}` {
		t.Fatal("original inference body or stream changed")
	}
	before := calls.Load()
	for _, item := range []struct {
		method, path, body, key string
		code                    int
	}{
		{"GET", "/v1/models", "", "wrong", 401}, {"GET", "/api/tags", "", token, 404}, {"POST", "/v1/models", "{}", token, 404}, {"GET", "/v1/models?x=1", "", token, 404}, {"GET", "/v1/models?", "", token, 404}, {"GET", "/v1/m%6Fdels", "", token, 404}, {"GET", "/v1/models", "x", token, 404}, {"POST", "/v1/chat/completions", strings.Repeat("x", 65537), token, 413},
	} {
		if w := call(item.method, item.path, item.body, item.key); w.Code != item.code {
			t.Fatalf("invalid request returned %d want %d", w.Code, item.code)
		}
	}
	if calls.Load() != before {
		t.Fatal("rejected request reached model server")
	}
	for _, raw := range []string{"http://192.168.146.100:11434", "https://127.0.0.1:11434", "http://127.0.0.1:11434/private", "http://127.0.0.1:11434?x=1"} {
		u, _ := url.Parse(raw)
		if _, err = newRelayHandler(token, u); err == nil {
			t.Fatal("unapproved relay destination accepted")
		}
	}
}

func TestAIProviderRelayRejectsOversizedModelMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", (1<<20)+1)) }))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	handler, err := newRelayHandler(strings.Repeat("a", 32), target)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 502 || strings.Contains(w.Body.String(), strings.Repeat("x", 128)) {
		t.Fatal("oversized provider body escaped relay boundary")
	}
}
