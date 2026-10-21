package aigateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderNetworkPublicDNSPrivateOriginsAndMetadataDenial(t *testing.T) {
	p, err := NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: "http://192.168.146.100:11437/v1", AllowedIPs: []string{"192.168.146.100"}, LabHTTP: true}})
	if err != nil {
		t.Fatal(err)
	}
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	for _, value := range []string{"https://API.Example:443/v1/", "http://192.168.146.100:11437/v1"} {
		result, err := p.Resolve(context.Background(), value)
		if err != nil || len(result.EndpointSHA256) != 64 || p.ValidateResolved(result) != nil {
			t.Fatal("approved endpoint rejected", err)
		}
	}
	for _, value := range []string{"http://192.168.146.100:11436/v1", "http://api.example/v1", "https://127.0.0.1/v1", "https://10.0.0.1/v1", "https://169.254.169.254/v1", "https://100.100.100.200/v1", "https://[::ffff:169.254.169.254]/v1", "https://[fd00::1]/v1", "https://[fe80::1]/v1", "https://[64:ff9b::a9fe:a9fe]/v1", "https://api.example:6443/v1", "https://secret@api.example/v1", "https://api.example/v1?x=1", "https://api.example/v1?", "https://api.example/v1#secret", "https://api.example/%76%31", "https://api.example/v1/../admin", "https://api.example:0/v1"} {
		if _, err := p.Resolve(context.Background(), value); err == nil {
			t.Fatal("unapproved origin or special endpoint accepted", value)
		}
	}
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("192.168.146.100")}, nil
	}
	if _, err := p.Resolve(context.Background(), "https://api.example/v1"); err == nil {
		t.Fatal("mixed public/private DNS answer accepted")
	}
	for _, ip := range []string{"169.254.169.254", "100.100.100.200", "0.0.0.0", "224.0.0.1"} {
		if _, err := NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: "http://metadata.example:1234/v1", AllowedIPs: []string{ip}, LabHTTP: true}}); err == nil {
			t.Fatal("operator entry allowed metadata or special network")
		}
	}
}

func TestProviderModelDiscoveryAuthenticationBoundsAndNoRedirect(t *testing.T) {
	const secret = "disposable-provider-secret-0123456789"
	var mode atomic.Int32
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("Cookie") != "" {
			t.Error("provider probe shape or credential rejected")
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 1:
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"`+secret+`"}`)
		case 2:
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
			w.WriteHeader(302)
		case 3:
			io.WriteString(w, strings.Repeat("x", (1<<20)+1))
		case 4:
			io.WriteString(w, `{"object":"list","data":[{"id":"`+secret+`"}]}`)
		case 5:
			io.WriteString(w, `{"object":"list","data":[]}`)
		default:
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "ignored": "future metadata", "data": []map[string]string{{"id": "qwen2.5:1.5b"}, {"id": "qwen2.5:0.5b"}, {"id": "qwen2.5:0.5b"}}})
		}
	}))
	defer peer.Close()
	p, err := NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: peer.URL + "/v1", AllowedIPs: []string{"127.0.0.1"}, LabHTTP: true}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := p.Resolve(context.Background(), peer.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.DiscoverModels(context.Background(), endpoint, secret, 5*time.Second)
	if err != nil || strings.Join(models, ",") != "qwen2.5:0.5b,qwen2.5:1.5b" {
		t.Fatal("actual model metadata was not sorted and deduplicated", err)
	}
	for _, item := range []struct {
		mode int32
		code string
	}{{1, "provider_authentication_rejected"}, {2, "provider_redirect_rejected"}, {3, "provider_model_metadata_limit"}, {4, "provider_model_metadata_rejected"}} {
		mode.Store(item.mode)
		_, err = p.DiscoverModels(context.Background(), endpoint, secret, 5*time.Second)
		typed, ok := err.(ProviderProbeError)
		if !ok || typed.Code != item.code || strings.Contains(err.Error(), secret) {
			t.Fatal("typed provider failure rejected or leaked body", err)
		}
	}
	mode.Store(5)
	models, err = p.DiscoverModels(context.Background(), endpoint, secret, 5*time.Second)
	if err != nil || len(models) != 0 {
		t.Fatal("empty model service invented a model")
	}
	if calls.Load() != 6 {
		t.Fatal("metadata request was replayed or redirect followed")
	}
	tlsPeer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS probe reached handler") }))
	defer tlsPeer.Close()
	p, err = NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: tlsPeer.URL + "/v1", AllowedIPs: []string{"127.0.0.1"}}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err = p.Resolve(context.Background(), tlsPeer.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.DiscoverModels(context.Background(), endpoint, secret, 5*time.Second)
	typed, ok := err.(ProviderProbeError)
	if !ok || typed.Code != "provider_tls_verification_failed" {
		t.Fatal("untrusted provider TLS did not fail closed", err)
	}
}
