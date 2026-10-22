package aigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeProviderDiscoveryWireAndPagination(t *testing.T) {
	for _, kind := range []string{"anthropic", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			const secret = "native-discovery-disposable-key"
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.Query().Get("key") != "" {
					t.Error("discovery leaked credentials or invoked inference")
				}
				w.Header().Set("Content-Type", "application/json")
				if kind == "anthropic" {
					if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != secret || r.Header.Get("x-goog-api-key") != "" || r.Header.Get("anthropic-version") != "2023-06-01" || r.URL.Query().Get("limit") != "100" {
						t.Error("Anthropic discovery wire mismatch")
					}
					if r.URL.Query().Get("after_id") == "" {
						fmt.Fprint(w, `{"data":[{"type":"model","id":"claude-z"}],"has_more":true,"last_id":"claude-z"}`)
					} else {
						if r.URL.Query().Get("after_id") != "claude-z" {
							t.Error("native cursor lost")
						}
						fmt.Fprint(w, `{"data":[{"type":"model","id":"claude-a"}],"has_more":false,"last_id":"claude-a"}`)
					}
				} else {
					if r.URL.Path != "/v1beta/models" || r.Header.Get("x-goog-api-key") != secret || r.Header.Get("x-api-key") != "" || r.URL.Query().Get("pageSize") != "100" {
						t.Error("Gemini discovery wire mismatch")
					}
					if r.URL.Query().Get("pageToken") == "" {
						fmt.Fprint(w, `{"models":[{"name":"models/gemini-z"}],"nextPageToken":"opaque+/=&cursor"}`)
					} else {
						if r.URL.Query().Get("pageToken") != "opaque+/=&cursor" {
							t.Error("opaque cursor not escaped")
						}
						fmt.Fprint(w, `{"models":[{"name":"models/gemini-a"},{"name":"models/gemini-z"}]}`)
					}
				}
			}))
			defer peer.Close()
			policy, err := NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: peer.URL + "/v1", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true}})
			if err != nil {
				t.Fatal(err)
			}
			endpoint, err := policy.Resolve(context.Background(), peer.URL+"/v1")
			if err != nil {
				t.Fatal(err)
			}
			models, err := policy.DiscoverProviderModels(context.Background(), endpoint, secret, 5*time.Second, kind)
			prefix := "claude"
			if kind == "gemini" {
				prefix = "gemini"
			}
			if err != nil || strings.Join(models, ",") != prefix+"-a,"+prefix+"-z" || calls.Load() != 2 {
				t.Fatal("native bounded catalog not preserved", err, models, calls.Load())
			}
		})
	}
}

func TestNativeProviderDiscoveryRejectsPartialAndPrivateMetadata(t *testing.T) {
	const secret = "native-discovery-private-key"
	for _, tc := range []struct {
		name, kind, body, code string
		status                 int
	}{
		{"auth", "anthropic", `{"error":"` + secret + `"}`, "provider_authentication_rejected", 401},
		{"redirect", "gemini", `{}`, "provider_redirect_rejected", 302},
		{"missing_final_flag", "anthropic", `{"data":[]}`, "provider_model_metadata_rejected", 200},
		{"invalid_cursor", "anthropic", `{"data":[{"id":"claude","type":"model"}],"has_more":true,"last_id":"other"}`, "provider_model_metadata_rejected", 200},
		{"loop", "gemini", `{"models":[{"name":"models/gemini"}],"nextPageToken":"same"}`, "provider_model_pagination_rejected", 200},
		{"foreign_resource", "gemini", `{"models":[{"name":"projects/secret/models/gemini"}]}`, "provider_model_metadata_rejected", 200},
		{"secret_model", "anthropic", `{"data":[{"id":"` + secret + `","type":"model"}],"has_more":false}`, "provider_model_metadata_rejected", 200},
		{"unbounded", "gemini", strings.Repeat("x", (1<<20)+1), "provider_model_metadata_limit", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Location", "http://169.254.169.254/latest")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer peer.Close()
			p, _ := NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: peer.URL + "/v1", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true}})
			endpoint, _ := p.Resolve(context.Background(), peer.URL+"/v1")
			models, err := p.DiscoverProviderModels(context.Background(), endpoint, secret, 5*time.Second, tc.kind)
			probe, ok := err.(ProviderProbeError)
			if !ok || probe.Code != tc.code || len(models) != 0 || strings.Contains(err.Error(), secret) {
				t.Fatal("partial catalog accepted or unsafe error", err, models)
			}
			want := int32(1)
			if tc.name == "loop" {
				want = 2
			}
			if calls.Load() != want {
				t.Fatal("redirect or retry occurred", calls.Load())
			}
		})
	}
}

func TestNativeProviderTypesAndBoundedCatalog(t *testing.T) {
	if ProviderType("") != "openai_compatible" || ProviderType("arbitrary") != "" || !ProviderProtocolsValid("anthropic", []string{Messages}) || ProviderProtocolsValid("gemini", nil) || ProviderProtocolsValid("anthropic", []string{Messages, Responses}) || ProviderProtocolsValid("gemini", []string{Messages}) {
		t.Fatal("provider protocol/profile mismatch accepted")
	}
	const secret = "bounded-catalog-disposable-key"
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		models := []map[string]string{}
		for i := 0; i < 100; i++ {
			models = append(models, map[string]string{"name": fmt.Sprintf("models/gemini-%d-%d", page, i)})
		}
		json.NewEncoder(w).Encode(map[string]any{"models": models, "nextPageToken": fmt.Sprint(page)})
	}))
	defer peer.Close()
	p, _ := NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: peer.URL + "/v1", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true}})
	endpoint, _ := p.Resolve(context.Background(), peer.URL+"/v1")
	models, err := p.DiscoverProviderModels(context.Background(), endpoint, secret, 5*time.Second, "gemini")
	if err == nil || len(models) != 0 || calls.Load() != 3 {
		t.Fatal("oversized catalog silently truncated", err, calls.Load())
	}
	prior := calls.Load()
	for _, kind := range []string{"arbitrary", "anthropic"} {
		key := secret
		if kind == "anthropic" {
			key = ""
		}
		if _, err := p.DiscoverProviderModels(context.Background(), endpoint, key, 5*time.Second, kind); err == nil {
			t.Fatal("invalid metadata credential/profile admitted")
		}
	}
	if calls.Load() != prior {
		t.Fatal("invalid profile reached provider")
	}
}
