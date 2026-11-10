package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type dsgaTenantTestAPI struct {
	mu        sync.Mutex
	posts     int
	exists    bool
	mode      string
	fact      map[string]any
	afterPost func()
	storage   *dsgaStorageClient
}

// 从实际公开PG17接受路由生成原随机plan/Operation，VM尚未创建；真实元数据库
// 和typed TLS链执行。这里只模拟storage-controller协议，不冒称真实存储E2E。
func dsgaTenantFixture(t *testing.T, mode string) (context.Context, *dsgaCreationBinding, *dsgaTenantTestAPI) {
	t.Helper()
	f := newDSGAProjectFixture(t)
	key := "tenant-original-project"
	if response := f.request("creation_admin", key, f.body); response.Code != 202 {
		t.Fatal("project not accepted")
	}
	op, _, _ := f.reservation(key)
	f.exec(`UPDATE operations SET state='running',lease_owner='tenant-worker',lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, op)
	binding, err := f.s.loadDSGACreationBinding(f.ctx, op, "tenant-worker")
	if err != nil {
		t.Fatal(err)
	}
	tenant := binding.intent.TenantID
	api := &dsgaTenantTestAPI{mode: mode, fact: map[string]any{"tenant_id": tenant, "shards": []any{map[string]any{"tenant_shard_id": tenant}}, "policy": map[string]any{"Attached": 0}}}
	if mode == "existing_sharded" {
		api.exists = true
		api.fact["shards"] = []any{map[string]any{"tenant_shard_id": tenant + "-0002"}, map[string]any{"tenant_shard_id": tenant + "-0102"}}
	}
	base := "/"
	api.storage = dsgaStorageTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		scope, allowed := storageRequestScope(r.Method, strings.TrimPrefix(r.URL.Path, "/"))
		if !allowed || !validDSGAStorageToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), api.storage.publicKey, scope) {
			creationStatus(w, 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == base+"control/v1/tenant/"+tenant {
			if !api.exists {
				creationStatus(w, 404)
				return
			}
			if api.mode == "duplicate" {
				_, _ = w.Write([]byte(`{"tenant_id":"foreign","tenant_id":"` + tenant + `","shards":[{"tenant_shard_id":"` + tenant + `"}]}`))
				return
			}
			_ = json.NewEncoder(w).Encode(api.fact)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != base+"v1/tenant" {
			creationStatus(w, 404)
			return
		}
		api.posts++
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["new_tenant_id"] != tenant {
			t.Error("Tenant POST differs from original official request")
			creationStatus(w, 400)
			return
		}
		if api.mode == "429" {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(metav1.Status{Reason: metav1.StatusReasonTooManyRequests, Code: 429})
			return
		}
		api.exists = true
		if api.afterPost != nil {
			api.afterPost()
		}
		if api.mode == "unknown" {
			api.mode = "success"
			creationStatus(w, 504)
			return
		}
		if api.mode == "conflict" || api.mode == "conflict_drift" {
			if api.mode == "conflict_drift" {
				api.fact["tenant_id"] = "foreign"
			}
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(metav1.Status{Reason: metav1.StatusReasonConflict, Code: 409})
			return
		}
		if api.mode == "drift" {
			api.fact["tenant_id"] = "foreign"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"shards": []any{map[string]any{"shard_id": tenant, "node_id": 2, "generation": 1}}})
	}))
	return f.ctx, binding, api
}

func tenantFact(t *testing.T, ctx context.Context, binding *dsgaCreationBinding) (string, string) {
	t.Helper()
	var tenant, xmin string
	if err := binding.server.db.QueryRow(ctx, `SELECT tenant_id,xmin::text FROM dsga_creation_tenants WHERE operation_id=$1`, binding.operationID).Scan(&tenant, &xmin); err != nil {
		t.Fatal(err)
	}
	return tenant, xmin
}

func TestDSGATenantOriginalRecoveryNeverRecreatesConfirmedStorage(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "conflict", "conflict_drift", "existing_sharded", "429", "drift", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			ctx, binding, api := dsgaTenantFixture(t, mode)
			err := binding.prepareTenantWithStorage(ctx, api.storage)
			if mode == "unknown" || mode == "429" || mode == "drift" || mode == "duplicate" || mode == "conflict_drift" {
				if err == nil {
					t.Fatal("failed/ambiguous/mismatched Tenant write fabricated confirmation")
				}
				api.mu.Lock()
				posts := api.posts
				api.mu.Unlock()
				if posts != 1 {
					t.Fatal("Tenant POST transparently retried")
				}
				if mode != "unknown" {
					return
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := binding.prepareTenantWithStorage(ctx, api.storage); err != nil {
				t.Fatal(err)
			}
			before, xmin := tenantFact(t, ctx, binding)
			if before != binding.intent.TenantID {
				t.Fatal("wrong Tenant confirmed")
			}
			if err := binding.prepareTenantWithStorage(ctx, api.storage); err != nil {
				t.Fatal(err)
			}
			after, afterXmin := tenantFact(t, ctx, binding)
			if before != after || xmin != afterXmin {
				t.Fatal("Tenant recovery rewrote identity/xmin")
			}
			// 旧框架可把步骤改回running，但不得因此遗忘已创建存储的独立事实。
			if _, err := binding.server.db.Exec(ctx, `UPDATE operation_steps SET state='running' WHERE operation_id=$1 AND ordinal=0`, binding.operationID); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			posts := api.posts
			api.exists = false
			api.mu.Unlock()
			if err := binding.prepareTenantWithStorage(ctx, api.storage); !errors.Is(err, errDSGATenantUnconfirmed) {
				t.Fatal("confirmed Tenant unavailable state was not preserved")
			}
			api.mu.Lock()
			afterPosts := api.posts
			api.mu.Unlock()
			if afterPosts != posts {
				t.Fatal("known Tenant loss caused POST")
			}
		})
	}
}

func TestDSGATenantExpiredLeaseAndSQLBindingFailurePreserveOriginal(t *testing.T) {
	ctx, binding, api := dsgaTenantFixture(t, "success")
	api.afterPost = func() {
		if _, err := binding.server.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, binding.operationID); err != nil {
			t.Error("external API held SQL Operation lock")
		}
	}
	if err := binding.prepareTenantWithStorage(ctx, api.storage); err == nil {
		t.Fatal("expired worker confirmed Tenant")
	}
	var count int
	if err := binding.server.db.QueryRow(ctx, `SELECT count(*) FROM dsga_creation_tenants WHERE operation_id=$1`, binding.operationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired worker left a Tenant fact")
	}
	api.mu.Lock()
	api.afterPost = nil
	api.mu.Unlock()
	if _, err := binding.server.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, binding.operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := binding.server.db.Exec(ctx, `CREATE FUNCTION fail_tenant_binding() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'test tenant bind failure'; END$$; CREATE TRIGGER fail_tenant_binding BEFORE INSERT ON dsga_creation_tenants FOR EACH ROW EXECUTE FUNCTION fail_tenant_binding()`); err != nil {
		t.Fatal(err)
	}
	if err := binding.prepareTenantWithStorage(ctx, api.storage); err == nil {
		t.Fatal("failed SQL Tenant binding reported success")
	}
	if _, err := binding.server.db.Exec(ctx, `DROP TRIGGER fail_tenant_binding ON dsga_creation_tenants`); err != nil {
		t.Fatal(err)
	}
	if err := binding.prepareTenantWithStorage(ctx, api.storage); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	posts := api.posts
	api.mu.Unlock()
	if posts != 1 {
		t.Fatal("SQL failure recovery recreated original Tenant")
	}
}

func TestDSGATenantForwardReplayAndImmutableGuard(t *testing.T) {
	ctx, binding, api := dsgaTenantFixture(t, "success")
	if err := binding.prepareTenantWithStorage(ctx, api.storage); err != nil {
		t.Fatal(err)
	}
	before, xmin := tenantFact(t, ctx, binding)
	for range 2 {
		if err := migrate(ctx, binding.server.db); err != nil {
			t.Fatal(err)
		}
	}
	after, afterXmin := tenantFact(t, ctx, binding)
	if before != after || xmin != afterXmin {
		t.Fatal("migration replay rewrote Tenant fact")
	}
	for _, query := range []string{`UPDATE dsga_creation_tenants SET tenant_id=repeat('f',32) WHERE operation_id=$1`, `UPDATE dsga_creation_tenants SET created_at=created_at WHERE operation_id=$1`, `DELETE FROM dsga_creation_tenants WHERE operation_id=$1`} {
		_, err := binding.server.db.Exec(ctx, query, binding.operationID)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatal("database allowed original Tenant mutation")
		}
	}
}

func TestDSGATenantShardIdentityRejectsHolesAliasesAndOtherTenants(t *testing.T) {
	tenant := strings.Repeat("a", 32)
	fact := func(ids ...string) map[string]any {
		shards := make([]any, len(ids))
		for i, id := range ids {
			shards[i] = map[string]any{"tenant_shard_id": id}
		}
		return map[string]any{"tenant_id": tenant, "shards": shards}
	}
	for _, ids := range [][]string{{tenant}, {tenant + "-0001"}, {tenant + "-0102", tenant + "-0002"}} {
		if err := validateDSGATenant(fact(ids...), tenant); err != nil {
			t.Fatal("official complete shard identity rejected")
		}
	}
	for name, invalid := range map[string]map[string]any{
		"empty": fact(), "hole": fact(tenant + "-0002"), "duplicate": fact(tenant+"-0002", tenant+"-0002"), "mixed": fact(tenant, tenant+"-0001"),
		"foreign": fact(strings.Repeat("b", 32)), "out_of_range": fact(tenant + "-0101"), "zero_alias": fact(tenant + "-0000"), "uppercase": fact(tenant + "-000A"),
		"case_root": {"Tenant_ID": tenant, "shards": []any{map[string]any{"tenant_shard_id": tenant}}}, "case_shard": {"tenant_id": tenant, "shards": []any{map[string]any{"Tenant_Shard_ID": tenant}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateDSGATenant(invalid, tenant); err == nil {
				t.Fatal("incomplete/ambiguous Tenant identity accepted")
			}
		})
	}
}

func TestDSGATenantRejectsEndpointCreationAndForeignSQLIdentity(t *testing.T) {
	ctx, s, _, _, _, binding, _ := credentialSourceFixture(t)
	// endpoint创建即使使用相同tenant，也绝不能在存储丢失时越权重建根Tenant。
	// 故意移除可用网络地址：正确的action拒绝应早于任何客户端/HTTP创建。
	s.kube.base = "http://must-not-be-used.invalid"
	if err := binding.prepareTenant(ctx); !errors.Is(err, errDSGACreationIdentity) {
		t.Fatal("non-project action reached Tenant preparation")
	}
	_, err := s.db.Exec(ctx, `INSERT INTO dsga_creation_tenants(operation_id,tenant_id) VALUES($1,$2)`, binding.operationID, binding.intent.TenantID)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
		t.Fatal("database accepted endpoint action as root Tenant source")
	}
	ctx, project, _ := dsgaTenantFixture(t, "success")
	_, err = project.server.db.Exec(ctx, `INSERT INTO dsga_creation_tenants(operation_id,tenant_id) VALUES($1,$2)`, project.operationID, strings.Repeat("f", 32))
	pgerr = nil
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
		t.Fatal("database accepted another Tenant under original project")
	}
}
