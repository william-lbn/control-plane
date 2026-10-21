package functions

import (
	"context"
	"golang.org/x/net/websocket"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestWebSocketAuthenticatedTunnelAndActivity(t *testing.T) {
	node := websocket.Server{Handshake: func(config *websocket.Config, _ *http.Request) error {
		config.Protocol = []string{"echo.v1"}
		return nil
	}, Handler: func(ws *websocket.Conn) {
		defer ws.Close()
		b := make([]byte, 32)
		n, e := ws.Read(b)
		if e == nil {
			_, _ = ws.Write(b[:n])
		}
	}}
	c, m, _ := trustedClientFixture(t, func(w http.ResponseWriter, r *http.Request) { node.ServeHTTP(w, r) }, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v := invocationFor(m)
	v.Method = "GET"
	v.Body = nil
	v.Headers = map[string][]string{"Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Protocol": {"echo.v1"}}
	response, err := c.WebSocket(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 101 || response.Header.Get("Sec-WebSocket-Protocol") != "echo.v1" {
		t.Fatal("handshake changed")
	}
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active != 1 {
		t.Fatal("open tunnel not counted")
	}
	tunnel := response.Body.(io.ReadWriteCloser)
	// Real masked client text frame, followed by an actual server echo frame.
	frame := []byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2}
	if _, err = tunnel.Write(frame); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err = io.ReadFull(tunnel, buf); err != nil || string(buf[2:]) != "hi" {
		t.Fatalf("frame relay: %x %v", buf, err)
	}
	tunnel.Close()
	for i := 0; i < 50; i++ {
		m.mu.Lock()
		n := m.active
		m.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("closed tunnel remained active")
}

func TestWebSocketValidationAndApplicationRejection(t *testing.T) {
	h := http.Header{"Upgrade": {"websocket"}, "Connection": {"keep-alive, Upgrade"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Protocol": {"echo.v1, echo.v2"}}
	if !ValidateWebSocketHandshake("GET", h) {
		t.Fatal("standard handshake rejected")
	}
	for _, modify := range []func(http.Header){func(h http.Header) { h.Set("Sec-WebSocket-Key", "bad") }, func(h http.Header) { h.Set("Sec-WebSocket-Version", "12") }, func(h http.Header) { h.Set("Sec-WebSocket-Protocol", "one, one") }, func(h http.Header) { h.Set("Sec-WebSocket-Protocol", "has space") }, func(h http.Header) { h.Del("Connection") }} {
		bad := h.Clone()
		modify(bad)
		if ValidateWebSocketHandshake("GET", bad) {
			t.Fatal("invalid handshake admitted")
		}
	}
	if ValidateWebSocketHandshake("POST", h) {
		t.Fatal("POST admitted")
	}
	c, m, _ := trustedClientFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }, nil)
	v := invocationFor(m)
	v.Method = "GET"
	v.Body = nil
	v.Headers = map[string][]string(h)
	response, err := c.WebSocket(context.Background(), v)
	if err != nil || response.StatusCode != 401 {
		t.Fatal("application denial lost", err)
	}
	response.Body.Close()
	v.BootID = "00000000000000000000000000000000"
	if response, err = c.WebSocket(context.Background(), v); err == nil {
		response.Body.Close()
		t.Fatal("wrong boot admitted")
	}
}
