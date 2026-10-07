// Package adapter implements the pinned Neon Proxy/control-hook protocol.
// Kubernetes remains the authoritative route and workload store. This runtime
// replaces the laboratory Python server; it is not a distributed SQL fence.
package adapter

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Kubernetes interface {
	Request(context.Context, string, string, any) (map[string]any, error)
}

// APIError contains only a status, never a dependency response/token/body.
type APIError struct{ Status int }

func (e APIError) Error() string { return fmt.Sprintf("dependency HTTP %d", e.Status) }

type Credential struct{ File, Value string }

func (c Credential) read() (string, error) {
	value := c.Value
	if c.File != "" {
		b, err := os.ReadFile(c.File)
		if err != nil {
			return "", errors.New("credential file unavailable")
		}
		value = string(b)
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("invalid credential")
	}
	return value, nil
}

type Config struct {
	Namespace, RoutesSecret, NotificationsConfigMap string
	ProxyToken, HookToken                           Credential
	WakeTimeout, PollInterval                       time.Duration
	MaxConcurrentWakes                              int
	PageserverNodeID                                uint64
}

type Server struct {
	config                        Config
	kube                          Kubernetes
	logger                        *slog.Logger
	wakeSlots                     chan struct{}
	locks                         endpointLocks
	requests, failures, coldWakes atomic.Uint64
}

var dnsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var selectorName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
var nativeID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func New(config Config, kube Kubernetes, logger *slog.Logger) (*Server, error) {
	if kube == nil || logger == nil {
		return nil, errors.New("adapter dependencies required")
	}
	for _, name := range []string{config.Namespace, config.RoutesSecret, config.NotificationsConfigMap} {
		if !dnsName.MatchString(name) {
			return nil, errors.New("invalid Kubernetes resource name")
		}
	}
	proxy, err := config.ProxyToken.read()
	if err != nil {
		return nil, err
	}
	hook, err := config.HookToken.read()
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(proxy), []byte(hook)) == 1 {
		return nil, errors.New("proxy and hook credentials must differ")
	}
	if config.WakeTimeout <= 0 || config.WakeTimeout > 5*time.Minute || config.PollInterval <= 0 || config.PollInterval > 5*time.Second || config.MaxConcurrentWakes < 1 || config.MaxConcurrentWakes > 64 || config.PageserverNodeID == 0 {
		return nil, errors.New("invalid adapter bounds")
	}
	return &Server{config: config, kube: kube, logger: logger, wakeSlots: make(chan struct{}, config.MaxConcurrentWakes)}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"status": "ok", "runtime": "go", "process_role": "proxy_adapter", "distributed_fencing": false})
	})
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /cplane/get_endpoint_access_control", s.access)
	mux.HandleFunc("GET /cplane/wake_compute", s.wake)
	mux.HandleFunc("GET /cplane/endpoints/{endpoint}/jwks", s.jwks)
	mux.HandleFunc("PUT /notify-attach", s.notify)
	mux.HandleFunc("PUT /notify-safekeepers", s.notify)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		defer func() {
			if recovered := recover(); recovered != nil {
				s.failures.Add(1)
				s.logger.Error("adapter panic", "panic_type", fmt.Sprintf("%T", recovered))
				reply(w, 503, map[string]any{"error": "adapter_unavailable"})
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) authorized(w http.ResponseWriter, r *http.Request, credential Credential) bool {
	token, err := credential.read()
	if err != nil {
		s.unavailable(w, "credential", err)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
		reply(w, 401, map[string]any{"error": "unauthorized"})
		return false
	}
	return true
}
func (s *Server) unavailable(w http.ResponseWriter, stage string, err error) {
	s.failures.Add(1)
	s.logger.Warn("adapter dependency unavailable", "stage", stage, "error_type", fmt.Sprintf("%T", err))
	reply(w, 503, map[string]any{"error": "adapter_unavailable"})
}
func (s *Server) path(collection, name string) string {
	prefix := "/api/v1/namespaces/" + url.PathEscape(s.config.Namespace) + "/"
	switch collection {
	case "vm":
		prefix = "/apis/vm.neon.tech/v1/namespaces/" + url.PathEscape(s.config.Namespace) + "/virtualmachines"
	case "deployment":
		prefix = "/apis/apps/v1/namespaces/" + url.PathEscape(s.config.Namespace) + "/deployments"
	default:
		prefix += collection
	}
	if name != "" {
		prefix += "/" + url.PathEscape(name)
	}
	return prefix
}

type route struct {
	ProjectID  string            `json:"project_id"`
	BranchID   string            `json:"branch_id"`
	TenantID   string            `json:"tenant_id"`
	Kind       string            `json:"kind"`
	Workload   string            `json:"workload"`
	Address    string            `json:"address"`
	Role       string            `json:"role"`
	Verifier   string            `json:"verifier"`
	Roles      map[string]string `json:"roles"`
	AllowedIPs []string          `json:"allowed_ips"`
	State      string            `json:"state"`
	NodeID     uint64            `json:"pageserver_node_id"`
	Template   map[string]any    `json:"vm_template"`
}

func (s *Server) routes(ctx context.Context) (map[string]route, error) {
	secret, err := s.kube.Request(ctx, http.MethodGet, s.path("secrets", s.config.RoutesSecret), nil)
	if err != nil {
		return nil, err
	}
	value, ok := nested(secret, "data", "routes.json").(string)
	if !ok {
		return nil, errors.New("routes field missing")
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("invalid routes encoding/size")
	}
	var routes map[string]route
	if err = json.Unmarshal(raw, &routes); err != nil || routes == nil {
		return nil, errors.New("invalid route document")
	}
	return routes, nil
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := s.config.ProxyToken.read(); err != nil {
		s.unavailable(w, "credential", err)
		return
	}
	if _, err := s.config.HookToken.read(); err != nil {
		s.unavailable(w, "credential", err)
		return
	}
	if _, err := s.routes(ctx); err != nil {
		s.unavailable(w, "routes", err)
		return
	}
	if _, err := s.kube.Request(ctx, http.MethodGet, s.path("configmaps", s.config.NotificationsConfigMap), nil); err != nil {
		s.unavailable(w, "receipts", err)
		return
	}
	reply(w, 200, map[string]any{"status": "ready", "runtime": "go", "process_role": "proxy_adapter", "distributed_fencing": false})
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// Fixed labels only: no endpoint/role/token cardinality or route disclosure.
	_, _ = fmt.Fprintf(w, "# TYPE neon_adapter_requests_total counter\nneon_adapter_requests_total %d\n# TYPE neon_adapter_failures_total counter\nneon_adapter_failures_total %d\n# TYPE neon_adapter_cold_wakes_total counter\nneon_adapter_cold_wakes_total %d\n# TYPE neon_adapter_wakes_in_flight gauge\nneon_adapter_wakes_in_flight %d\n", s.requests.Load(), s.failures.Load(), s.coldWakes.Load(), len(s.wakeSlots))
}
func (s *Server) selected(w http.ResponseWriter, r *http.Request) (string, route, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values["endpointish"]) != 1 || !selectorName.MatchString(values.Get("endpointish")) {
		reply(w, 400, map[string]any{"error": "invalid_endpoint"})
		return "", route{}, false
	}
	key := values.Get("endpointish")
	routes, err := s.routes(r.Context())
	if err != nil {
		s.unavailable(w, "routes", err)
		return "", route{}, false
	}
	item, ok := routes[key]
	if !ok || (item.State != "" && item.State != "active" && item.State != "ready" && item.State != "suspended") {
		reply(w, 404, map[string]any{"error": "unknown endpoint"})
		return "", route{}, false
	}
	if err = s.validateRoute(key, item); err != nil {
		s.unavailable(w, "route_validation", err)
		return "", route{}, false
	}
	return key, item, true
}
func (s *Server) validateRoute(key string, item route) error {
	if item.ProjectID == "" || item.BranchID == "" || !dnsName.MatchString(item.Workload) || (item.Kind != "neonvm" && item.Kind != "deployment") {
		return errors.New("invalid route identity")
	}
	host, port, err := net.SplitHostPort(item.Address)
	if err != nil || port != "55433" || host != item.Workload+"."+s.config.Namespace+".svc.cluster.local" {
		return errors.New("route address outside owned Service")
	}
	if len(item.Template) > 0 {
		if item.Kind != "neonvm" || !strings.HasPrefix(key, "ep-") || nested(item.Template, "kind") != "VirtualMachine" || nested(item.Template, "apiVersion") != "vm.neon.tech/v1" || nested(item.Template, "metadata", "name") != item.Workload || nested(item.Template, "metadata", "labels", "neon-control/project-id") != item.ProjectID || nested(item.Template, "metadata", "labels", "neon-control/endpoint-id") != "ep_"+strings.TrimPrefix(key, "ep-") {
			return errors.New("VM template ownership mismatch")
		}
		if namespace := nested(item.Template, "metadata", "namespace"); namespace != nil && namespace != s.config.Namespace {
			return errors.New("VM namespace mismatch")
		}
		for _, key := range []string{"uid", "resourceVersion", "deletionTimestamp"} {
			if nested(item.Template, "metadata", key) != nil {
				return errors.New("VM template contains live identity")
			}
		}
		if item.Template["status"] != nil || item.Template["spec"] == nil {
			return errors.New("invalid VM template state")
		}
	}
	return nil
}
func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r, s.config.ProxyToken) {
		return
	}
	_, item, ok := s.selected(w, r)
	if !ok {
		return
	}
	values, _ := url.ParseQuery(r.URL.RawQuery)
	role := values.Get("role")
	if len(values["role"]) != 1 || role == "" || len(role) > 63 {
		reply(w, 400, map[string]any{"error": "invalid_role"})
		return
	}
	verifier := item.Roles[role]
	if len(item.Roles) == 0 && role == item.Role {
		verifier = item.Verifier
	}
	if verifier == "" {
		reply(w, 404, map[string]any{"error": "unknown role"})
		return
	}
	allowed := item.AllowedIPs
	if allowed == nil {
		allowed = []string{"0.0.0.0/0"}
	}
	reply(w, 200, map[string]any{"role_secret": verifier, "allowed_ips": allowed})
}
func (s *Server) jwks(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r, s.config.ProxyToken) {
		return
	}
	// No JWT database authentication is advertised by this adapter. It preserves
	// the pinned empty-list protocol instead of inventing user identity keys.
	reply(w, 200, map[string]any{"jwks": []any{}})
}

func nested(value map[string]any, names ...string) any {
	var item any = value
	for _, name := range names {
		m, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		item = m[name]
	}
	return item
}
func isStatus(err error, status int) bool {
	var e APIError
	return errors.As(err, &e) && e.Status == status
}
func pause(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type endpointLock struct {
	token      chan struct{}
	references int
}
type endpointLocks struct {
	mu    sync.Mutex
	items map[string]*endpointLock
}

func (locks *endpointLocks) acquire(ctx context.Context, key string) (func(), error) {
	locks.mu.Lock()
	if locks.items == nil {
		locks.items = make(map[string]*endpointLock)
	}
	item := locks.items[key]
	if item == nil {
		item = &endpointLock{token: make(chan struct{}, 1)}
		locks.items[key] = item
	}
	item.references++
	locks.mu.Unlock()
	releaseReference := func() {
		locks.mu.Lock()
		defer locks.mu.Unlock()
		item.references--
		if item.references == 0 {
			delete(locks.items, key)
		}
	}
	select {
	case item.token <- struct{}{}:
		return func() { <-item.token; releaseReference() }, nil
	case <-ctx.Done():
		releaseReference()
		return nil, ctx.Err()
	}
}

func decodeDocument(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
