package aigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func nativeConfig(base string) *Config {
	return &Config{Version: 1, Generation: 1, TimeoutSeconds: 5, MaxConcurrentPerBranch: 1, RequestsPerMinute: 20, Routes: []Route{{ID: "alias", Name: "Alias", Provider: "openai_compatible", BaseURL: base + "/v1", UpstreamModel: "upstream", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true, MaxOutputTokens: 64, Protocols: []string{"chat_completions", Responses, Embeddings, Messages, Gemini}}}}
}
func TestNativeProtocolWireAndUsage(t *testing.T) {
	cases := []struct {
		protocol, body, path, reply string
		input, output               int64
	}{
		{"chat_completions", `{"model":"alias","messages":[{"role":"user","content":[{"type":"text","text":"weather"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aCb0AAAAASUVORK5CYII="}}]}],"max_completion_tokens":32,"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]}`, "/v1/chat/completions", `{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`, 5, 3},
		{Responses, `{"model":"alias","input":[{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aCb0AAAAASUVORK5CYII="}]}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"max_output_tokens":32}`, "/v1/responses", `{"object":"response","status":"completed","error":null,"output":[{"type":"function_call","name":"weather","call_id":"call-1","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,"input_tokens_details":{"cached_tokens":2}}}`, 5, 3},
		{Embeddings, `{"model":"alias","input":["one","two"],"dimensions":2}`, "/v1/embeddings", `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]},{"object":"embedding","index":1,"embedding":[0.3,0.4]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`, 4, 0},
		{Messages, `{"model":"alias","messages":[{"role":"user","content":[{"type":"text","text":"weather"}]}],"tools":[{"name":"weather","input_schema":{"type":"object"}}],"thinking":{"type":"enabled","budget_tokens":16},"max_tokens":32}`, "/v1/messages", `{"type":"message","content":[{"type":"tool_use","id":"tool-1","name":"weather","input":{}}],"usage":{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`, 8, 3},
		{Gemini, `{"contents":[{"role":"user","parts":[{"text":"weather"},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aCb0AAAAASUVORK5CYII="}}]}],"tools":[{"functionDeclarations":[{"name":"weather","parameters":{"type":"OBJECT"}}]}],"generationConfig":{"maxOutputTokens":32}}`, "/v1beta/models/upstream:generateContent", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"thoughtsTokenCount":1,"totalTokenCount":8},"modelVersion":"upstream"}`, 5, 3},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Header.Get("Cookie") != "" {
					t.Error("unexpected target or inherited caller headers")
				}
				if tc.protocol == Messages {
					if r.Header.Get("x-api-key") != "native-provider-secret" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
						t.Error("invalid native supplier authentication")
					}
				} else if tc.protocol == Gemini {
					if r.Header.Get("x-goog-api-key") != "native-provider-secret" || r.Header.Get("Authorization") != "" {
						t.Error("invalid Gemini authentication")
					}
				} else if r.Header.Get("Authorization") != "Bearer native-provider-secret" {
					t.Error("missing supplier credential")
				}
				raw, _ := io.ReadAll(r.Body)
				v, _ := nativeObject(raw)
				expected, _ := nativeObject([]byte(tc.body))
				if tc.protocol == Gemini {
					delete(expected, "model")
				} else {
					expected["model"] = "upstream"
				}
				if tc.protocol == Responses {
					expected["store"] = false
					expected["background"] = false
				}
				if !reflect.DeepEqual(v, expected) {
					t.Error("native tool, image, thinking or embedding fields changed in transit")
				}
				if tc.protocol != Gemini && v["model"] != "upstream" {
					t.Error("model not translated")
				}
				if tc.protocol == Responses && (v["store"] != false || v["background"] != false) {
					t.Error("response storage enabled")
				}
				if tc.protocol == Gemini && v["model"] != nil {
					t.Error("Gemini model left in body")
				}
				fmt.Fprint(w, tc.reply)
			}))
			defer upstream.Close()
			cfg := nativeConfig(upstream.URL)
			model := ""
			if tc.protocol == Gemini {
				model = "alias"
			}
			q, r, err := DecodeNative([]byte(tc.body), cfg, tc.protocol, model, false)
			if err != nil {
				t.Fatal(err)
			}
			r, _ = r.WithProviderCredential("native-provider-secret")
			w := httptest.NewRecorder()
			out := ForwardNative(context.Background(), w, q, r, 5*time.Second)
			if w.Code != 200 || out.Status != "completed" || out.Usage == nil || out.Usage.PromptTokens != tc.input || out.Usage.CompletionTokens != tc.output || !out.Dispatched {
				t.Fatalf("invalid native settlement: %+v / %s", out, w.Body)
			}
			var data map[string]any
			if json.Unmarshal(w.Body.Bytes(), &data) != nil {
				t.Fatal("invalid normalized JSON")
			}
			if tc.protocol != Gemini && data["model"] != "alias" {
				t.Fatal("private model alias leaked")
			}
		})
	}
}

func TestNativeProtocolAdmissionIsolation(t *testing.T) {
	cfg := nativeConfig("http://127.0.0.1:1")
	for _, tc := range []struct{ protocol, body string }{
		{"chat_completions", `{"model":"alias","messages":[{"role":"user","content":"a"}],"max_tokens":1,"temperature":3}`},
		{"chat_completions", `{"model":"alias","messages":[{"role":"user","content":"a","unknown":1}],"max_tokens":1}`},
		{"chat_completions", `{"model":"alias","messages":[{"role":"user","content":"a"}],"max_tokens":1,"max_completion_tokens":1}`},
		{Responses, `{"model":"alias","input":"a","max_output_tokens":1,"store":true}`},
		{Responses, `{"model":"alias","input":"a","max_output_tokens":1,"previous_response_id":"other-branch"}`},
		{Responses, `{"model":"alias","input":[{"type":"item_reference","id":"other-branch"}],"max_output_tokens":1}`},
		{Responses, `{"model":"alias","input":[{"role":"user","content":[{"type":"input_image","image_url":"http://internal/private"}]}],"max_output_tokens":1}`},
		{Responses, `{"model":"alias","input":"a","tools":[{"type":"web_search"}],"max_output_tokens":1}`},
		{Responses, `{"model":"alias","input":"a","max_output_tokens":65}`},
		{Embeddings, `{"model":"alias","input":"a","dimensions":0}`},
		{Embeddings, `{"model":"alias","input":"a","stream":true}`},
		{Messages, `{"model":"alias","messages":[{"role":"user","content":"a"}],"max_tokens":1,"tools":[{"type":"web_search_20250305","name":"web_search"}]}`},
		{Gemini, `{"contents":[{"parts":[{"fileData":{"fileUri":"gs://other-branch"}}]}],"generationConfig":{"maxOutputTokens":1}}`},
		{Gemini, `{"contents":[{"parts":[{"text":"a"}]}],"generationConfig":{"maxOutputTokens":1},"tools":[{"googleSearch":{}}]}`},
	} {
		if _, _, e := DecodeNative([]byte(tc.body), cfg, tc.protocol, "alias", false); e == nil {
			t.Fatalf("unsafe input accepted: %s", tc.body)
		}
	}
	legacy := *cfg
	legacy.Routes = append([]Route(nil), cfg.Routes...)
	legacy.Routes[0].Protocols = nil
	if _, _, e := DecodeNative([]byte(`{"model":"alias","input":"a","max_output_tokens":1}`), &legacy, Responses, "", false); e == nil {
		t.Fatal("unpublished capability accepted")
	}
	if ValidateProtocols([]string{Responses, Responses}) == nil || ValidateProtocols([]string{"arbitrary"}) == nil {
		t.Fatal("invalid capabilities admitted")
	}
}

func TestNativeProtocolStreams(t *testing.T) {
	cases := []struct {
		protocol, body, stream string
		wantInput, wantOutput  int64
	}{
		{"chat_completions", `{"model":"alias","messages":[{"role":"user","content":"a"}],"max_tokens":4,"stream":true,"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]}`, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n", 3, 2},
		{Responses, `{"model":"alias","input":"a","max_output_tokens":4,"stream":true}`, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"a\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"model\":\"upstream\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n", 3, 2},
		{Messages, `{"model":"alias","messages":[{"role":"user","content":"a"}],"max_tokens":4,"stream":true}`, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"upstream\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", 3, 2},
		{Gemini, `{"contents":[{"parts":[{"text":"a"}]}],"generationConfig":{"maxOutputTokens":4}}`, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n", 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.stream)
			}))
			defer upstream.Close()
			q, route, e := DecodeNative([]byte(tc.body), nativeConfig(upstream.URL), tc.protocol, "alias", true)
			if e != nil {
				t.Fatal(e)
			}
			w := httptest.NewRecorder()
			out := ForwardNative(context.Background(), w, q, route, 5*time.Second)
			if out.Status != "completed" || out.Usage == nil || out.Usage.PromptTokens != tc.wantInput || out.Usage.CompletionTokens != tc.wantOutput {
				t.Fatalf("stream usage %+v: %s", out, w.Body)
			}
			if tc.protocol == Responses && !strings.Contains(w.Body.String(), "event: response.output_text.delta") {
				t.Fatal("native event lost")
			}
		})
	}
}

func TestNativeProtocolUncertainOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, reply, content string
		stream               bool
	}{
		{"invalid-usage", `{"object":"response","status":"completed","output":[],"usage":{}}`, "application/json", false},
		{"missing-terminal", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n", "text/event-stream", true},
		{"malformed", `private provider detail`, "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.content)
				io.WriteString(w, tc.reply)
			}))
			defer upstream.Close()
			body := fmt.Sprintf(`{"model":"alias","input":"a","max_output_tokens":4,"stream":%t}`, tc.stream)
			q, route, _ := DecodeNative([]byte(body), nativeConfig(upstream.URL), Responses, "", false)
			w := httptest.NewRecorder()
			out := ForwardNative(context.Background(), w, q, route, 5*time.Second)
			if out.Usage != nil || !out.Dispatched || out.Status == "completed" || strings.Contains(w.Body.String(), "private provider detail") {
				t.Fatalf("unknown usage guessed or error leaked %+v", out)
			}
		})
	}
	q, route, _ := DecodeNative([]byte(`{"model":"alias","input":"a","max_output_tokens":4}`), nativeConfig("http://127.0.0.1:1"), Responses, "", false)
	out := ForwardNative(context.Background(), httptest.NewRecorder(), q, route, time.Second)
	if out.Dispatched || out.Usage != nil {
		t.Fatal("pre-dispatch failure misclassified")
	}
}

func TestNativeProtocolFinalUsageAndErrorPrivacy(t *testing.T) {
	for _, tc := range []struct {
		protocol, body, reply string
		stream                bool
		status                string
		known                 bool
	}{
		{Responses, `{"model":"alias","input":"a","max_output_tokens":4}`, `{"object":"response","status":"incomplete","error":null,"output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`, false, "incomplete", true},
		{Responses, `{"model":"alias","input":"a","max_output_tokens":4,"stream":true}`, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"private-provider-detail\"}}}\n\n", true, "incomplete", false},
		{Messages, `{"model":"alias","messages":[{"role":"user","content":"a"}],"max_tokens":4,"stream":true}`, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", true, "usage_unknown", false},
		{Gemini, `{"contents":[{"parts":[{"text":"a"}]}],"generationConfig":{"maxOutputTokens":4}}`, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1,\"totalTokenCount\":4}}\n\n", true, "incomplete", false},
	} {
		t.Run(tc.protocol+tc.status, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				io.WriteString(w, tc.reply)
			}))
			defer peer.Close()
			q, route, e := DecodeNative([]byte(tc.body), nativeConfig(peer.URL), tc.protocol, "alias", tc.stream)
			if e != nil {
				t.Fatal(e)
			}
			w := httptest.NewRecorder()
			out := ForwardNative(context.Background(), w, q, route, time.Second)
			if out.Status != tc.status || (out.Usage != nil) != tc.known || strings.Contains(w.Body.String(), "private-provider-detail") {
				t.Fatalf("terminal usage or error handling: %+v", out)
			}
		})
	}
}
