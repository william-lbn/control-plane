// ai-provider-relay is an optional laboratory transport for an Ollama server
// reached through an SSH reverse tunnel. It is not a model or a production TLS
// terminator. No prompt, completion or Authorization header is logged.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
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
	proxy := &httputil.ReverseProxy{Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(target)
		p.Out.Header.Del("Authorization")
		p.Out.Header.Del("Cookie")
	}, FlushInterval: -1, Transport: &http.Transport{ResponseHeaderTimeout: 180 * time.Second}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
		http.Error(w, "provider transport unavailable", 502)
	}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+key)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 65536)
		proxy.ServeHTTP(w, r)
	})
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
