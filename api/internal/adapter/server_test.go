package adapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSelector = "ep-0123456789abcdef"
const testTenant = "0123456789abcdef0123456789abcdef"
const testTimeline = "abcdef0123456789abcdef0123456789"

type fakeKube struct {
	mu        sync.Mutex
	calls     map[string]int
	document  map[string]any
	configmap map[string]any
	vm        map[string]any
	on        func(context.Context, string, string, any) (map[string]any, error, bool)
}

func copyMap(input map[string]any) map[string]any {
	raw, _ := json.Marshal(input)
	var output map[string]any
	_ = json.Unmarshal(raw, &output)
	return output
}
func (f *fakeKube) Request(ctx context.Context, method, path string, body any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method+" "+path]++
	if f.on != nil {
		value, err, done := f.on(ctx, method, path, body)
		if done {
			return value, err
		}
	}
	switch {
	case strings.Contains(path, "/secrets/"):
		encoded, _ := json.Marshal(f.document)
		return map[string]any{"data": map[string]any{"routes.json": base64.StdEncoding.EncodeToString(encoded)}}, nil
	case strings.Contains(path, "/configmaps/"):
		if method == http.MethodPut {
			f.configmap = copyMap(body.(map[string]any))
		}
		return copyMap(f.configmap), nil
	case strings.Contains(path, "/services/http:storage-controller:"):
		return map[string]any{"shards": []any{map[string]any{"node_attached": float64(2)}}}, nil
	case strings.Contains(path, "/virtualmachines"):
		if method == http.MethodPost {
			if f.vm != nil {
				return nil, APIError{409}
			}
			f.vm = copyMap(body.(map[string]any))
			f.vm["metadata"].(map[string]any)["uid"] = "owned-new-uid"
			f.vm["status"] = map[string]any{"phase": "Running"}
		}
		if f.vm == nil {
			return nil, APIError{404}
		}
		return copyMap(f.vm), nil
	}
	return nil, errors.New("unexpected request")
}
func fixture(t *testing.T) (*Server, *fakeKube, Config) {
	t.Helper()
	config := Config{Namespace: "neon", RoutesSecret: "neon-control-routes", NotificationsConfigMap: "neon-control-notifications", ProxyToken: Credential{Value: "proxy-test-credential"}, HookToken: Credential{Value: "hook-test-credential"}, WakeTimeout: time.Second, PollInterval: time.Millisecond, MaxConcurrentWakes: 8, PageserverNodeID: 2}
	template := map[string]any{"apiVersion": "vm.neon.tech/v1", "kind": "VirtualMachine", "metadata": map[string]any{"name": "cp-0123456789abcdef", "labels": map[string]any{"neon-control/project-id": "prj_test", "neon-control/endpoint-id": "ep_0123456789abcdef"}}, "spec": map[string]any{"guest": map[string]any{}}}
	document := map[string]any{testSelector: map[string]any{"project_id": "prj_test", "branch_id": "br_test", "tenant_id": testTenant, "kind": "neonvm", "workload": "cp-0123456789abcdef", "address": "cp-0123456789abcdef.neon.svc.cluster.local:55433", "roles": map[string]any{"cloud_admin": "fixture-verifier"}, "pageserver_node_id": 2, "vm_template": template}}
	fake := &fakeKube{calls: map[string]int{}, document: document, configmap: map[string]any{"metadata": map[string]any{"resourceVersion": "1"}, "data": map[string]any{"receipts.json": "{}", "unrelated": "retained"}}}
	server, err := New(config, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return server, fake, config
}
func request(server *Server, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	return w
}
func routeValue(fake *fakeKube) map[string]any { return fake.document[testSelector].(map[string]any) }
func TestProxyProtocolAuthorizationAndNoSecretLeak(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, token string
		status                    int
	}{
		{"health", "GET", "/healthz", "", 200}, {"ready", "GET", "/readyz", "", 200},
		{"missing_token", "GET", "/cplane/get_endpoint_access_control?endpointish=" + testSelector + "&role=cloud_admin", "", 401},
		{"hook_token_is_not_proxy", "GET", "/cplane/wake_compute?endpointish=" + testSelector, "hook-test-credential", 401},
		{"proxy_token_is_not_hook", "PUT", "/notify-attach", "proxy-test-credential", 401},
		{"valid_role", "GET", "/cplane/get_endpoint_access_control?endpointish=" + testSelector + "&role=cloud_admin", "proxy-test-credential", 200},
		{"unknown_role", "GET", "/cplane/get_endpoint_access_control?endpointish=" + testSelector + "&role=absent", "proxy-test-credential", 404},
		{"unknown_endpoint", "GET", "/cplane/get_endpoint_access_control?endpointish=absent&role=cloud_admin", "proxy-test-credential", 404},
		{"missing_selector", "GET", "/cplane/wake_compute", "proxy-test-credential", 400},
		{"duplicate_selector", "GET", "/cplane/wake_compute?endpointish=" + testSelector + "&endpointish=other", "proxy-test-credential", 400},
		{"bad_query", "GET", "/cplane/wake_compute?endpointish=%zz", "proxy-test-credential", 400},
		{"duplicate_role", "GET", "/cplane/get_endpoint_access_control?endpointish=" + testSelector + "&role=cloud_admin&role=other", "proxy-test-credential", 400},
		{"jwks_no_managed_auth_claim", "GET", "/cplane/endpoints/ep-test/jwks", "proxy-test-credential", 200},
		{"unknown_path", "GET", "/unknown", "proxy-test-credential", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _ := fixture(t)
			w := request(s, tc.method, tc.path, tc.token, "")
			if w.Code != tc.status {
				t.Fatalf("status %d expected %d", w.Code, tc.status)
			}
			if tc.name != "valid_role" && strings.Contains(w.Body.String(), "fixture-verifier") {
				t.Fatal("verifier disclosure")
			}
			for key := range f.calls {
				if strings.HasPrefix(key, "POST ") || strings.HasPrefix(key, "PUT ") {
					t.Fatal("read/auth mutated state")
				}
			}
		})
	}
}
func TestProxyCredentialFileRotationAndFailClosed(t *testing.T) {
	s, _, c := fixture(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(c.ProxyToken.Value), 0600); err != nil {
		t.Fatal(err)
	}
	s.config.ProxyToken.File = file
	path := "/cplane/get_endpoint_access_control?endpointish=" + testSelector + "&role=cloud_admin"
	if request(s, "GET", path, c.ProxyToken.Value, "").Code != 200 {
		t.Fatal("original token failed")
	}
	if err := os.WriteFile(file, []byte("rotated-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if request(s, "GET", path, c.ProxyToken.Value, "").Code != 401 || request(s, "GET", path, "rotated-test-credential", "").Code != 200 {
		t.Fatal("rotation failed")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if request(s, "GET", path, c.ProxyToken.Value, "").Code != 503 {
		t.Fatal("fell back to stale env credential")
	}
}
func TestWakeCreatesOneOwnedVMAndReleasesLock(t *testing.T) {
	s, f, _ := fixture(t)
	path := "/cplane/wake_compute?endpointish=" + testSelector
	for i := 0; i < 2; i++ {
		w := request(s, "GET", path, "proxy-test-credential", "")
		if w.Code != 200 {
			t.Fatalf("wake failed %d", w.Code)
		}
		if i == 0 && !strings.Contains(w.Body.String(), "pool_miss") {
			t.Fatal("cold classification missing")
		}
		if i == 1 && !strings.Contains(w.Body.String(), "warm") {
			t.Fatal("warm classification missing")
		}
	}
	if f.calls["POST "+s.path("vm", "")] != 1 {
		t.Fatal("repeated VM creation")
	}
	if len(s.locks.items) != 0 || len(s.wakeSlots) != 0 {
		t.Fatal("wake lock leaked")
	}
}
func TestWakeRejectsForeignAndMalformedOwnership(t *testing.T) {
	for _, kind := range []string{"foreign_vm", "missing_uid", "foreign_template", "template_live_uid", "foreign_namespace", "external_target", "deleted_route", "replacement_conflict", "unknown_mutation_outcome"} {
		t.Run(kind, func(t *testing.T) {
			s, f, _ := fixture(t)
			value := routeValue(f)
			switch kind {
			case "foreign_vm", "missing_uid":
				f.vm = copyMap(value["vm_template"].(map[string]any))
				f.vm["status"] = map[string]any{"phase": "Running"}
				if kind == "foreign_vm" {
					f.vm["metadata"].(map[string]any)["uid"] = "foreign"
					f.vm["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/project-id"] = "other"
				}
			case "foreign_template":
				value["vm_template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/endpoint-id"] = "ep_other"
			case "template_live_uid":
				value["vm_template"].(map[string]any)["metadata"].(map[string]any)["uid"] = "must-not-adopt"
			case "foreign_namespace":
				value["vm_template"].(map[string]any)["metadata"].(map[string]any)["namespace"] = "other"
			case "external_target":
				value["address"] = "attacker.example:55433"
			case "deleted_route":
				value["state"] = "deleted"
			case "replacement_conflict":
				f.on = func(ctx context.Context, method, path string, body any) (map[string]any, error, bool) {
					if method == "POST" {
						f.vm = copyMap(body.(map[string]any))
						f.vm["metadata"].(map[string]any)["uid"] = "foreign"
						f.vm["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/project-id"] = "other"
						return nil, APIError{409}, true
					}
					return nil, nil, false
				}
			case "unknown_mutation_outcome":
				f.on = func(ctx context.Context, method, path string, body any) (map[string]any, error, bool) {
					if method == "POST" {
						return nil, errors.New("unknown outcome"), true
					}
					return nil, nil, false
				}
			}
			w := request(s, "GET", "/cplane/wake_compute?endpointish="+testSelector, "proxy-test-credential", "")
			if w.Code != 503 && w.Code != 404 {
				t.Fatalf("foreign/invalid wake allowed: %d", w.Code)
			}
			if f.calls["POST "+s.path("vm", "")] > 1 {
				t.Fatal("unknown mutation replayed")
			}
		})
	}
}
func TestConcurrentWakeSerializesAndBoundsAdmission(t *testing.T) {
	s, f, _ := fixture(t)
	var wg sync.WaitGroup
	failed := make(chan int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(s, "GET", "/cplane/wake_compute?endpointish="+testSelector, "proxy-test-credential", "")
			if w.Code != 200 {
				failed <- w.Code
			}
		}()
	}
	wg.Wait()
	close(failed)
	for code := range failed {
		t.Fatalf("concurrent wake failed %d", code)
	}
	if f.calls["POST "+s.path("vm", "")] != 1 {
		t.Fatal("concurrent duplicate creation")
	}
	for i := 0; i < cap(s.wakeSlots); i++ {
		s.wakeSlots <- struct{}{}
	}
	w := request(s, "GET", "/cplane/wake_compute?endpointish="+testSelector, "proxy-test-credential", "")
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("unbounded admission")
	}
}
func TestReadinessDependencyFailureDoesNotDiscloseBody(t *testing.T) {
	s, f, _ := fixture(t)
	f.on = func(context.Context, string, string, any) (map[string]any, error, bool) {
		return nil, errors.New("sensitive dependency detail"), true
	}
	w := request(s, "GET", "/readyz", "", "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatal("readiness did not fail closed")
	}
	w = request(s, "GET", "/metrics", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "neon_adapter_failures_total") || strings.Contains(w.Body.String(), testSelector) {
		t.Fatal("metrics disclosure/schema")
	}
}

func TestWakeRouteClosureAndCancellation(t *testing.T) {
	for _, kind := range []string{"closed_before_create", "closed_before_response", "caller_cancelled", "bounded_timeout"} {
		t.Run(kind, func(t *testing.T) {
			s, f, _ := fixture(t)
			count := 0
			s.config.WakeTimeout = 10 * time.Millisecond
			f.on = func(ctx context.Context, method, path string, body any) (map[string]any, error, bool) {
				if strings.Contains(path, "/secrets/") {
					count++
					if (kind == "closed_before_create" && count == 3) || (kind == "closed_before_response" && count == 4) {
						routeValue(f)["state"] = "deleted"
					}
				}
				if kind == "bounded_timeout" && strings.Contains(path, "/virtualmachines") && method == "GET" {
					return map[string]any{"metadata": map[string]any{"uid": "owned", "name": "cp-0123456789abcdef", "labels": map[string]any{"neon-control/project-id": "prj_test", "neon-control/endpoint-id": "ep_0123456789abcdef"}}, "status": map[string]any{"phase": "Starting"}}, nil, true
				}
				return nil, nil, false
			}
			r := httptest.NewRequest("GET", "/cplane/wake_compute?endpointish="+testSelector, nil)
			r.Header.Set("Authorization", "Bearer proxy-test-credential")
			if kind == "caller_cancelled" {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 503 {
				t.Fatalf("closed/cancelled wake allowed %d", w.Code)
			}
			if (kind == "closed_before_create" || kind == "caller_cancelled") && f.calls["POST "+s.path("vm", "")] != 0 {
				t.Fatal("closed/cancelled wake mutated")
			}
		})
	}
}
