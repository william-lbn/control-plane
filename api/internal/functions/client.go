package functions

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"time"
)

var bootPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// InstanceStatus contains privileged supervisor observations. NodeObservation
// belongs to customer code and must never authorize retirement or resource use.
type InstanceStatus struct {
	Scope                     Scope           `json:"scope"`
	BootID                    string          `json:"boot_id"`
	ActiveInvocations         int             `json:"active_invocations"`
	Draining                  bool            `json:"draining"`
	LastActivity              time.Time       `json:"last_activity"`
	Runtime                   string          `json:"runtime"`
	BackgroundCountersTrusted bool            `json:"background_counters_trusted"`
	NodeReady                 bool            `json:"node_ready"`
	NodeObservation           json.RawMessage `json:"node_observation,omitempty"`
}

// InstanceClient is fixed to one admitted instance and private Service IP. The
// control Driver must establish Kubernetes ownership before constructing it;
// TLS/HMAC authenticate this peer but do not replace admission or epoch fencing.
// There is no URL, redirect, environment proxy, system CA, or TLS bypass option.
type InstanceClient struct {
	scope     Scope
	key       []byte
	origin    string
	client    *http.Client
	transport *http.Transport
}

func NewInstanceClient(scope Scope, target netip.AddrPort, key []byte, caPEM string) (*InstanceClient, error) {
	if scope.Validate() != nil || !target.IsValid() || !target.Addr().Is4() || target.Port() != 9090 || !(target.Addr().IsPrivate() || target.Addr().IsLoopback()) || len(key) != 32 {
		return nil, errors.New("invalid admitted Functions manager connection")
	}
	roots, err := trustPool(caPEM)
	if err != nil {
		return nil, errors.New("explicit Functions manager CA required")
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: scope.InstanceID + ".functions.neon.internal"},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Minute,
		MaxResponseHeaderBytes: 32 << 10, MaxIdleConns: 16, MaxIdleConnsPerHost: 16, MaxConnsPerHost: 16,
		IdleConnTimeout: 30 * time.Second, DisableCompression: true,
	}
	return &InstanceClient{scope: scope, key: append([]byte(nil), key...), origin: "https://" + target.String(), transport: transport,
		client: &http.Client{Transport: transport, Timeout: 15 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Close releases idle connections. Callers own and close each Invoke body and
// retain its context until the stream completes; no automatic POST retry occurs.
func (c *InstanceClient) Close() { c.transport.CloseIdleConnections() }

func (c *InstanceClient) request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.origin+path, bytes.NewReader(body))
	if err != nil || SignManagerRequest(request, body, c.key, time.Now()) != nil {
		return nil, errors.New("Functions manager request unavailable")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		// net/http errors can contain the internal address. Never propagate the
		// transport error or signed request headers to public API/audit output.
		return nil, errors.New("Functions manager transport unavailable")
	}
	return response, nil
}

func (c *InstanceClient) Status(ctx context.Context) (InstanceStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := c.request(ctx, http.MethodGet, "/internal/v1/status", nil)
	if err != nil {
		return InstanceStatus{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	var status InstanceStatus
	if err != nil || response.StatusCode != 200 || len(body) > 16<<10 || strictJSON(body, &status) != nil || status.Scope != c.scope || !bootPattern.MatchString(status.BootID) || status.Runtime != Runtime || status.LastActivity.IsZero() || status.ActiveInvocations < 0 || status.ActiveInvocations > MaxInvocations || status.BackgroundCountersTrusted {
		return InstanceStatus{}, errors.New("Functions manager identity or status rejected")
	}
	return status, nil
}

// Invoke returns the original status/headers/body, including application errors
// and redirects. The fixed manager never follows the application's Location.
func (c *InstanceClient) Invoke(ctx context.Context, invocation Invocation) (*http.Response, error) {
	validator := &Manager{scope: c.scope, bootID: invocation.BootID}
	if !bootPattern.MatchString(invocation.BootID) || !validator.validInvocation(invocation) {
		return nil, errors.New("invalid Functions invocation scope or input")
	}
	body, err := json.Marshal(invocation)
	if err != nil || len(body) > MaxEnvelopeBytes {
		return nil, errors.New("Functions invocation envelope exceeds bounds")
	}
	return c.request(ctx, http.MethodPost, "/internal/v1/invoke", body)
}

// Shutdown addresses an explicitly observed boot. A stale boot cannot stop a
// replacement; an interrupted request is retried against the same boot by the
// leased Driver, never by silently refreshing the status to a new process.
func (c *InstanceClient) Shutdown(ctx context.Context, bootID string) error {
	if !bootPattern.MatchString(bootID) {
		return errors.New("explicit Functions boot required for shutdown")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, _ := json.Marshal(struct {
		BootID     string `json:"boot_id"`
		Generation int64  `json:"generation"`
	}{bootID, c.scope.Generation})
	response, err := c.request(ctx, http.MethodPost, "/internal/v1/shutdown", body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("Functions shutdown incomplete; retain the same boot identity")
	}
	return nil
}
