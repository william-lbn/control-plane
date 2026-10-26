package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functionedge"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

func functionEdgeEnabled() bool { return os.Getenv("NEON_FUNCTIONS_EDGE_ENABLED") == "true" }

// The broker is a private control listener, never mounted on Console/public
// routes. The only admitted client identity is the independent edge leaf.
func (s *server) newFunctionEdgeBrokerServer() (*http.Server, error) {
	if s.functions == nil || functionPublicOrigin() == "" || validateFunctionPublicOrigin() != nil {
		return nil, errFunctionsPlatform
	}
	cert, err := tls.LoadX509KeyPair(os.Getenv("NEON_FUNCTIONS_EDGE_BROKER_CERT_FILE"), os.Getenv("NEON_FUNCTIONS_EDGE_BROKER_KEY_FILE"))
	if err != nil {
		return nil, errors.New("function edge broker identity unavailable")
	}
	ca, err := functionPrivateFile(os.Getenv("NEON_FUNCTIONS_EDGE_CLIENT_CA_FILE"), 64<<10)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("function edge client CA rejected")
	}
	return &http.Server{Addr: env("NEON_FUNCTIONS_EDGE_BROKER_BIND", ":8795"), Handler: s.functionEdgeBrokerHandler(),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"}},
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}, nil
}

func (s *server) functionEdgeBrokerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != "neon-function-edge" {
			http.Error(w, "edge identity rejected", 403)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != functionedge.BrokerPath || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		s.functionEdgeAdmission(w, r)
	})
}

func validEdgeAdmission(input functionedge.Admission) bool {
	u, err := url.ParseRequestURI(input.URI)
	prefix := "/functions/v1/" + input.Branch + "/" + input.Slug
	if err != nil || u.Scheme != "" || u.Host != "" || u.Fragment != "" || !(u.Path == prefix || strings.HasPrefix(u.Path, prefix+"/")) ||
		!functions.ValidSlug(input.Slug) || len(input.Body) > functions.MaxInvokeBytes {
		return false
	}
	if input.WebSocket {
		h := input.Headers.Clone()
		h.Set("Connection", "Upgrade")
		h.Set("Upgrade", "websocket")
		if len(input.Body) != 0 || !functions.ValidateWebSocketHandshake(input.Method, h) {
			return false
		}
	}
	v := functions.Invocation{BootID: strings.Repeat("0", 32), Generation: 1, Method: input.Method, URL: functionPublicOrigin() + input.URI,
		Headers: functionedge.IngressHeaders(input.Headers), Body: input.Body}
	return functions.ValidateInvocation(functions.Scope{BranchID: input.Branch, Slug: input.Slug, Generation: 1}, v)
}

func (s *server) functionEdgeAdmission(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	if controller.EnableFullDuplex() != nil {
		http.Error(w, "duplex admission required", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 19*time.Minute)
	defer cancel()
	_ = controller.SetReadDeadline(time.Now().Add(19 * time.Minute))
	decoder := json.NewDecoder(io.LimitReader(r.Body, functions.MaxEnvelopeBytes+8192))
	decoder.DisallowUnknownFields()
	var input functionedge.Admission
	if decoder.Decode(&input) != nil || !validEdgeAdmission(input) {
		http.Error(w, "invalid invocation", 422)
		return
	}
	p, err := s.awaitPublicFunction(ctx, input.Branch, input.Slug)
	if err != nil {
		http.Error(w, "function unavailable", 503)
		return
	}
	release, ok := s.functionAdmissionLock(ctx, p.ID, false)
	if !ok {
		http.Error(w, "function admission busy", 503)
		return
	}
	defer release()
	// Re-resolve every request after obtaining admission. A deployment/rollback
	// never leaves a cached route pointing at a retired boot.
	fresh, err := s.functionHTTPIdentity(ctx, p.Branch, p.Slug)
	if err != nil || fresh.ID != p.ID || fresh.State != "active" {
		http.Error(w, "function changed", 503)
		return
	}
	runtime, err := s.loadPublishedFunction(ctx, p.Project, p.Branch, p.Slug)
	if err != nil || runtime.target.FunctionID != p.ID {
		http.Error(w, "function unavailable", 503)
		return
	}
	client, status, err := s.observePublishedFunction(ctx, runtime)
	if err != nil {
		http.Error(w, "runtime unavailable", 503)
		return
	}
	defer client.Close()
	if !status.NodeReady || status.Draining {
		http.Error(w, "runtime unavailable", 503)
		return
	}
	if input.WebSocket {
		capabilities, e := client.Capabilities(ctx)
		if e != nil || capabilities.BootID != runtime.bootID || !capabilities.WebSockets {
			http.Error(w, "websocket unavailable", 503)
			return
		}
	}
	invocation := functions.Invocation{BootID: runtime.bootID, Generation: runtime.target.Generation, Method: input.Method,
		URL: functionPublicOrigin() + input.URI, Headers: functionedge.IngressHeaders(input.Headers), Body: input.Body}
	if !functions.ValidateInvocation(runtime.instance.Scope, invocation) {
		http.Error(w, "invalid invocation", 422)
		return
	}
	deadline := time.Now().Add(functionedge.MaxStreamDuration)
	request := r.WithContext(context.WithValue(ctx, requestIDKey, newID("fer_")))
	event, err := s.beginFunctionEdgeEvent(ctx, request, runtime, len(input.Body), input.Method, deadline)
	if err != nil {
		http.Error(w, "admission journal unavailable", 503)
		return
	}
	finished := false
	defer func() {
		if !finished {
			s.finishFunctionEvent(event, 0, 0, "outcome_unknown")
		}
	}()
	// Observation failures are represented by missing samples, never zero CPU.
	s.sampleFunctionResources(ctx, runtime, client)
	delegated, err := client.DelegateInvocation(invocation, input.WebSocket)
	if err != nil {
		http.Error(w, "invocation rejected", 503)
		return
	}
	grant := functionedge.Grant{EventID: event.ID, RequestID: requestID(request), DefinitionVersion: runtime.version, Deadline: deadline, Request: delegated}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	if json.NewEncoder(w).Encode(grant) != nil || controller.Flush() != nil {
		return
	}
	var completion functionedge.Completion
	if decoder.Decode(&completion) != nil || !validEdgeCompletion(completion, event, int64(len(input.Body))) {
		return
	}
	// Completion is accepted only from this live authenticated edge stream and
	// belongs to this admission. There is no customer-controlled report API.
	finished = s.completeFunctionEdgeEvent(event, completion)
	s.sampleFunctionResources(ctx, runtime, client)
	_ = json.NewEncoder(w).Encode(record{"completed": finished})
	_ = controller.Flush()
}

func validEdgeCompletion(v functionedge.Completion, event functionEvent, requestBytes int64) bool {
	if v.EventID != event.ID || v.RequestBytes != requestBytes || v.ResponseBytes < 0 || v.StreamRequestBytes < 0 || v.DurationMS < 0 || v.DurationMS > 960000 || v.DurationMS > time.Since(event.Started).Milliseconds()+2000 {
		return false
	}
	if !v.Dispatched {
		return v.Outcome == "not_dispatched" && v.Status == 0 && v.ResponseBytes == 0 && v.StreamRequestBytes == 0
	}
	if v.Status != 0 && (v.Status < 100 || v.Status > 599) {
		return false
	}
	switch v.Outcome {
	case "completed", "response_incomplete", "client_disconnected":
		return v.Status >= 100
	case "outcome_unknown":
		return true
	}
	return false
}
