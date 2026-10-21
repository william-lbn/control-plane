// ai-provider-relay is an optional laboratory transport for an Ollama server
// reached through an SSH reverse tunnel. It is not a model or a production TLS
// terminator. No prompt, completion or Authorization header is logged.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func newRelayHandler(key string, target *url.URL) (http.Handler, error) {
	if len(key) < 32 || len(key) > 8192 || strings.ContainsAny(key, "\r\n") || target == nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" || target.Port() == "" || target.User != nil || target.Path != "" || target.RawPath != "" || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return nil, errors.New("private loopback relay configuration rejected")
	}
	fixed := *target
	proxy := &httputil.ReverseProxy{Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(&fixed)
		p.Out.Header.Del("Authorization")
		p.Out.Header.Del("Cookie")
	}, FlushInterval: -1,
		Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 180 * time.Second}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			http.Error(w, "provider transport unavailable", 502)
		},
		ModifyResponse: func(r *http.Response) error {
			if r.Request.Method != http.MethodGet {
				return nil
			}
			defer r.Body.Close()
			body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
			if err != nil || len(body) > 1<<20 {
				return errors.New("provider model metadata exceeds limit")
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.TransferEncoding = nil
			r.Header.Del("Transfer-Encoding")
			r.Header.Set("Content-Length", fmt.Sprint(len(body)))
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+key)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models" && r.ContentLength == 0 && len(r.TransferEncoding) == 0:
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, "request body exceeds limit", 413)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		default:
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}), nil
}

func main() {
	raw, e := os.ReadFile(os.Getenv("AI_RELAY_KEY_FILE"))
	key := strings.TrimSpace(string(raw))
	if e != nil || len(key) < 32 || strings.ContainsAny(key, "\r\n") {
		os.Exit(2)
	}
	target, e := url.Parse(os.Getenv("AI_RELAY_UPSTREAM"))
	if e != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" || target.User != nil || target.RawQuery != "" || target.Path != "" {
		os.Exit(2)
	}
	handler, e := newRelayHandler(key, target)
	if e != nil {
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: os.Getenv("AI_RELAY_BIND"), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 190 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	if server.Addr == "" {
		os.Exit(2)
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	case e := <-done:
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			io.WriteString(os.Stderr, "provider relay listener failed\n")
			os.Exit(1)
		}
	}
}
