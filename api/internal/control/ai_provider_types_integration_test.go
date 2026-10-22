package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

func TestAIProviderNativeDiscoveryLifecycle(t *testing.T) {
	for _, kind := range []string{"anthropic", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			f := newProviderFixture(t)
			var gets, posts atomic.Int32
			model, protocol := "claude-native", aigateway.Messages
			if kind == "gemini" {
				model, protocol = "gemini-native", aigateway.Gemini
			}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "" {
					t.Error("native service received legacy authentication")
				}
				if kind == "anthropic" {
					if r.Header.Get("x-api-key") != providerTestKey || r.Header.Get("anthropic-version") != "2023-06-01" {
						t.Error("native credential missing")
					}
				} else if r.Header.Get("x-goog-api-key") != providerTestKey {
					t.Error("Gemini credential missing")
				}
				if r.Method == "GET" {
					gets.Add(1)
					if kind == "anthropic" {
						if r.URL.Path != "/v1/models" {
							t.Error("Anthropic discovery path")
						}
						fmt.Fprint(w, `{"data":[{"type":"model","id":"claude-native"}],"has_more":false}`)
					} else {
						if r.URL.Path != "/v1beta/models" {
							t.Error("Gemini discovery path")
						}
						fmt.Fprint(w, `{"models":[{"name":"models/gemini-native"}]}`)
					}
					return
				}
				posts.Add(1)
				if kind == "anthropic" {
					if r.URL.Path != "/v1/messages" {
						t.Error("Messages path")
					}
					fmt.Fprint(w, `{"type":"message","content":[{"type":"text","text":"native-profile-result"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
				} else {
					if r.URL.Path != "/v1beta/models/gemini-native:generateContent" {
						t.Error("Gemini inference path")
					}
					fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"native-profile-result"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`)
				}
			}))
			defer peer.Close()
			f.s.aiProviders = providerSettingsFixture(t, peer.URL+"/v1", true)
			base := "/api/v1/platform/ai-providers"
			actor := "provider_operator"
			input := f.draft(0, "sealed", providerTestKey)
			input.BaseURL = peer.URL + "/v1"
			input.ProviderType = kind
			created := f.call(actor, "POST", base, "native-create-"+kind, input, 202)
			id, revision := stringVal(created["provider_id"]), stringVal(created["revision_id"])
			if gets.Load() != 0 || posts.Load() != 0 {
				t.Fatal("draft performed I/O")
			}
			f.work()
			if gets.Load() != 1 || posts.Load() != 0 {
				t.Fatal("discovery was inference")
			}
			var stored string
			if f.s.db.QueryRow(f.ctx, `SELECT provider_type FROM ai_provider_revisions WHERE id=$1`, revision).Scan(&stored) != nil || stored != kind {
				t.Fatal("profile not fixed in revision")
			}
			pub := aiProviderPublishInput{Version: 1, RevisionID: revision, Models: []aiProviderModel{{ID: "native-published", Name: "Native published", UpstreamModel: model, MaxOutputTokens: 32, Protocols: []string{aigateway.Responses}}}}
			f.call(actor, "POST", base+"/"+id+"/publish", "wrong-native-protocol", pub, 422)
			pub.Models[0].Protocols = []string{protocol}
			f.call(actor, "POST", base+"/"+id+"/publish", "native-publication", pub, 200)
			snapshot, err := f.s.aiCatalog(f.ctx, true)
			if err != nil {
				t.Fatal(err)
			}
			route, ok := snapshot.Config.Find("native-published")
			if !ok || route.ProviderType != kind || !route.Supports(protocol) {
				t.Fatal("native approval not retained in runtime")
			}
			body := []byte(`{"model":"native-published","messages":[{"role":"user","content":"native test"}],"max_tokens":8}`)
			path := "/ai/v1/br_provider/v1/messages"
			if kind == "gemini" {
				body = []byte(`{"contents":[{"parts":[{"text":"native test"}]}],"generationConfig":{"maxOutputTokens":8}}`)
				path = "/ai/v1/br_provider/gemini/v1beta/models/native-published:generateContent"
			}
			r := httptest.NewRequest("POST", path, bytes.NewReader(body))
			r.SetPathValue("branch", "br_provider")
			if kind == "gemini" {
				r.SetPathValue("aiModelAction", "native-published:generateContent")
			}
			r.Header.Set("X-Neon-Console-Ticket", f.s.signAIProtocolTicket(actor, "br_provider", "native-published", body, snapshot.Config.Generation, protocol))
			w := httptest.NewRecorder()
			f.s.aiPublic(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "native-profile-result") || posts.Load() != 1 {
				t.Fatalf("published native route %d %s", w.Code, w.Body)
			}
			var tokens int64
			if f.s.db.QueryRow(f.ctx, `SELECT total_tokens FROM ai_gateway_requests`).Scan(&tokens) != nil || tokens != 5 {
				t.Fatal("native usage not settled")
			}
			reuse := input
			reuse.Version = 2
			reuse.ProviderType = "openai_compatible"
			reuse.Credential = aiProviderCredentialInput{Mode: "reuse", ReuseRevisionID: revision}
			f.call(actor, "POST", base+"/"+id+"/revisions", "cross-type-reuse-rejected", reuse, 422)
			if gets.Load() != 1 || posts.Load() != 1 {
				t.Fatal("rejected reuse invoked provider")
			}
		})
	}
}

func TestAIProviderNativeTypeImmutabilityAndLegacyReplay(t *testing.T) {
	f := newProviderFixture(t)
	input := f.draft(0, "none", "")
	raw, _ := json.Marshal(input)
	if bytes.Contains(raw, []byte("provider_type")) {
		t.Fatal("legacy keyed request changed")
	}
	base := "/api/v1/platform/ai-providers"
	created := f.call("provider_operator", "POST", base, "legacy-profile-replay", input, 202)
	replay := f.request("provider_operator", "POST", base, "legacy-profile-replay", input)
	if replay.Code != 202 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("original omitted type replay broken")
	}
	revision := stringVal(created["revision_id"])
	var kind string
	if f.s.db.QueryRow(f.ctx, `SELECT provider_type FROM ai_provider_revisions WHERE id=$1`, revision).Scan(&kind) != nil || kind != "openai_compatible" {
		t.Fatal("historical profile reinterpreted")
	}
	if _, err := f.s.db.Exec(f.ctx, `UPDATE ai_provider_revisions SET provider_type='anthropic' WHERE id=$1`, revision); err == nil {
		t.Fatal("pending revision profile mutable")
	}
	invalid := input
	invalid.ProviderType = "gemini"
	f.call("provider_operator", "POST", base, "native-no-key-rejected", invalid, 422)
	invalid.ProviderType = "arbitrary"
	f.call("provider_operator", "POST", base, "unknown-type-rejected", invalid, 422)
	if f.gets.Load() != 0 || f.posts.Load() != 0 {
		t.Fatal("invalid native profile reached upstream")
	}
}
