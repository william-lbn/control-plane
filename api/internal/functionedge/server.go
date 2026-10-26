package functionedge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

type Config struct {
	PublicOrigin string
	BrokerOrigin string
	BrokerTLS    *tls.Config
	GuestCA      []byte
	LabHTTP      bool
}

type edge struct {
	origin       *url.URL
	broker       string
	brokerClient *http.Client
	guestRoots   *x509.CertPool
	dialGuest    func(context.Context, string, string) (net.Conn, error)
}

func NewHandler(config Config) (http.Handler, error) {
	public, e := url.Parse(config.PublicOrigin)
	broker, be := url.Parse(config.BrokerOrigin)
	if e != nil || public.Host == "" || public.User != nil || public.Path != "" || public.RawQuery != "" || public.Fragment != "" ||
		(public.Scheme != "https" && !(config.LabHTTP && public.Scheme == "http")) || be != nil || broker.Scheme != "https" || broker.Host == "" ||
		broker.User != nil || broker.Path != "" || broker.RawQuery != "" || broker.Fragment != "" || config.BrokerTLS == nil ||
		config.BrokerTLS.InsecureSkipVerify || config.BrokerTLS.RootCAs == nil || len(config.BrokerTLS.Certificates) != 1 {
		return nil, errors.New("explicit Functions edge origins and mutual TLS required")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(config.GuestCA) {
		return nil, errors.New("explicit Functions guest CA required")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: config.BrokerTLS.Clone(), ForceAttemptHTTP2: false,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 190 * time.Second, MaxConnsPerHost: 16, MaxIdleConnsPerHost: 16,
		IdleConnTimeout: 30 * time.Second, DisableCompression: true}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	transport.TLSClientConfig.MinVersion = tls.VersionTLS13
	s := &edge{origin: public, broker: config.BrokerOrigin, guestRoots: roots,
		brokerClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	mux := http.NewServeMux()
	mux.HandleFunc("/functions/v1/{branch}/{slug}", s.invoke)
	mux.HandleFunc("/functions/v1/{branch}/{slug}/{rest...}", s.invoke)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Host, s.origin.Host) {
			http.Error(w, "Functions authority rejected", 421)
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func publicError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}

func (s *edge) invoke(w http.ResponseWriter, r *http.Request) {
	if !functions.ValidSlug(r.PathValue("slug")) {
		publicError(w, 404, "not_found")
		return
	}
	websocket := r.Header.Get("Upgrade") != ""
	if websocket && (!functions.ValidateWebSocketHandshake(r.Method, r.Header) || r.ContentLength > 0 || len(r.TransferEncoding) > 0) {
		publicError(w, 400, "invalid_websocket_handshake")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, functions.MaxInvokeBytes))
	if err != nil {
		publicError(w, 413, "invocation_too_large")
		return
	}
	input := Admission{Branch: r.PathValue("branch"), Slug: r.PathValue("slug"), Method: r.Method, URI: r.URL.RequestURI(),
		Headers: IngressHeaders(r.Header), Body: body, WebSocket: websocket}
	probe := functions.Invocation{BootID: strings.Repeat("0", 32), Generation: 1, Method: input.Method, URL: s.origin.String() + input.URI, Headers: input.Headers, Body: body}
	if !functions.ValidateInvocation(functions.Scope{BranchID: input.Branch, Slug: input.Slug, Generation: 1}, probe) {
		publicError(w, 422, "invalid_invocation")
		return
	}
	// Keep the control connection alive for the completion receipt even when
	// the public client disconnects. Guest execution still uses r.Context().
	brokerCtx, brokerCancel := context.WithTimeout(context.WithoutCancel(r.Context()), MaxStreamDuration+4*time.Minute)
	defer brokerCancel()
	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close()
	defer pipeWriter.Close()
	req, _ := http.NewRequestWithContext(brokerCtx, http.MethodPost, s.broker+BrokerPath, pipeReader)
	req.Header.Set("Content-Type", "application/x-ndjson")
	go func() {
		if json.NewEncoder(pipeWriter).Encode(input) != nil {
			pipeWriter.CloseWithError(errors.New("admission write failed"))
		}
	}()
	response, err := s.brokerClient.Do(req)
	if err != nil {
		publicError(w, 503, "function_admission_unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		publicError(w, 503, "function_unavailable")
		return
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, functions.MaxEnvelopeBytes+8192))
	decoder.DisallowUnknownFields()
	var grant Grant
	if decoder.Decode(&grant) != nil || !validGrant(grant, input, time.Now()) {
		publicError(w, 503, "function_admission_rejected")
		return
	}
	w.Header().Set("X-Request-ID", grant.RequestID)
	w.Header().Set("X-Function-Deployment-ID", grant.Request.Scope.DeploymentID)
	w.Header().Set("X-Function-Generation", strconv.FormatInt(grant.Request.Scope.Generation, 10))
	invokeCtx, invokeCancel := context.WithDeadline(r.Context(), grant.Deadline)
	defer invokeCancel()
	ack := make(chan error, 1)
	go func() {
		var value struct {
			Completed bool `json:"completed"`
		}
		err := decoder.Decode(&value)
		if err != nil || !value.Completed {
			invokeCancel()
		}
		ack <- err
	}()
	completion := Completion{EventID: grant.EventID, RequestBytes: int64(len(body)), Outcome: "not_dispatched"}
	started := time.Now()
	defer func() {
		completion.DurationMS = max(0, time.Since(started).Milliseconds())
		reported := make(chan error, 1)
		go func() { reported <- json.NewEncoder(pipeWriter).Encode(completion); _ = pipeWriter.Close() }()
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		select {
		case err := <-reported:
			if err != nil {
				return
			}
		case <-timer.C:
			brokerCancel()
			return
		}
		select {
		case <-ack:
		case <-timer.C:
			brokerCancel()
		}
	}()
	upstream, closeTransport, err := s.dispatch(invokeCtx, grant.Request)
	defer closeTransport()
	completion.Dispatched = true // Sending may have executed even if the response is lost.
	completion.Outcome = "outcome_unknown"
	if err != nil {
		publicError(w, 502, "function_invocation_outcome_unknown")
		return
	}
	defer upstream.Body.Close()
	completion.Status = upstream.StatusCode
	if websocket && upstream.StatusCode == 101 {
		tunnel := &meteredTunnel{ReadWriteCloser: upstream.Body.(io.ReadWriteCloser)}
		upstream.Body = tunnel
		_, err := functions.RelayWebSocket(invokeCtx, w, upstream)
		completion.ResponseBytes = tunnel.received
		completion.StreamRequestBytes = tunnel.sent
		completion.Outcome = "completed"
		if err != nil {
			completion.Outcome = "client_disconnected"
		}
		return
	}
	CopyPublicHeaders(w.Header(), upstream.Header)
	w.WriteHeader(upstream.StatusCode)
	completion.Outcome = "completed"
	buffer := make([]byte, 16<<10)
	controller := http.NewResponseController(w)
	for {
		n, readErr := upstream.Body.Read(buffer)
		if n > 0 {
			written, writeErr := w.Write(buffer[:n])
			completion.ResponseBytes += int64(written)
			if writeErr != nil || controller.Flush() != nil {
				completion.Outcome = "client_disconnected"
				break
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				completion.Outcome = "response_incomplete"
			}
			break
		}
	}
}

// RelayWebSocket joins both copy goroutines before returning, so directional
// counters are read only after their writers have exited.
type meteredTunnel struct {
	io.ReadWriteCloser
	received, sent int64
}

func (m *meteredTunnel) Read(p []byte) (int, error) {
	n, err := m.ReadWriteCloser.Read(p)
	m.received += int64(n)
	return n, err
}
func (m *meteredTunnel) Write(p []byte) (int, error) {
	n, err := m.ReadWriteCloser.Write(p)
	m.sent += int64(n)
	return n, err
}

func validGrant(g Grant, input Admission, now time.Time) bool {
	v := g.Request
	target, targetErr := netip.ParseAddrPort(v.Target)
	if targetErr != nil || !target.Addr().Is4() || !(target.Addr().IsPrivate() || target.Addr().IsLoopback()) || target.Port() != 9090 {
		return false
	}
	if g.EventID == "" || g.RequestID == "" || g.DefinitionVersion < 1 || !g.Deadline.After(now) || g.Deadline.After(now.Add(MaxStreamDuration+time.Minute)) ||
		v.Scope.Validate() != nil || v.Scope.BranchID != input.Branch || v.Scope.Slug != input.Slug || !v.ExpiresAt.After(now) || v.ExpiresAt.After(now.Add(31*time.Second)) {
		return false
	}
	path := "/internal/v1/invoke"
	if input.WebSocket {
		path = "/internal/v1/websocket"
	}
	if v.Path != path || len(v.Body) > functions.MaxEnvelopeBytes {
		return false
	}
	var invocation functions.Invocation
	return json.Unmarshal(v.Body, &invocation) == nil && invocation.BootID == v.BootID && invocation.Generation == v.Scope.Generation &&
		invocation.TriggerInvocationID == "" && invocation.Method == input.Method && bytes.Equal(invocation.Body, input.Body) && functions.ValidateInvocation(v.Scope, invocation)
}

func (s *edge) dispatch(ctx context.Context, v functions.DelegatedRequest) (*http.Response, func(), error) {
	noop := func() {}
	target, err := netip.ParseAddrPort(v.Target)
	if err != nil || !target.Addr().Is4() || !(target.Addr().IsPrivate() || target.Addr().IsLoopback()) || target.Port() != 9090 {
		return nil, noop, errors.New("private original manager address required")
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, ForceAttemptHTTP2: false,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: s.guestRoots, ServerName: v.Scope.InstanceID + ".functions.neon.internal"},
		DialContext:     (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: MaxStreamDuration, MaxResponseHeaderBytes: 32 << 10}
	if s.dialGuest != nil {
		transport.DialContext = s.dialGuest
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+v.Target+v.Path, bytes.NewReader(v.Body))
	r.Header = v.Headers.Clone()
	response, err := transport.RoundTrip(r) // RoundTrip never follows redirects or retries a POST.
	if err != nil {
		return nil, transport.CloseIdleConnections, errors.New("manager transport unavailable")
	}
	if response.StatusCode == 101 {
		if v.Path != "/internal/v1/websocket" {
			response.Body.Close()
			return nil, transport.CloseIdleConnections, errors.New("unexpected upgrade")
		}
		if _, ok := response.Body.(io.ReadWriteCloser); !ok {
			response.Body.Close()
			return nil, transport.CloseIdleConnections, errors.New("upgrade tunnel unavailable")
		}
	}
	return response, transport.CloseIdleConnections, nil
}
