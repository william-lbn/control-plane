package aigateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderEgressPublicProxyPrivateDirectAndLiteralCONNECT(t *testing.T) {
	p, err := NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: "http://192.168.146.100:11437/v1", AllowedIPs: []string{"192.168.146.100"}, LabHTTP: true}, {BaseURL: "https://192.168.146.100/v1", AllowedIPs: []string{"192.168.146.100"}}})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := p.WithEgressProxy("http://192.168.146.1:11121")
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"http://192.168.146.100:11437/v1", "https://192.168.146.100/v1", "https://1.1.1.1/v1"} {
		endpoint, err := policy.Resolve(context.Background(), base)
		if err != nil {
			t.Fatal(err)
		}
		route, err := policy.ConfigureRoute(endpoint, Route{}, "")
		if err != nil {
			t.Fatal(err)
		}
		expected := strings.Contains(base, "1.1.1.1")
		if (route.egressProxy != "") != expected {
			t.Fatal("public/private egress policy diverged")
		}
	}
	for _, value := range []string{"http://169.254.169.254:80", "http://100.100.100.200:80", "http://1.1.1.1:80", "http://user:secret@127.0.0.1:1", "socks5://127.0.0.1:1", "http://127.0.0.1:1/path", "http://127.0.0.1:1?x=1", "http://127.0.0.1:1?", "http://127.0.0.1:1#x"} {
		if _, err = p.WithEgressProxy(value); err == nil {
			t.Fatal("unapproved proxy configuration accepted")
		}
	}
	var connects atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer disposable-proxy-key-123456" {
			t.Error("provider TLS authorization missing")
		}
		fmt.Fprint(w, strings.Repeat("x", 65536))
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("provider credential entered proxy CONNECT headers")
			w.WriteHeader(403)
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || net.ParseIP(host) == nil {
			t.Error("proxy was asked to resolve provider DNS")
			w.WriteHeader(403)
			return
		}
		peer, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			peer.Close()
			return
		}
		defer client.Close()
		defer peer.Close()
		connects.Add(1)
		fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(client, peer)
		io.Copy(peer, buffer)
	}))
	defer proxy.Close()
	route := Route{BaseURL: upstream.URL + "/v1", AllowedIPs: []string{"127.0.0.1"}, egressProxy: proxy.URL}
	client := Client(route, 5*time.Second)
	client.Transport.(*http.Transport).TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest("GET", upstream.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer disposable-proxy-key-123456")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("trusted CONNECT tunnel failed", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || len(body) != 65536 || connects.Load() != 1 {
		t.Fatal("tunnel buffering or TLS failed", err)
	}
	// Real private request ignores both an approved egress proxy and env proxies.
	privatePolicy, err := NewProviderNetworkPolicy([]ApprovedProviderEndpoint{{BaseURL: upstream.URL + "/v1", AllowedIPs: []string{"127.0.0.1"}}})
	if err != nil {
		t.Fatal(err)
	}
	privatePolicy, err = privatePolicy.WithEgressProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := privatePolicy.Resolve(context.Background(), upstream.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	directRoute, err := privatePolicy.ConfigureRoute(endpoint, Route{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	direct := Client(directRoute, 5*time.Second)
	direct.Transport.(*http.Transport).TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer direct.CloseIdleConnections()
	response, err = direct.Do(req)
	if err != nil {
		t.Fatal("private request incorrectly used proxy", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if connects.Load() != 1 {
		t.Fatal("private request reached public egress proxy")
	}
}
