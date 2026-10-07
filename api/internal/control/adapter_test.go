package control

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestAdapterReadRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"connection_reset", &url.Error{Op: "Get", Err: syscall.ECONNRESET}, true},
		{"connection_refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"network_timeout", &net.DNSError{IsTimeout: true}, true},
		{"untrusted_ca", &url.Error{Op: "Get", Err: x509.UnknownAuthorityError{}}, false},
		{"wrong_hostname", x509.HostnameError{}, false},
		{"invalid_cert", x509.CertificateInvalidError{}, false},
		{"http_503", kubeError{Status: 503}, false},
		{"invalid_json", errors.New("invalid JSON"), false},
		{"cancelled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if transientAdapterRead(tc.err) != tc.retry {
				t.Fatal("retry boundary incorrect")
			}
		})
	}
}
func TestAdapterKubeMutationsNeverTransportReplay(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				hijacker := w.(http.Hijacker)
				conn, _, _ := hijacker.Hijack()
				_ = conn.Close()
			}))
			defer peer.Close()
			client := adapterKubernetes{&kubeClient{base: peer.URL, http: peer.Client()}}
			if _, err := client.Request(context.Background(), method, "/uncertain", map[string]any{"intent": true}); err == nil {
				t.Fatal("uncertain mutation hidden")
			}
			if calls.Load() != 1 {
				t.Fatal("transport replayed mutation")
			}
		})
	}
}
