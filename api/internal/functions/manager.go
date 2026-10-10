package functions

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const MaxInvokeBytes = 1 << 20
const MaxEnvelopeBytes = 2 << 20
const MaxInvocations = 16

var instancePattern = regexp.MustCompile(`^fni_[a-f0-9]{16}$`)
var deploymentPattern = regexp.MustCompile(`^fdp_[a-f0-9]{16}$`)
var projectPattern = regexp.MustCompile(`^prj_[a-f0-9]{16}$`)
var branchPattern = regexp.MustCompile(`^br_[a-f0-9]{16}$`)

type Scope struct {
	InstanceID   string `json:"instance_id"`
	DeploymentID string `json:"deployment_id"`
	ProjectID    string `json:"project_id"`
	BranchID     string `json:"branch_id"`
	Slug         string `json:"slug"`
	Generation   int64  `json:"generation"`
}

func (s Scope) Validate() error {
	if !instancePattern.MatchString(s.InstanceID) || !deploymentPattern.MatchString(s.DeploymentID) || !projectPattern.MatchString(s.ProjectID) || !branchPattern.MatchString(s.BranchID) || !ValidSlug(s.Slug) || s.Generation < 1 {
		return errors.New("invalid immutable Functions instance scope")
	}
	return nil
}

type Invocation struct {
	BootID     string              `json:"boot_id"`
	Generation int64               `json:"generation"`
	Method     string              `json:"method"`
	URL        string              `json:"url"`
	Headers    map[string][]string `json:"headers"`
	Body       []byte              `json:"body"`
}

type Manager struct {
	scope        Scope
	auth         *ManagerAuthenticator
	bootID       string
	origin       string
	client       *http.Client
	stop         func(context.Context) error
	mu           sync.Mutex
	active       int
	draining     bool
	stopping     bool
	stopped      bool
	lastActivity time.Time
}

// NewManager has a fixed loopback runtime destination. The destination is never
// supplied by an invocation, preventing a privileged supervisor from becoming
// an arbitrary network proxy. Callers must serve this handler over trusted TLS.
func NewManager(scope Scope, key []byte, origin string, stop func(context.Context) error) (*Manager, error) {
	if stop == nil {
		return nil, errors.New("actual child shutdown callback required")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("fixed loopback Node destination required")
	}
	auth, err := NewManagerAuthenticator(key)
	if err != nil {
		return nil, err
	}
	var boot [16]byte
	if _, err = rand.Read(boot[:]); err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 16, MaxIdleConnsPerHost: 16, MaxConnsPerHost: 16, ResponseHeaderTimeout: 15 * time.Minute, IdleConnTimeout: 30 * time.Second, DisableCompression: true}
	return &Manager{scope: scope, auth: auth, bootID: hex.EncodeToString(boot[:]), origin: origin, stop: stop, lastActivity: time.Now(), client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func strictJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("one JSON object required")
	}
	return nil
}

func managerError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxEnvelopeBytes))
	if err != nil {
		managerError(w, 413, "manager_body_limit")
		return
	}
	if m.auth.Verify(r, body) != nil {
		managerError(w, 401, "manager_authentication_rejected")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/status" && r.URL.RawQuery == "" && len(body) == 0:
		m.status(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/invoke" && r.URL.RawQuery == "":
		m.invoke(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/shutdown" && r.URL.RawQuery == "":
		m.shutdown(w, r, body)
	default:
		managerError(w, 404, "manager_route_not_found")
	}
}

func (m *Manager) status(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	state := map[string]any{"scope": m.scope, "boot_id": m.bootID, "active_invocations": m.active, "draining": m.draining, "last_activity": m.lastActivity.UTC().Format(time.RFC3339Nano), "runtime": Runtime, "background_counters_trusted": false}
	m.mu.Unlock()
	// Node health is observation, not a signed isolation/fencing assertion: user
	// code shares that process and can tamper with its module-level counters.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.origin+"/_neon/status", nil)
	response, err := m.client.Do(request)
	state["node_ready"] = false
	if err == nil {
		defer response.Body.Close()
		content, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		var node map[string]any
		if readErr == nil && len(content) <= 4096 && response.StatusCode == 200 && json.Unmarshal(content, &node) == nil {
			state["node_ready"] = node["ready"] == true
			state["node_observation"] = node
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(state)
}

func safeHeader(name string, values []string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	for _, value := range values {
		for _, c := range value {
			if c < 32 && c != '\t' || c == 127 {
				return false
			}
		}
	}
	return true
}

func hopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "x-neon-")
}

func (m *Manager) validInvocation(v Invocation) bool {
	if v.BootID != m.bootID || v.Generation != m.scope.Generation || len(v.Body) > MaxInvokeBytes || v.Headers == nil || len(v.Headers) > 64 {
		return false
	}
	switch v.Method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return false
	}
	if (v.Method == "GET" || v.Method == "HEAD") && len(v.Body) > 0 {
		return false
	}
	u, err := url.Parse(v.URL)
	prefix := "/functions/v1/" + m.scope.BranchID + "/" + m.scope.Slug
	if err != nil || !(u.Scheme == "https" || u.Scheme == "http") || u.Host == "" || u.User != nil || u.Fragment != "" || !(u.Path == prefix || strings.HasPrefix(u.Path, prefix+"/")) {
		return false
	}
	total := 0
	for name, values := range v.Headers {
		if !safeHeader(name, values) || len(values) > 16 {
			return false
		}
		total += len(name)
		for _, value := range values {
			total += len(value)
		}
	}
	return total <= 16<<10
}

func (m *Manager) invoke(w http.ResponseWriter, r *http.Request, body []byte) {
	var v Invocation
	if strictJSON(body, &v) != nil || !m.validInvocation(v) {
		managerError(w, 422, "invalid_invocation_scope_or_input")
		return
	}
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		managerError(w, 409, "instance_draining")
		return
	}
	if m.active >= MaxInvocations {
		m.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		managerError(w, 429, "instance_capacity_exhausted")
		return
	}
	m.active++
	m.lastActivity = time.Now()
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.active--; m.lastActivity = time.Now(); m.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, v.Method, m.origin+"/", bytes.NewReader(v.Body))
	for name, values := range v.Headers {
		if !hopHeader(name) {
			request.Header[name] = append([]string(nil), values...)
		}
	}
	request.Header.Set("X-Neon-Request-URL", v.URL)
	response, err := m.client.Do(request)
	if err != nil {
		managerError(w, 502, "runtime_unavailable")
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		if !hopHeader(name) {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(flushingWriter{w}, response.Body)
}

type flushingWriter struct{ http.ResponseWriter }

func (w flushingWriter) Write(content []byte) (int, error) {
	n, err := w.ResponseWriter.Write(content)
	if err == nil {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}

func (m *Manager) shutdown(w http.ResponseWriter, r *http.Request, body []byte) {
	var v struct {
		BootID     string `json:"boot_id"`
		Generation int64  `json:"generation"`
	}
	if strictJSON(body, &v) != nil || v.BootID != m.bootID || v.Generation != m.scope.Generation {
		managerError(w, 422, "invalid_shutdown_scope")
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	if m.stopping {
		m.mu.Unlock()
		managerError(w, 409, "shutdown_in_progress")
		return
	}
	m.draining = true
	m.stopping = true
	m.mu.Unlock()
	success := false
	defer func() { m.mu.Lock(); m.stopping = false; m.stopped = success; m.mu.Unlock() }()
	if m.stop != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if m.stop(ctx) != nil {
			managerError(w, 503, "shutdown_incomplete")
			return
		}
	}
	success = true
	w.WriteHeader(204)
}
