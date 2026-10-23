package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type dsgaServicesTestAPI struct {
	mu        sync.Mutex
	objects   map[string]*core.Service
	posts     int
	mode      string
	afterPost func()
}

// 真实元数据库/原凭据/typed held→ready，Kube Service部分仅模拟API协议。
// 计数包含失败POST，GET返回复制对象，不覆盖同名赢家，不冒称K8s运行或SQL就绪。
func dsgaServicesFixture(t *testing.T, mode string) (context.Context, *dsgaCreationBinding, *dsgaServicesTestAPI) {
	ctx, binding, state, _, _ := dsgaServicesSourceFixture(t, mode)
	return ctx, binding, state
}

func dsgaServicesSourceFixture(t *testing.T, mode string) (context.Context, *dsgaCreationBinding, *dsgaServicesTestAPI, *creationTestAPI, **core.Secret) {
	t.Helper()
	ctx, _, _, f, _, binding, source := credentialSourceFixture(t)
	if _, err := binding.prepareVM(ctx); err != nil {
		t.Fatal(err)
	}
	state := &dsgaServicesTestAPI{objects: map[string]*core.Service{}, mode: mode}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer creation-kube-identity" {
			creationStatus(w, 403)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == f.kube.path("secret", brokerTestWorkload+"-credentials") {
			f.mu.Lock()
			defer f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(*source)
			return
		}
		if strings.HasPrefix(r.URL.Path, f.kube.path("service", "")) {
			state.mu.Lock()
			defer state.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				name := strings.TrimPrefix(r.URL.Path, f.kube.path("service", "")+"/")
				if state.objects[name] == nil {
					creationStatus(w, 404)
					return
				}
				_ = json.NewEncoder(w).Encode(state.objects[name])
				return
			}
			if r.Method != http.MethodPost || r.URL.Path != f.kube.path("service", "") {
				creationStatus(w, 405)
				return
			}
			state.posts++
			var proposed core.Service
			if json.NewDecoder(r.Body).Decode(&proposed) != nil {
				creationStatus(w, 400)
				return
			}
			if state.objects[proposed.Name] != nil {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(metav1.Status{Reason: metav1.StatusReasonAlreadyExists, Code: 409})
				return
			}
			if state.mode == "429" {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(429)
				_ = json.NewEncoder(w).Encode(metav1.Status{Reason: metav1.StatusReasonTooManyRequests, Code: 429})
				return
			}
			proposed.UID, proposed.ResourceVersion = types.UID("service-"+strconv.Itoa(state.posts)), "1"
			proposed.Spec.ClusterIP = "10.0.0." + strconv.Itoa(state.posts+10)
			state.objects[proposed.Name] = proposed.DeepCopy()
			if state.afterPost != nil {
				state.afterPost()
			}
			if state.mode == "unknown" {
				state.mode = "success"
				creationStatus(w, 504)
				return
			}
			if state.mode == "conflict" {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(metav1.Status{Reason: metav1.StatusReasonAlreadyExists, Code: 409})
				return
			}
			if state.mode == "drift" {
				state.objects[proposed.Name].Spec.Ports[0].TargetPort = intstr.FromInt(5432)
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(&proposed)
			return
		}
		f.serve(w, r)
	}))
	t.Cleanup(api.Close)
	f.kube.base, f.kube.http = api.URL, api.Client()
	return ctx, binding, state, f, source
}

func servicesFact(t *testing.T, ctx context.Context, binding *dsgaCreationBinding) string {
	t.Helper()
	var rows string
	if err := binding.server.db.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(s)||jsonb_build_object('row_xmin',s.xmin::text) ORDER BY service_kind),'[]'::jsonb)::text FROM dsga_creation_services s WHERE operation_id=$1`, binding.operationID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestDSGAServicesOriginalUIDRecoveryAndUnknownWrite(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "conflict", "429", "drift"} {
		t.Run(mode, func(t *testing.T) {
			ctx, binding, api := dsgaServicesFixture(t, mode)
			err := binding.prepareServices(ctx)
			if mode == "unknown" || mode == "429" || mode == "drift" {
				if err == nil {
					t.Fatal("failed/unknown Service write fabricated completion")
				}
				api.mu.Lock()
				posts := api.posts
				api.mu.Unlock()
				if posts != 1 {
					t.Fatal("Service POST transparently retried")
				}
				if mode != "unknown" {
					return
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := binding.prepareServices(ctx); err != nil {
				t.Fatal(err)
			}
			before := servicesFact(t, ctx, binding)
			if before == "[]" {
				t.Fatal("Service identities not bound")
			}
			if err := binding.prepareServices(ctx); err != nil {
				t.Fatal(err)
			}
			if servicesFact(t, ctx, binding) != before {
				t.Fatal("recovery rewrote Service UID/xmin facts")
			}
			api.mu.Lock()
			posts := api.posts
			copy := api.objects[brokerTestWorkload].DeepCopy()
			api.mu.Unlock()
			if posts != 2 {
				t.Fatal("original recovery created extra Services")
			}
			api.mu.Lock()
			delete(api.objects, brokerTestWorkload)
			api.mu.Unlock()
			if err := binding.prepareServices(ctx); err == nil {
				t.Fatal("lost bound Service recreated")
			}
			api.mu.Lock()
			copy.UID = "replacement"
			api.objects[copy.Name] = copy
			api.mu.Unlock()
			if err := binding.prepareServices(ctx); err == nil {
				t.Fatal("same-name replacement Service adopted")
			}
			api.mu.Lock()
			posts = api.posts
			api.mu.Unlock()
			if posts != 2 {
				t.Fatal("bound UID loss caused a POST")
			}
		})
	}
}

func TestDSGAServicesNoNetworkSQLLockAndAtomicBindingFailureRecovery(t *testing.T) {
	ctx, binding, api := dsgaServicesFixture(t, "success")
	// 外部写发生时同一Operation必须可由另一SQL事务更新，证明没有跨HTTP持锁；
	// 回到绑定阶段必须拒绝已经到期的worker，不把实际Service存在当接受成功。
	api.afterPost = func() {
		if _, err := binding.server.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, binding.operationID); err != nil {
			t.Error("lease update blocked or failed")
		}
	}
	if err := binding.prepareServices(ctx); err == nil {
		t.Fatal("expired worker bound Service")
	}
	if servicesFact(t, ctx, binding) != "[]" {
		t.Fatal("expired worker left a Service binding")
	}
	api.mu.Lock()
	api.afterPost = nil
	api.mu.Unlock()
	if _, err := binding.server.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, binding.operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := binding.server.db.Exec(ctx, `CREATE FUNCTION fail_service_binding() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'test bind failure'; END$$; CREATE TRIGGER fail_service_binding BEFORE INSERT ON dsga_creation_services FOR EACH ROW EXECUTE FUNCTION fail_service_binding()`); err != nil {
		t.Fatal(err)
	}
	if err := binding.prepareServices(ctx); err == nil {
		t.Fatal("failed SQL binding reported success")
	}
	if servicesFact(t, ctx, binding) != "[]" {
		t.Fatal("binding failure partially committed")
	}
	if _, err := binding.server.db.Exec(ctx, `DROP TRIGGER fail_service_binding ON dsga_creation_services`); err != nil {
		t.Fatal(err)
	}
	if err := binding.prepareServices(ctx); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	posts := api.posts
	api.mu.Unlock()
	if posts != 2 {
		t.Fatal("SQL recovery recreated the original external Service")
	}
}

func TestDSGAServicesForwardMigrationAndImmutableDatabaseGuard(t *testing.T) {
	ctx, binding, _ := dsgaServicesFixture(t, "success")
	if err := binding.prepareServices(ctx); err != nil {
		t.Fatal(err)
	}
	before := servicesFact(t, ctx, binding)
	for range 2 {
		if err := migrate(ctx, binding.server.db); err != nil {
			t.Fatal(err)
		}
	}
	if servicesFact(t, ctx, binding) != before {
		t.Fatal("forward migration rewrote Service facts")
	}
	for _, query := range []string{`UPDATE dsga_creation_services SET service_uid='replacement' WHERE operation_id=$1`, `UPDATE dsga_creation_services SET created_at=created_at WHERE operation_id=$1`, `DELETE FROM dsga_creation_services WHERE operation_id=$1`} {
		_, err := binding.server.db.Exec(ctx, query, binding.operationID)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatal("database allowed original Service fact mutation")
		}
	}
}

func TestDSGAServicesRejectForeignRouteAndNoRepairWrites(t *testing.T) {
	ctx, binding, api := dsgaServicesFixture(t, "success")
	if err := binding.prepareServices(ctx); err != nil {
		t.Fatal(err)
	}
	before := servicesFact(t, ctx, binding)
	api.mu.Lock()
	original := api.objects[brokerTestWorkload].DeepCopy()
	api.mu.Unlock()
	for name, mutate := range map[string]func(*core.Service){
		"owner":         func(s *core.Service) { s.OwnerReferences[0].UID = "foreign-vm" },
		"operation":     func(s *core.Service) { s.Annotations[dsgaCredentialOperation] = "foreign-operation" },
		"selector":      func(s *core.Service) { s.Spec.Selector["vm.neon.tech/name"] = "foreign-vm" },
		"nodeport":      func(s *core.Service) { s.Spec.Type = core.ServiceTypeNodePort; s.Spec.Ports[0].NodePort = 30000 },
		"port":          func(s *core.Service) { s.Spec.Ports[0].TargetPort = intstr.FromInt(5432) },
		"external_ip":   func(s *core.Service) { s.Spec.ExternalIPs = []string{"192.0.2.1"} },
		"loopback":      func(s *core.Service) { s.Spec.ClusterIP = "127.0.0.1" },
		"early_publish": func(s *core.Service) { s.Spec.PublishNotReadyAddresses = true },
		"affinity":      func(s *core.Service) { s.Spec.SessionAffinity = core.ServiceAffinityClientIP },
		"traffic_local": func(s *core.Service) { v := core.ServiceInternalTrafficPolicyLocal; s.Spec.InternalTrafficPolicy = &v },
	} {
		t.Run(name, func(t *testing.T) {
			bad := original.DeepCopy()
			mutate(bad)
			api.mu.Lock()
			api.objects[bad.Name] = bad
			api.mu.Unlock()
			if err := binding.prepareServices(ctx); err == nil {
				t.Fatal("foreign/unsafe Service accepted")
			}
			api.mu.Lock()
			posts := api.posts
			api.objects[original.Name] = original.DeepCopy()
			api.mu.Unlock()
			if posts != 2 || servicesFact(t, ctx, binding) != before {
				t.Fatal("rejected drift repaired/rebound original Service")
			}
		})
	}
}
