package functions

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func trustedClientFixture(t *testing.T, handler http.HandlerFunc, stop func(context.Context) error) (*InstanceClient, *Manager, Bootstrap) {
	t.Helper()
	b := bootstrapFixture(t)
	key, _ := secretKey(b.ManagerKey)
	node := httptest.NewServer(handler)
	t.Cleanup(node.Close)
	if stop == nil {
		stop = func(context.Context) error { return nil }
	}
	m, err := NewManager(b.Scope, key, node.URL, stop)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(m)
	server.TLS, err = b.ManagerTLS(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	c, err := NewInstanceClient(b.Scope, netip.MustParseAddrPort("127.0.0.1:9090"), key, b.ManagerCA)
	if err != nil {
		t.Fatal(err)
	}
	// Only tests redirect the fixed target to the random isolated TLS listener.
	// Certificate hostname, TLS version, chain, MAC and real HTTP remain intact.
	c.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	t.Cleanup(c.Close)
	return c, m, b
}

func readyNode(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_neon/status" {
		_, _ = io.WriteString(w, `{"ready":true,"background_pending":17}`)
		return
	}
	w.Header().Set("X-App-Response", "retained")
	w.Header().Set("X-Neon-Private", "discarded")
	_, _ = io.Copy(w, r.Body)
}

func TestInstanceClientTrustedTLSAndOriginalResponse(t *testing.T) {
	c, m, _ := trustedClientFixture(t, readyNode, nil)
	status, err := c.Status(context.Background())
	if err != nil || status.Scope != m.scope || status.BootID != m.bootID || !status.NodeReady || status.BackgroundCountersTrusted {
		t.Fatal("trusted supervisor observation unavailable", err)
	}
	v := invocationFor(m)
	v.Body = []byte("real application body")
	r, err := c.Invoke(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil || r.StatusCode != 200 || string(body) != "real application body" || r.Header.Get("X-App-Response") != "retained" || r.Header.Get("X-Neon-Private") != "" || r.TLS == nil || r.TLS.Version != tls.VersionTLS13 {
		t.Fatal("response or trusted transport changed")
	}
}

func TestInstanceClientStreamsAndNeverFollowsAppRedirect(t *testing.T) {
	c, m, _ := trustedClientFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_neon/status" {
			readyNode(w, r)
			return
		}
		if r.Header.Get("X-App") == "redirect" {
			w.Header().Set("Location", "http://169.254.169.254/private")
			w.WriteHeader(302)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		time.Sleep(180 * time.Millisecond)
		_, _ = io.WriteString(w, "last\n")
	}, nil)
	v := invocationFor(m)
	v.Headers = map[string][]string{}
	r, err := c.Invoke(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 6)
	_, err = io.ReadFull(r.Body, first)
	at := time.Now()
	rest, readErr := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || readErr != nil || string(first) != "first\n" || string(rest) != "last\n" || time.Since(at) < 120*time.Millisecond {
		t.Fatal("stream was buffered")
	}
	v.Headers = map[string][]string{"X-App": {"redirect"}}
	r, err = c.Invoke(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 302 || r.Header.Get("Location") != "http://169.254.169.254/private" {
		t.Fatal("application redirect followed or changed")
	}
}

func TestInstanceClientRetainsBootAcrossShutdownRetry(t *testing.T) {
	var calls atomic.Int32
	c, m, _ := trustedClientFixture(t, readyNode, func(context.Context) error { calls.Add(1); return nil })
	if c.Shutdown(context.Background(), strings.Repeat("0", 32)) == nil || calls.Load() != 0 {
		t.Fatal("old boot stopped current instance")
	}
	if err := c.Shutdown(context.Background(), m.bootID); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(context.Background(), m.bootID); err != nil || calls.Load() != 1 {
		t.Fatal("completed shutdown was not idempotent", err)
	}
}

func TestInstanceClientRejectsWrongScopeTrustAndSigningKey(t *testing.T) {
	c, m, _ := trustedClientFixture(t, readyNode, nil)
	c.scope.BranchID = "br_0000000000000009"
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("cross-branch status accepted")
	}
	c.scope = m.scope
	c.transport.TLSClientConfig.RootCAs, _ = trustPool(bootstrapFixture(t).ManagerCA)
	c.transport.CloseIdleConnections()
	if _, err := c.Status(context.Background()); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatal("untrusted certificate accepted or private transport disclosed")
	}
	c, m, b := trustedClientFixture(t, readyNode, nil)
	_ = m
	c.key = []byte(strings.Repeat("x", 32))
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("wrong signing key accepted")
	}
	key, _ := secretKey(b.ManagerKey)
	for _, target := range []string{"192.0.2.2:9090", "10.42.0.1:443", "[::1]:9090", "0.0.0.0:9090"} {
		if _, err := NewInstanceClient(b.Scope, netip.MustParseAddrPort(target), key, b.ManagerCA); err == nil {
			t.Fatal("unadmitted network target accepted")
		}
	}
}

func TestInstanceClientChecksInvocationBeforeTransport(t *testing.T) {
	c, m, _ := trustedClientFixture(t, readyNode, nil)
	for _, path := range []string{"/functions/v1/" + m.scope.BranchID + "/" + m.scope.Slug + "/../other", "/functions/v1/br_0000000000000009/hello", "/functions/v1/" + m.scope.BranchID + "/hello/%2e%2e/other"} {
		v := invocationFor(m)
		v.URL = "https://functions.example.test" + path
		if r, err := c.Invoke(context.Background(), v); err == nil || r != nil {
			t.Fatal("out-of-scope invocation transported")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Status(ctx); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatal("cancelled request continued or disclosed address")
	}
}
