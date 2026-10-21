package functions

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func headerToken(h http.Header, name, want string) bool {
	for _, value := range h.Values(name) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

// ValidateWebSocketHandshake runs before cold wake. Compression is deliberately
// not negotiated; the guest validates masked frames, UTF-8 and message limits.
func ValidateWebSocketHandshake(method string, h http.Header) bool {
	if method != "GET" || !strings.EqualFold(h.Get("Upgrade"), "websocket") || !headerToken(h, "Connection", "upgrade") || h.Get("Sec-WebSocket-Version") != "13" || len(h.Values("Sec-WebSocket-Key")) != 1 {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(h.Get("Sec-WebSocket-Key"))
	if err != nil || len(key) != 16 {
		return false
	}
	seen := map[string]bool{}
	total := 0
	for _, value := range h.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(value, ",") {
			p = strings.TrimSpace(p)
			total += len(p)
			if !safeHeader(p, nil) || seen[p] || total > 1024 {
				return false
			}
			seen[p] = true
		}
	}
	return len(seen) <= 16
}

func validWebSocketResponse(h http.Header, response *http.Response) bool {
	if response.StatusCode != 101 || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") || !headerToken(response.Header, "Connection", "upgrade") || response.Header.Get("Sec-WebSocket-Extensions") != "" {
		return false
	}
	sum := sha1.Sum([]byte(h.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return false
	}
	selected := response.Header.Get("Sec-WebSocket-Protocol")
	if selected == "" {
		return true
	}
	if len(response.Header.Values("Sec-WebSocket-Protocol")) != 1 {
		return false
	}
	for _, v := range h.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if strings.TrimSpace(p) == selected {
				return true
			}
		}
	}
	return false
}

func (c *InstanceClient) WebSocket(ctx context.Context, v Invocation) (*http.Response, error) {
	h := http.Header(v.Headers).Clone()
	h.Set("Connection", "Upgrade")
	h.Set("Upgrade", "websocket")
	if !ValidateInvocation(c.scope, v) || v.TriggerInvocationID != "" || len(v.Body) != 0 || !ValidateWebSocketHandshake(v.Method, h) {
		return nil, errors.New("invalid Functions WebSocket invocation")
	}
	body, err := json.Marshal(v)
	if err != nil || len(body) > MaxEnvelopeBytes {
		return nil, errors.New("invalid Functions WebSocket envelope")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+"/internal/v1/websocket", strings.NewReader(string(body)))
	if err != nil || SignManagerRequest(request, body, c.key, time.Now()) != nil {
		return nil, errors.New("Functions WebSocket signing unavailable")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response, err := c.transport.RoundTrip(request)
	if err != nil {
		return nil, errors.New("Functions WebSocket manager unavailable")
	}
	if response.StatusCode >= 200 && response.StatusCode <= 599 && response.Header.Get("X-Neon-Function-Handshake-Rejected") == "true" {
		return response, nil
	}
	if !validWebSocketResponse(h, response) {
		response.Body.Close()
		return nil, errors.New("Functions WebSocket handshake rejected")
	}
	if _, ok := response.Body.(io.ReadWriteCloser); !ok {
		response.Body.Close()
		return nil, errors.New("Functions WebSocket tunnel unavailable")
	}
	return response, nil
}

// RelayWebSocket keeps the original buffered reader (including a first frame
// already read with the HTTP headers), and closes both ends on cancellation.
// No credentials, arbitrary upstream headers, or internal addresses are exposed.
func RelayWebSocket(ctx context.Context, w http.ResponseWriter, response *http.Response) (int64, error) {
	upstream, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		return 0, errors.New("bidirectional WebSocket required")
	}
	downstream, buffer, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return 0, err
	}
	defer downstream.Close()
	defer upstream.Close()
	stop := context.AfterFunc(ctx, func() { downstream.Close(); upstream.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = downstream.SetDeadline(deadline)
	}
	fmt.Fprint(buffer, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nCache-Control: no-store\r\n")
	for _, name := range []string{"Sec-WebSocket-Accept", "Sec-WebSocket-Protocol"} {
		if value := response.Header.Get(name); value != "" {
			if !safeHeader(name, []string{value}) {
				return 0, errors.New("invalid WebSocket response")
			}
			fmt.Fprintf(buffer, "%s: %s\r\n", name, value)
		}
	}
	fmt.Fprint(buffer, "\r\n")
	if err = buffer.Flush(); err != nil {
		return 0, err
	}
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 2)
	go func() { n, e := io.Copy(upstream, buffer); done <- result{n, e} }()
	go func() { n, e := io.Copy(downstream, upstream); done <- result{n, e} }()
	first := <-done
	downstream.Close()
	upstream.Close()
	second := <-done
	if first.err != nil {
		return first.n + second.n, first.err
	}
	return first.n + second.n, second.err
}

func (m *Manager) websocket(w http.ResponseWriter, r *http.Request, body []byte) {
	var v Invocation
	if strictJSON(body, &v) != nil || !m.validInvocation(v) || v.TriggerInvocationID != "" || len(v.Body) != 0 || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerToken(r.Header, "Connection", "upgrade") {
		managerError(w, 422, "invalid_websocket_invocation")
		return
	}
	h := http.Header(v.Headers).Clone()
	h.Set("Connection", "Upgrade")
	h.Set("Upgrade", "websocket")
	if !ValidateWebSocketHandshake(v.Method, h) {
		managerError(w, 422, "invalid_websocket_handshake")
		return
	}
	m.mu.Lock()
	if m.draining || m.active >= MaxInvocations {
		m.mu.Unlock()
		managerError(w, 429, "instance_capacity_unavailable")
		return
	}
	m.active++
	m.lastActivity = time.Now()
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.active--; m.lastActivity = time.Now(); m.mu.Unlock() }()
	m.output.Platform("invoke begin")
	defer m.output.Platform("invoke end")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.origin+"/", nil)
	for name, values := range h {
		if !hopHeader(name) && !strings.HasPrefix(strings.ToLower(name), "sec-websocket-extensions") {
			request.Header[name] = append([]string(nil), values...)
		}
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("X-Neon-Request-URL", v.URL)
	response, err := m.client.Do(request)
	if err != nil {
		managerError(w, 502, "websocket_runtime_unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 101 {
		if response.StatusCode < 200 || response.StatusCode > 599 {
			managerError(w, 502, "websocket_runtime_rejected")
			return
		}
		w.Header().Set("X-Neon-Function-Handshake-Rejected", "true")
		w.WriteHeader(response.StatusCode)
		_, _ = io.CopyN(w, response.Body, 4096)
		return
	}
	if !validWebSocketResponse(h, response) {
		managerError(w, 502, "websocket_runtime_handshake_rejected")
		return
	}
	_, _ = RelayWebSocket(ctx, w, response)
}
