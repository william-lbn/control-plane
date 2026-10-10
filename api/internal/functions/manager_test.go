package functions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func managerFixture(t *testing.T, handler http.HandlerFunc, stop func(context.Context) error) (*Manager, *httptest.Server, []byte) {
	t.Helper()
	if stop == nil {
		stop = func(context.Context) error { return nil }
	}
	node := httptest.NewServer(handler)
	t.Cleanup(node.Close)
	key := bytes.Repeat([]byte{31}, 32)
	scope := Scope{InstanceID: "fni_0000000000000001", DeploymentID: "fdp_0000000000000002", ProjectID: "prj_0000000000000003", BranchID: "br_0000000000000004", Slug: "hello", Generation: 2}
	manager, err := NewManager(scope, key, node.URL, stop)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(manager)
	t.Cleanup(server.Close)
	return manager, server, key
}

func managerCall(t *testing.T, origin, path string, key []byte, body []byte) *http.Response {
	t.Helper()
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	request, err := http.NewRequest(method, origin+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err = SignManagerRequest(request, body, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func invocationFor(m *Manager) Invocation {
	return Invocation{BootID: m.bootID, Generation: m.scope.Generation, Method: "POST", URL: "https://functions.example.test/functions/v1/" + m.scope.BranchID + "/" + m.scope.Slug + "/child", Headers: map[string][]string{"X-App": {"value"}}, Body: []byte("input")}
}
func encode(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestManagerAuthenticatesBeforeExposingRuntime(t *testing.T) {
	var calls atomic.Int32
	_, server, _ := managerFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }, nil)
	response, err := http.Get(server.URL + "/internal/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 401 || calls.Load() != 0 {
		t.Fatal("unauthenticated manager call reached runtime")
	}
}

func TestManagerInvokePreservesRequestAndStreamsResponse(t *testing.T) {
	m, server, key := managerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/" || r.Method != "POST" || string(body) != "input" || r.Header.Get("X-App") != "value" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Neon-Request-URL") == "" {
			t.Error("request proxy boundary changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Neon-Secret", "must-not-forward")
		w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond)
		w.Write([]byte("data: second\n\n"))
	}, nil)
	v := invocationFor(m)
	v.Headers["X-Neon-Manager-Digest"] = []string{"must-not-forward"}
	response := managerCall(t, server.URL, "/internal/v1/invoke", key, encode(t, v))
	if response.StatusCode != 200 || response.Header.Get("X-Neon-Secret") != "" {
		t.Fatal("response boundary failed")
	}
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "data: first\n\n" {
		t.Fatal("SSE did not preserve first chunk")
	}
	rest, err := io.ReadAll(response.Body)
	if err != nil || string(rest) != "data: second\n\n" {
		t.Fatal("SSE did not preserve second chunk")
	}
}

func TestManagerRejectsCrossBranchAndOldBootInvocation(t *testing.T) {
	var calls atomic.Int32
	m, server, key := managerFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, nil)
	for _, kind := range []string{"branch", "slug", "generation", "boot", "method", "header", "body"} {
		t.Run(kind, func(t *testing.T) {
			v := invocationFor(m)
			switch kind {
			case "branch":
				v.URL = "https://functions.example.test/functions/v1/br_0000000000000099/hello"
			case "slug":
				v.URL = "https://functions.example.test/functions/v1/" + m.scope.BranchID + "/other"
			case "generation":
				v.Generation++
			case "boot":
				v.BootID = "previous-boot"
			case "method":
				v.Method = "CONNECT"
			case "header":
				v.Headers["X-Test"] = []string{"bad\r\nInjected: header"}
			case "body":
				v.Body = bytes.Repeat([]byte{1}, MaxInvokeBytes+1)
			}
			response := managerCall(t, server.URL, "/internal/v1/invoke", key, encode(t, v))
			if response.StatusCode != 422 {
				t.Fatalf("invalid scope accepted: %d", response.StatusCode)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid invocation reached runtime")
	}
}

func TestManagerDoesNotFollowUserRedirects(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls.Add(1) }))
	defer foreign.Close()
	m, server, key := managerFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, 302) }, nil)
	response := managerCall(t, server.URL, "/internal/v1/invoke", key, encode(t, invocationFor(m)))
	if response.StatusCode != 302 || foreignCalls.Load() != 0 {
		t.Fatal("privileged manager followed arbitrary redirect")
	}
}

func TestFailedShutdownKeepsAdmissionClosedAndRetriesStop(t *testing.T) {
	var stops atomic.Int32
	m, server, key := managerFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }, func(context.Context) error {
		if stops.Add(1) == 1 {
			return errors.New("private failure")
		}
		return nil
	})
	body := encode(t, map[string]any{"boot_id": m.bootID, "generation": m.scope.Generation})
	if response := managerCall(t, server.URL, "/internal/v1/shutdown", key, body); response.StatusCode != 503 {
		t.Fatal("failed stop marked successful")
	}
	if response := managerCall(t, server.URL, "/internal/v1/invoke", key, encode(t, invocationFor(m))); response.StatusCode != 409 {
		t.Fatal("draining instance admitted invocation")
	}
	if response := managerCall(t, server.URL, "/internal/v1/shutdown", key, body); response.StatusCode != 204 {
		t.Fatal("original stop cannot resume")
	}
	if response := managerCall(t, server.URL, "/internal/v1/shutdown", key, body); response.StatusCode != 204 || stops.Load() != 2 {
		t.Fatal("completed stop is not idempotent")
	}
}

func TestManagerStrictEnvelopeAndAuthenticatedStatus(t *testing.T) {
	m, server, key := managerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ready":true,"active":0,"pending":0}`))
	}, nil)
	response := managerCall(t, server.URL, "/internal/v1/status", key, nil)
	var status map[string]any
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || status["node_ready"] != true || status["background_counters_trusted"] != false || status["boot_id"] != m.bootID {
		t.Fatal("manager observation misrepresented its boundary")
	}
	v := invocationFor(m)
	body := bytes.TrimSuffix(encode(t, v), []byte("}"))
	body = append(body, []byte(`,"unexpected":true}`)...)
	if response := managerCall(t, server.URL, "/internal/v1/invoke", key, body); response.StatusCode != 422 {
		t.Fatal("unknown invocation field accepted")
	}
}

func TestManagerNeverAcceptsNonLoopbackRuntimeDestination(t *testing.T) {
	scope := Scope{InstanceID: "fni_0000000000000001", DeploymentID: "fdp_0000000000000002", ProjectID: "prj_0000000000000003", BranchID: "br_0000000000000004", Slug: "hello", Generation: 1}
	for _, origin := range []string{"http://169.254.169.254:8081", "https://127.0.0.1:8081", "http://127.0.0.1:8081/other", "http://user@127.0.0.1:8081", "http://127.0.0.1:8081?target=secret"} {
		if _, err := NewManager(scope, bytes.Repeat([]byte{1}, 32), origin, func(context.Context) error { return nil }); err == nil {
			t.Fatal("arbitrary privileged destination accepted")
		}
	}
}

func TestManagerRequiresPhysicalChildShutdownImplementation(t *testing.T) {
	scope := Scope{InstanceID: "fni_0000000000000001", DeploymentID: "fdp_0000000000000002", ProjectID: "prj_0000000000000003", BranchID: "br_0000000000000004", Slug: "hello", Generation: 1}
	if _, err := NewManager(scope, bytes.Repeat([]byte{1}, 32), "http://127.0.0.1:8081", nil); err == nil {
		t.Fatal("manager can acknowledge shutdown without closing its child")
	}
}
