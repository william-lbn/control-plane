package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// 真实TCP/SOCKS握手及TLS，后端是协议夹具；不把本测试称为真实集群E2E。
func serviceSOCKSFixture(t *testing.T, target string, stall bool) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	seen, done := make(chan string, 1), make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		header := make([]byte, 3)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		if header[0] != 5 || header[1] != 1 || header[2] != 0 {
			t.Error("unexpected SOCKS method")
			return
		}
		if stall {
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
		request := make([]byte, 4)
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		if request[0] != 5 || request[1] != 1 || request[2] != 0 || request[3] != 1 {
			t.Error("expected exact IPv4 CONNECT")
			return
		}
		body := make([]byte, 6)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		seen <- net.JoinHostPort(net.IP(body[:4]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(body[4:]))))
		upstream, err := net.DialTimeout("tcp", target, time.Second)
		if err != nil {
			t.Error("fixture target unavailable")
			return
		}
		defer upstream.Close()
		if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
			return
		}
		copied := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close(); close(copied) }()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
		<-copied
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("tunnel fixture did not finish")
		}
	})
	return listener.Addr().String(), seen
}

func TestInternalServiceTunnelConfigurationAndNoGlobalProxy(t *testing.T) {
	for _, value := range []string{"localhost:8080", "198.51.100.1:8080", "http://127.0.0.1:8080", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:00080", "127.0.0.1:+80", "127.0.0.1:80/path"} {
		t.Setenv("NEON_INTERNAL_SOCKS5_ADDR", value)
		if _, err := internalServiceSOCKS5(); err == nil {
			t.Fatal("untrusted tunnel endpoint accepted")
		}
	}
	for _, value := range []string{"", "127.0.0.1:18080", "[::1]:18080"} {
		t.Setenv("NEON_INTERNAL_SOCKS5_ADDR", value)
		if got, err := internalServiceSOCKS5(); err != nil || got != value {
			t.Fatal("valid explicit endpoint rejected", err)
		}
	}
	t.Setenv("NEON_INTERNAL_SOCKS5_ADDR", "")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	transport := &http.Transport{DialContext: dialInternalService}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get(target.URL)
	if err != nil {
		t.Fatal("unconfigured production path changed", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal("direct request failed")
	}
}

func TestInternalServiceTunnelRetainsTLSNameAndNativeCredential(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.ServerName != "example.com" || r.Header.Get("Authorization") != "Bearer synthetic-native-credential" {
			t.Error("tunnel changed TLS identity or native credential")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(204)
	}))
	defer target.Close()
	address, seen := serviceSOCKSFixture(t, target.Listener.Addr().String(), false)
	t.Setenv("NEON_INTERNAL_SOCKS5_ADDR", address)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	transport := &http.Transport{DialContext: dialInternalService, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, "https://198.51.100.42:8443/fixed-read", nil)
	request.Header.Set("Authorization", "Bearer synthetic-native-credential")
	response, err := (&http.Client{Transport: transport, Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatal("bounded tunnel request failed", err)
	}
	response.Body.Close()
	if response.StatusCode != 204 || <-seen != "198.51.100.42:8443" {
		t.Fatal("original Service target changed")
	}
}

func TestInternalServiceTunnelCancellationAndNoFallback(t *testing.T) {
	address, _ := serviceSOCKSFixture(t, "", true)
	t.Setenv("NEON_INTERNAL_SOCKS5_ADDR", address)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if conn, err := dialInternalService(ctx, "tcp", "198.51.100.42:8443"); err == nil {
		conn.Close()
		t.Fatal("stalled handshake did not honor cancellation")
	}
	// 已知关闭的本地代理不能悄悄改为直连仍可达的目标。
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	closed.Close()
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	t.Setenv("NEON_INTERNAL_SOCKS5_ADDR", closedAddr)
	if conn, err := dialInternalService(context.Background(), "tcp", target.Addr().String()); err == nil {
		conn.Close()
		t.Fatal("tunnel failure fell back to direct")
	}
}
