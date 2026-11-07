package functions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// DelegatedRequest is a single exact manager capability, not a manager key.
// Changing its method, path, body or nonce invalidates the manager HMAC. The
// manager consumes its nonce once and rejects it after the 30 second window.
type DelegatedRequest struct {
	Target    string      `json:"target"`
	Scope     Scope       `json:"scope"`
	BootID    string      `json:"boot_id"`
	Path      string      `json:"path"`
	Headers   http.Header `json:"headers"`
	Body      []byte      `json:"body"`
	ExpiresAt time.Time   `json:"expires_at"`
}

func (c *InstanceClient) DelegateInvocation(v Invocation, websocket bool) (DelegatedRequest, error) {
	if !ValidateInvocation(c.scope, v) || v.TriggerInvocationID != "" {
		return DelegatedRequest{}, errors.New("delegated invocation rejected")
	}
	path := "/internal/v1/invoke"
	if websocket {
		h := http.Header(v.Headers).Clone()
		h.Set("Connection", "Upgrade")
		h.Set("Upgrade", "websocket")
		if len(v.Body) != 0 || !ValidateWebSocketHandshake(v.Method, h) {
			return DelegatedRequest{}, errors.New("delegated websocket rejected")
		}
		path = "/internal/v1/websocket"
	}
	body, err := json.Marshal(v)
	if err != nil || len(body) > MaxEnvelopeBytes {
		return DelegatedRequest{}, errors.New("delegated envelope exceeds limit")
	}
	now := time.Now()
	r, _ := http.NewRequest(http.MethodPost, c.origin+path, nil)
	if SignManagerRequest(r, body, c.key, now) != nil {
		return DelegatedRequest{}, errors.New("delegation unavailable")
	}
	r.Header.Set("Content-Type", "application/json")
	if websocket {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	}
	return DelegatedRequest{Target: strings.TrimPrefix(c.origin, "https://"), Scope: c.scope, BootID: v.BootID,
		Path: path, Headers: r.Header, Body: body, ExpiresAt: now.Add(managerWindow)}, nil
}

// Resources comes from the privileged supervisor's cgroup, never customer
// NodeObservation. A guest without this endpoint fails closed as unavailable.
func (c *InstanceClient) Resources(ctx context.Context) (ResourceMeter, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := c.request(ctx, http.MethodGet, "/internal/v1/resources", nil)
	if err != nil {
		return ResourceMeter{}, err
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	var out ResourceMeter
	if err != nil || r.StatusCode != 200 || len(body) > 4096 || strictJSON(body, &out) != nil ||
		out.Scope != c.scope || !bootPattern.MatchString(out.BootID) || out.ObservedAt.IsZero() ||
		out.CPUUsec < 0 || out.MemoryBytes < 0 || out.MemoryPeakBytes < out.MemoryBytes || out.Source != "supervisor_cgroup_v2" {
		return ResourceMeter{}, errors.New("supervisor resource observation unavailable")
	}
	return out, nil
}
