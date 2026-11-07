package functions

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDelegatedInvocationExecutesOnceWithoutManagerKey(t *testing.T) {
	c, m, b := trustedClientFixture(t, readyNode, nil)
	v := invocationFor(m)
	v.Body = []byte("delegated exact original body")
	grant, err := c.DelegateInvocation(v, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(grant)
	if bytes.Contains(raw, []byte(b.ManagerKey)) || grant.Path != "/internal/v1/invoke" || grant.Scope != m.scope {
		t.Fatal("delegation leaked management authority or scope")
	}
	call := func(path string, body []byte) int {
		r, _ := http.NewRequest(http.MethodPost, c.origin+path, bytes.NewReader(body))
		r.Header = grant.Headers.Clone()
		response, err := c.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	if call("/internal/v1/shutdown", grant.Body) != 401 || call(grant.Path, append(append([]byte(nil), grant.Body...), byte(' '))) != 401 {
		t.Fatal("invocation grant became arbitrary manager authority")
	}
	if call(grant.Path, grant.Body) != 200 || call(grant.Path, grant.Body) != 401 {
		t.Fatal("exact delegated execution or single consumption failed")
	}
	expired, err := c.DelegateInvocation(v, false)
	if err != nil {
		t.Fatal(err)
	}
	m.auth.clock = func() time.Time { return time.Now().Add(time.Minute) }
	r := httptest.NewRequest(http.MethodPost, "https://original.invalid"+expired.Path, bytes.NewReader(expired.Body))
	r.Header = expired.Headers
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("expired delegation accepted")
	}
}

func TestSupervisorResourcesRequireAuthenticatedOriginalBoot(t *testing.T) {
	c, m, _ := trustedClientFixture(t, readyNode, nil)
	if _, err := c.Resources(context.Background()); err == nil {
		t.Fatal("missing resource reader fabricated zero usage")
	}
	m.resourceMeter = func() (ResourceMeter, error) {
		return ResourceMeter{CPUUsec: 1234, MemoryBytes: 4096, MemoryPeakBytes: 8192}, nil
	}
	v, err := c.Resources(context.Background())
	if err != nil || v.Scope != m.scope || v.BootID != m.bootID || v.CPUUsec != 1234 || v.MemoryBytes != 4096 || v.Source != "supervisor_cgroup_v2" {
		t.Fatal("privileged resources lost scope or counters", err)
	}
	r := httptest.NewRequest(http.MethodGet, "https://manager.invalid/internal/v1/resources", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("public request read privileged counters")
	}
}
