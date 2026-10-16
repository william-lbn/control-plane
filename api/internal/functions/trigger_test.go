package functions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTriggerCapabilitiesAndPrivilegedHeaderAttestation(t *testing.T) {
	var called atomic.Int32
	id := "fti_" + strings.Repeat("a", 64)
	client, manager, _ := trustedClientFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_neon/status" {
			readyNode(w, r)
			return
		}
		count := called.Add(1)
		wanted := ""
		if count == 2 {
			wanted = id
		}
		if r.Header.Get("X-Neon-Trigger-Invocation-Id") != wanted || r.Header.Get("X-Neon-Private") != "" {
			t.Error("caller header bypassed trusted trigger field")
		}
		_, _ = io.Copy(w, r.Body)
	}, nil)
	caps, err := client.Capabilities(context.Background())
	if err != nil || !caps.TriggerInvocations || caps.Scope != manager.scope || caps.BootID != manager.bootID {
		t.Fatal("signed manager capabilities rejected", err)
	}
	status, err := client.Status(context.Background())
	if err != nil || status.Scope != manager.scope {
		t.Fatal("original status representation changed", err)
	}
	v := invocationFor(manager)
	v.Headers["X-Neon-Trigger-Invocation-Id"] = []string{"forged"}
	v.Headers["X-Neon-Private"] = []string{"discard"}
	response, err := client.Invoke(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("ordinary invocation changed")
	}
	v.TriggerInvocationID = id
	v.Body, _ = json.Marshal(map[string]any{"version": 1, "invocation_id": id, "trigger": map[string]string{"type": "schedule"}})
	response, err = client.Invoke(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("attested trigger invocation rejected")
	}
	for _, change := range []func(*Invocation){func(v *Invocation) { v.TriggerInvocationID = "forged" }, func(v *Invocation) { v.Method = "GET" }, func(v *Invocation) { v.Body = []byte(`{"version":1,"invocation_id":"wrong"}`) }, func(v *Invocation) { v.Body = []byte(`{"version":2}`) }} {
		bad := v
		change(&bad)
		if _, err = client.Invoke(context.Background(), bad); err == nil {
			t.Fatal("invalid privileged field reached manager")
		}
	}
	if called.Load() != 2 {
		t.Fatal("rejected trigger was dispatched")
	}
	w := httptest.NewRecorder()
	manager.ServeHTTP(w, httptest.NewRequest("GET", "/internal/v1/capabilities", nil))
	if w.Code != 401 {
		t.Fatal("capabilities bypassed manager authentication")
	}
}

type triggerCapabilityTransport func(*http.Request) (*http.Response, error)

func (f triggerCapabilityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTriggerCapabilitiesFailClosedForOldOrMisboundGuest(t *testing.T) {
	client, manager, _ := trustedClientFixture(t, readyNode, nil)
	for _, sample := range []struct {
		status int
		body   string
	}{{404, "old guest"}, {200, `{"scope":{},"boot_id":"` + manager.bootID + `","trigger_invocations":true}`}, {200, strings.Repeat("x", 4097)}, {200, `{"unexpected":true}`}} {
		client.client.Transport = triggerCapabilityTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: sample.status, Body: io.NopCloser(strings.NewReader(sample.body)), Header: http.Header{}}, nil
		})
		if _, err := client.Capabilities(context.Background()); err == nil {
			t.Fatal("old or misbound capabilities accepted")
		}
	}
}
