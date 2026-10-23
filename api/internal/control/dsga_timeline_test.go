package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 真实元数据库与实际公开接受入口，只模拟原生controller协议；不运行K8s或
// pageserver，不把fixture中幂等重放等同于目标环境的持久恢复认证。
type dsgaTimelineAPI struct {
	mu        sync.Mutex
	binding   *dsgaCreationBinding
	storage   *dsgaStorageClient
	mode      string
	exists    bool
	posts     int
	request   []byte
	shards    []any
	afterPost func()
}

func dsgaTimelineShards(tenant, timeline string) []any {
	return []any{
		map[string]any{"tenant_id": tenant + "-0002", "timeline_id": timeline, "pg_version": 17, "ancestor_timeline_id": nil, "ancestor_lsn": nil, "state": "Active"},
		map[string]any{"tenant_id": tenant + "-0102", "timeline_id": timeline, "pg_version": 17, "ancestor_timeline_id": nil, "ancestor_lsn": nil, "state": "Active"},
	}
}

func newDSGATimelineFixture(t *testing.T, mode string) (context.Context, *dsgaCreationBinding, *dsgaTimelineAPI) {
	t.Helper()
	f := newDSGAProjectFixture(t)
	key := "timeline-original-project"
	if response := f.request("creation_admin", key, f.body); response.Code != 202 {
		t.Fatal("project not accepted")
	}
	op, _, _ := f.reservation(key)
	f.exec(`UPDATE operations SET state='running',lease_owner='timeline-worker',lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, op)
	b, err := f.s.loadDSGACreationBinding(f.ctx, op, "timeline-worker")
	if err != nil {
		t.Fatal(err)
	}
	p := b.intent.createPayload
	a := &dsgaTimelineAPI{binding: b, mode: mode, exists: mode == "existing", shards: dsgaTimelineShards(p.TenantID, p.TimelineID)}
	a.storage = dsgaStorageTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		scope, allowed := storageRequestScope(r.Method, strings.TrimPrefix(r.URL.Path, "/"))
		if !allowed || !validDSGAStorageToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), a.storage.publicKey, scope) {
			creationStatus(w, 403)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/control/v1/tenant/"+p.TenantID {
			_ = json.NewEncoder(w).Encode(map[string]any{"tenant_id": p.TenantID, "shards": []any{map[string]any{"tenant_shard_id": p.TenantID + "-0002"}, map[string]any{"tenant_shard_id": p.TenantID + "-0102"}}})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/control/v1/tenant/"+p.TenantID+"/timeline/"+p.TimelineID {
			if !a.exists {
				creationStatus(w, 503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"shards": a.shards})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenant/"+p.TenantID+"/timeline" {
			creationStatus(w, 404)
			return
		}
		a.posts++
		var got dsgaTimelineRequest
		if json.NewDecoder(r.Body).Decode(&got) != nil {
			t.Error("invalid timeline POST")
			creationStatus(w, 400)
			return
		}
		raw, _ := json.Marshal(got)
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var stored []byte
		var unknown bool
		if err := b.server.db.QueryRow(ctx, `SELECT request,confirmed_at IS NULL FROM dsga_creation_timelines WHERE operation_id=$1`, b.operationID).Scan(&stored, &unknown); err != nil || !unknown {
			t.Error("POST preceded durable original intent or held SQL lock")
			creationStatus(w, 500)
			return
		}
		var original dsgaTimelineRequest
		if decodeDSGAObject(stored, &original) != nil || original != got || got.TimelineID != p.TimelineID || got.PostgresVersion != 17 || got.ParentTimelineID != "" || got.ParentLSN != "" {
			t.Error("POST differs from frozen intent")
			creationStatus(w, 400)
			return
		}
		if a.request != nil && string(a.request) != string(raw) {
			t.Error("explicit replay changed original request")
		}
		a.request = raw
		if a.afterPost != nil {
			a.afterPost()
		}
		if a.mode == "throttle" {
			creationStatus(w, 429)
			return
		}
		if a.mode == "partial_unknown" && a.posts == 1 {
			creationStatus(w, 504)
			return
		}
		a.exists = true
		if a.mode == "second_pg16" {
			a.shards[1].(map[string]any)["pg_version"] = 16
		}
		if a.mode == "conflict" {
			creationStatus(w, 409)
			return
		}
		_ = json.NewEncoder(w).Encode(a.shards[0])
	}))
	if err := b.prepareTenantWithStorage(f.ctx, a.storage); err != nil {
		t.Fatal(err)
	}
	return f.ctx, b, a
}

func TestDSGATimelineAggregateOriginalRecoveryAndConfirmedLoss(t *testing.T) {
	for _, mode := range []string{"success", "partial_unknown", "conflict", "existing", "second_pg16", "throttle"} {
		t.Run(mode, func(t *testing.T) {
			ctx, b, a := newDSGATimelineFixture(t, mode)
			err := b.prepareTimelineWithStorage(ctx, a.storage)
			if mode == "partial_unknown" || mode == "second_pg16" || mode == "throttle" {
				if err == nil {
					t.Fatal("unknown/invalid storage fabricated confirmation")
				}
				var confirmed bool
				if err := b.server.db.QueryRow(ctx, `SELECT confirmed_at IS NOT NULL FROM dsga_creation_timelines WHERE operation_id=$1`, b.operationID).Scan(&confirmed); err != nil || confirmed {
					t.Fatal("unconfirmed request lost its state")
				}
				if a.posts != 1 {
					t.Fatal("POST transparently retried")
				}
				if mode != "partial_unknown" {
					return
				}
				// 控制器聚合GET仍503：它不能证明没有执行；仅显式恢复相同原意图。
				err = b.prepareTimelineWithStorage(ctx, a.storage)
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if mode == "existing" {
				expected = 0
			}
			if mode == "partial_unknown" {
				expected = 2
			}
			if a.posts != expected {
				t.Fatal("unexpected original POST count")
			}
			var xmin string
			if err := b.server.db.QueryRow(ctx, `SELECT xmin::text FROM dsga_creation_timelines WHERE operation_id=$1 AND confirmed_at IS NOT NULL`, b.operationID).Scan(&xmin); err != nil {
				t.Fatal(err)
			}
			if err := b.prepareTimelineWithStorage(ctx, a.storage); err != nil {
				t.Fatal(err)
			}
			var after string
			if err := b.server.db.QueryRow(ctx, `SELECT xmin::text FROM dsga_creation_timelines WHERE operation_id=$1`, b.operationID).Scan(&after); err != nil || after != xmin || a.posts != expected {
				t.Fatal("confirmed recovery rewrote fact")
			}
			a.mu.Lock()
			a.exists = false
			a.mu.Unlock()
			if err := b.prepareTimelineWithStorage(ctx, a.storage); !errors.Is(err, errDSGATimelineUnconfirmed) || a.posts != expected {
				t.Fatal("confirmed loss automatically recreated empty timeline")
			}
		})
	}
}

func TestDSGATimelineIntentGuardAndExpiredConfirmation(t *testing.T) {
	ctx, b, a := newDSGATimelineFixture(t, "success")
	a.afterPost = func() {
		if _, err := b.server.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, b.operationID); err != nil {
			t.Error(err)
		}
	}
	if err := b.prepareTimelineWithStorage(ctx, a.storage); err == nil {
		t.Fatal("expired worker confirmed timeline")
	}
	var confirmed bool
	if err := b.server.db.QueryRow(ctx, `SELECT confirmed_at IS NOT NULL FROM dsga_creation_timelines WHERE operation_id=$1`, b.operationID).Scan(&confirmed); err != nil || confirmed {
		t.Fatal("expired confirmation escaped")
	}
	for _, query := range []string{`DELETE FROM dsga_creation_timelines WHERE operation_id=$1`, `UPDATE dsga_creation_timelines SET request=request||'{"pg_version":16}'::jsonb WHERE operation_id=$1`} {
		_, err := b.server.db.Exec(ctx, query, b.operationID)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Fatal("original timeline intent mutable")
		}
	}
	if _, err := b.server.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, b.operationID); err != nil {
		t.Fatal(err)
	}
	if err := b.prepareTimelineWithStorage(ctx, a.storage); err != nil || a.posts != 1 {
		t.Fatal("lease recovery recreated existing timeline", err)
	}
}

func TestDSGATimelineAggregateRejectsPartialWrongVersionAndAncestry(t *testing.T) {
	tenant, timeline, parent := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	valid := func() map[string]any { return map[string]any{"shards": dsgaTimelineShards(tenant, timeline)} }
	if err := validateDSGATimelineAggregate(valid(), tenant, timeline, 17, "", "", true); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(map[string]any){
		"missing_shard": func(v map[string]any) { v["shards"] = v["shards"].([]any)[:1] },
		"duplicate":     func(v map[string]any) { v["shards"].([]any)[1] = v["shards"].([]any)[0] },
		"foreign": func(v map[string]any) {
			v["shards"].([]any)[1].(map[string]any)["tenant_id"] = strings.Repeat("f", 32) + "-0102"
		},
		"fractional_pg":    func(v map[string]any) { v["shards"].([]any)[1].(map[string]any)["pg_version"] = 17.5 },
		"other_version":    func(v map[string]any) { v["shards"].([]any)[1].(map[string]any)["pg_version"] = 16 },
		"loading":          func(v map[string]any) { v["shards"].([]any)[1].(map[string]any)["state"] = "Loading" },
		"missing_ancestry": func(v map[string]any) { delete(v["shards"].([]any)[1].(map[string]any), "ancestor_timeline_id") },
		"case_alias": func(v map[string]any) {
			s := v["shards"].([]any)[1].(map[string]any)
			delete(s, "pg_version")
			s["PG_VERSION"] = 17
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := valid()
			change(v)
			if validateDSGATimelineAggregate(v, tenant, timeline, 17, "", "", true) == nil {
				t.Fatal("invalid aggregate accepted")
			}
		})
	}
	branch := valid()
	for _, raw := range branch["shards"].([]any) {
		s := raw.(map[string]any)
		s["ancestor_timeline_id"] = parent
		s["ancestor_lsn"] = "0/01000"
	}
	if err := validateDSGATimelineAggregate(branch, tenant, timeline, 17, parent, "0/1000", true); err != nil {
		t.Fatal("canonical-equivalent original LSN refused")
	}
	branch["shards"].([]any)[1].(map[string]any)["ancestor_lsn"] = "0/2000"
	if validateDSGATimelineAggregate(branch, tenant, timeline, 17, parent, "0/1000", true) == nil {
		t.Fatal("one shard ancestor drift accepted")
	}
}

func TestDSGATimelineIntentMigrationReplayAndLargeAggregate(t *testing.T) {
	ctx, b, a := newDSGATimelineFixture(t, "existing")
	if err := b.prepareTimelineWithStorage(ctx, a.storage); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err := b.server.db.QueryRow(ctx, `SELECT xmin::text FROM dsga_creation_timelines WHERE operation_id=$1`, b.operationID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, b.server.db); err != nil {
		t.Fatal(err)
	}
	if err := b.server.db.QueryRow(ctx, `SELECT xmin::text FROM dsga_creation_timelines WHERE operation_id=$1`, b.operationID).Scan(&after); err != nil || before != after {
		t.Fatal("migration replay rewrote original timeline")
	}
	// 完整观测可超过写协议的1MiB：实际TLS读取必须使用只读入口，仍逐片核验。
	c := dsgaStorageTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"shards": dsgaTimelineShards(b.intent.TenantID, b.intent.TimelineID), "future_metadata": strings.Repeat("x", (1<<20)+1)})
	}))
	p := b.intent.createPayload
	fact, err := c.request(ctx, http.MethodGet, "control/v1/tenant/"+p.TenantID+"/timeline/"+p.TimelineID, nil)
	if err != nil {
		t.Fatal("large bounded aggregate transport failed", err)
	}
	if err = validateDSGATimelineAggregate(fact, p.TenantID, p.TimelineID, 17, "", "", true); err != nil {
		t.Fatal(err)
	}
}
