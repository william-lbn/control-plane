package aigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testConfig(base string) *Config {
	return &Config{Version: 1, Generation: 1, TimeoutSeconds: 5, MaxConcurrentPerBranch: 1, RequestsPerMinute: 12, Routes: []Route{{ID: "small", Name: "Small", Provider: "openai_compatible", BaseURL: base + "/v1", UpstreamModel: "qwen2.5:0.5b", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true, MaxOutputTokens: 128}}}
}
func TestAIChatValidation(t *testing.T) {
	c := testConfig("http://127.0.0.1:11434")
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for name, raw := range map[string]string{
		"unknown_model":     `{"model":"other","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`,
		"tools_unsupported": `{"model":"small","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"tools":[]}`,
		"zero_output":       `{"model":"small","messages":[{"role":"user","content":"hello"}],"max_tokens":0}`,
		"excess_output":     `{"model":"small","messages":[{"role":"user","content":"hello"}],"max_tokens":129}`,
		"empty_input":       `{"model":"small","messages":[{"role":"user","content":""}],"max_tokens":32}`,
		"invalid_role":      `{"model":"small","messages":[{"role":"tool","content":"hello"}],"max_tokens":32}`,
		"trailing_json":     `{"model":"small"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, e := Decode([]byte(raw), c); e == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	if _, _, e := Decode([]byte(`{"model":"small","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"stream":true}`), c); e != nil {
		t.Fatal(e)
	}
}
func TestAIConfigRejectsUnsafeRoutes(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"unapproved_http":      func(c *Config) { c.Routes[0].LabHTTP = false },
		"userinfo":             func(c *Config) { c.Routes[0].BaseURL = "https://secret@127.0.0.1/v1" },
		"query":                func(c *Config) { c.Routes[0].BaseURL = "https://127.0.0.1/v1?url=elsewhere" },
		"arbitrary_path":       func(c *Config) { c.Routes[0].BaseURL = "https://127.0.0.1/admin" },
		"unpinned":             func(c *Config) { c.Routes[0].AllowedIPs = nil },
		"duplicate_model":      func(c *Config) { c.Routes = append(c.Routes, c.Routes[0]) },
		"unsupported_provider": func(c *Config) { c.Routes[0].Provider = "anthropic" },
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig("http://127.0.0.1")
			change(c)
			if c.Validate() == nil {
				t.Fatal("unsafe route accepted")
			}
		})
	}
}
func TestAIForwardProtocolAndPrivacy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Cookie") != "" {
					t.Error("invalid upstream request")
				}
				if r.Header.Get("Authorization") != "Bearer private-provider-secret" {
					t.Error("provider key absent")
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != "qwen2.5:0.5b" {
					t.Error("model alias not resolved")
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"real reply\"}}]}\n\ndata: {\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"object":"chat.completion","choices":[{"message":{"content":"real reply"}}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
				}
			}))
			defer upstream.Close()
			c := testConfig(upstream.URL)
			r := c.Routes[0]
			r.key = "private-provider-secret"
			w := httptest.NewRecorder()
			result := Forward(context.Background(), w, ChatRequest{Model: "small", Messages: []Message{{"user", "hello"}}, MaxTokens: 32, Stream: stream}, r, 5*time.Second)
			if result.Status != "completed" || result.Usage == nil || result.Usage.TotalTokens != 9 || !strings.Contains(w.Body.String(), "real reply") || !strings.Contains(w.Body.String(), `"model":"small"`) || strings.Contains(w.Body.String(), r.key) {
				t.Fatal("invalid bounded protocol result")
			}
		})
	}
}
func TestAIForwardNeverRetriesOrLeaksProviderError(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "private-provider-secret and request prompt", 500)
	}))
	defer upstream.Close()
	w := httptest.NewRecorder()
	result := Forward(context.Background(), w, ChatRequest{Model: "small", MaxTokens: 32}, testConfig(upstream.URL).Routes[0], 5*time.Second)
	if calls != 1 || result.Status != "failed" || w.Code != 502 || strings.Contains(w.Body.String(), "private-provider-secret") {
		t.Fatal("provider failure leaked or replayed")
	}
}
func TestAIForwardRejectsRedirectAndPinnedIPMismatch(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer source.Close()
	r := testConfig(source.URL).Routes[0]
	w := httptest.NewRecorder()
	Forward(context.Background(), w, ChatRequest{Model: "small"}, r, 5*time.Second)
	if calls != 0 || w.Code != 502 {
		t.Fatal("redirect followed")
	}
	r.AllowedIPs = []string{"192.0.2.1"}
	_, e := Client(r, time.Second).Get(source.URL)
	if e == nil {
		t.Fatal("unpinned address reached")
	}
}
func TestAIIncompleteStreamAndUnknownUsage(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			if !incomplete {
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
		}))
		w := httptest.NewRecorder()
		result := Forward(context.Background(), w, ChatRequest{Model: "small", Stream: true}, testConfig(upstream.URL).Routes[0], time.Second)
		upstream.Close()
		want := "usage_unknown"
		if incomplete {
			want = "incomplete"
		}
		if result.Status != want || result.Usage != nil {
			t.Fatal("missing usage fabricated")
		}
	}
}
func TestAIUsageRejectsInvalidCounts(t *testing.T) {
	for _, raw := range []string{`{"usage":{"prompt_tokens":-1,"completion_tokens":2,"total_tokens":1}}`, `{"usage":{"prompt_tokens":1.5,"completion_tokens":2,"total_tokens":3.5}}`, `{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":30}}`} {
		var v map[string]any
		json.Unmarshal([]byte(raw), &v)
		if usageFrom(v) != nil {
			t.Fatal("invalid token accounting accepted")
		}
	}
}
