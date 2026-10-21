package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 通过真实typed Kube REST/TLS链验证服务代理路径与原PG版本，不模拟生产函数
// 返回成功。此夹具不运行pageserver/storage-controller，实际存储E2E另验。
type dsgaTimelineAPI struct {
	mu                                 sync.Mutex
	p                                  createPayload
	target, parent                     map[string]any
	existing                           bool
	posts, gets, redirects             int
	lost, conflict, throttle, redirect bool
	body                               map[string]any
}

func (f *dsgaTimelineAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer timeline-center" {
		creationStatus(w, 403)
		return
	}
	base := "/api/v1/namespaces/neon/services/"
	target := base + "http:pageserver-managed:9898/proxy/v1/tenant/" + f.p.TenantID + "/timeline/" + f.p.TimelineID
	parent := base + "http:pageserver-managed:9898/proxy/v1/tenant/" + f.p.TenantID + "/timeline/" + f.p.ParentTimelineID
	create := base + "http:storage-controller:1234/proxy/v1/tenant/" + f.p.TenantID + "/timeline"
	if r.URL.Path == "/redirected" {
		f.redirects++
		_ = json.NewEncoder(w).Encode(f.target)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == target {
		f.gets++
		if f.redirect {
			w.Header().Set("Location", "/redirected")
			w.WriteHeader(302)
			return
		}
		if !f.existing {
			creationStatus(w, 404)
			return
		}
		_ = json.NewEncoder(w).Encode(f.target)
		return
	}
	if r.Method == http.MethodGet && f.p.ParentTimelineID != "" && r.URL.Path == parent {
		_ = json.NewEncoder(w).Encode(f.parent)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == create {
		f.posts++
		if json.NewDecoder(r.Body).Decode(&f.body) != nil {
			creationStatus(w, 400)
			return
		}
		if f.throttle && f.posts == 1 {
			w.Header().Set("Retry-After", "1")
			creationStatus(w, 429)
			return
		}
		f.existing = true
		if f.lost {
			f.lost = false
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if f.conflict {
			creationStatus(w, 409)
			return
		}
		_ = json.NewEncoder(w).Encode(f.target)
		return
	}
	creationStatus(w, 404)
}

func newDSGATimelineTest(t *testing.T, branch bool) (*dsgaTimelineAPI, *kubeClient, createPayload, dsgaCreationPlan) {
	t.Helper()
	p, _, plan := dsgaPlanFixture(t)
	if branch {
		p.ParentTimelineID, p.ParentLSN = strings.Repeat("c", 32), "0/1000"
	}
	f := &dsgaTimelineAPI{p: p, target: map[string]any{"tenant_id": p.TenantID, "timeline_id": p.TimelineID, "pg_version": 17, "ancestor_timeline_id": nil, "ancestor_lsn": nil}, parent: map[string]any{"tenant_id": p.TenantID, "timeline_id": p.ParentTimelineID, "pg_version": 17, "ancestor_timeline_id": strings.Repeat("d", 32), "ancestor_lsn": "0/800"}}
	if branch {
		f.target["ancestor_timeline_id"], f.target["ancestor_lsn"] = p.ParentTimelineID, "0/01000"
	}
	server := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return f, &kubeClient{base: server.URL, namespace: "neon", token: "timeline-center", http: server.Client()}, p, plan
}

func TestDSGATimelineUsesFrozenVersionAndRecoversUnknownWrites(t *testing.T) {
	for _, name := range []string{"root", "branch", "existing", "conflict", "lost_reply"} {
		t.Run(name, func(t *testing.T) {
			f, k, p, plan := newDSGATimelineTest(t, name != "root")
			switch name {
			case "existing":
				f.existing = true
			case "conflict":
				f.conflict = true
			case "lost_reply":
				f.lost = true
			}
			err := k.createDSGATimeline(context.Background(), p, plan)
			if name == "lost_reply" {
				if err == nil {
					t.Fatal("unknown POST was reported successful")
				}
				f.mu.Lock()
				posts := f.posts
				f.mu.Unlock()
				if posts != 1 {
					t.Fatal("unknown POST transparently retried")
				}
				// 明确第二次调用代表同一原Operation恢复，先GET已有目标，不再POST。
				err = k.createDSGATimeline(context.Background(), p, plan)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			expected := 1
			if name == "existing" {
				expected = 0
			}
			if f.posts != expected {
				t.Fatal("recovery recreated the original timeline")
			}
			if expected == 1 {
				if f.body["pg_version"] != float64(17) || f.body["new_timeline_id"] != p.TimelineID {
					t.Fatalf("storage used wrong version or target: %v", f.body)
				}
				if p.ParentTimelineID != "" && (f.body["ancestor_timeline_id"] != p.ParentTimelineID || f.body["ancestor_start_lsn"] != p.ParentLSN) {
					t.Fatal("original ancestor/LSN not sent")
				}
			}
		})
	}
}

func TestDSGATimelineRejectsVersionAncestryAndTransportDowngrade(t *testing.T) {
	cases := map[string]func(*dsgaTimelineAPI){
		"target_pg16":               func(f *dsgaTimelineAPI) { f.existing = true; f.target["pg_version"] = 16 },
		"target_missing_version":    func(f *dsgaTimelineAPI) { f.existing = true; delete(f.target, "pg_version") },
		"target_fractional_version": func(f *dsgaTimelineAPI) { f.existing = true; f.target["pg_version"] = 17.5 },
		"target_string_version":     func(f *dsgaTimelineAPI) { f.existing = true; f.target["pg_version"] = "17" },
		"target_tenant":             func(f *dsgaTimelineAPI) { f.existing = true; f.target["tenant_id"] = strings.Repeat("f", 32) },
		"target_timeline":           func(f *dsgaTimelineAPI) { f.existing = true; f.target["timeline_id"] = strings.Repeat("f", 32) },
		"target_parent": func(f *dsgaTimelineAPI) {
			f.existing = true
			f.target["ancestor_timeline_id"] = strings.Repeat("f", 32)
		},
		"target_lsn":      func(f *dsgaTimelineAPI) { f.existing = true; f.target["ancestor_lsn"] = "0/2000" },
		"parent_pg16":     func(f *dsgaTimelineAPI) { f.parent["pg_version"] = 16 },
		"parent_identity": func(f *dsgaTimelineAPI) { f.parent["timeline_id"] = strings.Repeat("f", 32) },
		"created_pg16":    func(f *dsgaTimelineAPI) { f.target["pg_version"] = 16 },
		"conflict_pg16":   func(f *dsgaTimelineAPI) { f.conflict = true; f.target["pg_version"] = 16 },
		"throttle":        func(f *dsgaTimelineAPI) { f.throttle = true },
		"redirect":        func(f *dsgaTimelineAPI) { f.redirect = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, k, p, plan := newDSGATimelineTest(t, true)
			mutate(f)
			err := k.createDSGATimeline(context.Background(), p, plan)
			if err == nil {
				t.Fatalf("unsafe storage fact or transport %s accepted", name)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			expected := 0
			if name == "created_pg16" || name == "conflict_pg16" || name == "throttle" {
				expected = 1
			}
			if f.posts != expected || f.redirects != 0 {
				t.Fatalf("unsafe storage %s made extra POST or followed redirect: %d %d", name, f.posts, f.redirects)
			}
		})
	}
}

func TestDSGATimelineRejectsPlanBeforeStorageIO(t *testing.T) {
	f, k, p, plan := newDSGATimelineTest(t, false)
	plan.PostgresVersion = 16
	if !errors.Is(k.createDSGATimeline(context.Background(), p, plan), errDSGACreationPlan) {
		t.Fatal("invalid plan reached storage")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || f.gets != 0 {
		t.Fatal("invalid plan performed storage IO")
	}
}
