package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	core "k8s.io/api/core/v1"
)

// 独立schema和真实PG行锁，与其余API测试互不篡改；Kube侧仍是TLS协议夹具。
// envelope只是持久绑定的身份view，完整启动预算/生产者/Renderer另行接线。
func dsgaBindingFixture(t *testing.T) (context.Context, *server, string, *creationTestAPI, dsgaCreationInput) {
	t.Helper()
	url := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	if !regexp.MustCompile(`^v2_migration_[a-z0-9_]+$`).MatchString(os.Getenv("NEON_V2_TEST_SCHEMA")) {
		t.Fatal("a dedicated v2_migration_* schema is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	schema := "v2_migration_dsga_" + stableSuffix("test", "binding", newID("schema_"))
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 5
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["application_name"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f, input := newCreationTest(t)
	p := input.Payload
	p.BranchID, p.TenantID, p.TimelineID = "br_test", strings.Repeat("a", 32), strings.Repeat("b", 32)
	p.EndpointType = "read_write"
	input.Payload = p
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 与生产创建入口一样显式提供资源时间；001迁移故意没有替这些字段设默认值。
	// 测试适配真实schema，不能为了夹具放宽生产约束或改写已应用的迁移。
	exec(`INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at)
 VALUES($1,'local',$1,$2,'test',17,'ready','managed',clock_timestamp(),clock_timestamp())`, p.ProjectID, p.TenantID)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at)
 VALUES($1,$2,'main',$3,'ready',clock_timestamp())`, p.BranchID, p.ProjectID, p.TimelineID)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at)
 VALUES($1,$2,$3,$4,'neonvm',$5,$5,'provisioning','app_owner','postgres',clock_timestamp(),clock_timestamp())`, p.EndpointID, p.ProjectID, p.BranchID, selector(p.EndpointID), kubeName(p.EndpointID))
	var envelope dsgaCreationEnvelope
	envelope.createPayload = p
	envelope.Creation.ProtocolVersion, envelope.Creation.PostgresVersion = 1, 17
	envelope.Creation.Namespace, envelope.Creation.Incarnation, envelope.Creation.ComputeInstanceID = f.kube.namespace, input.VM.Spec.DSGABootstrap.Incarnation, input.VM.Spec.DSGABootstrap.ComputeInstanceID
	// 不解释的审计计数特意大于float64精确范围；绑定不能重写或损失任何payload数值。
	payload, err := json.Marshal(struct {
		dsgaCreationEnvelope
		AuditCounter uint64 `json:"audit_counter"`
	}{envelope, ^uint64(0)})
	if err != nil {
		t.Fatal(err)
	}
	op := newID("op_dsga_")
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at)
 VALUES($1,$2,'endpoint',$3,'create_endpoint_dsga_v1','running','test','test',$4,'worker_a',clock_timestamp()+interval '60 seconds')`, op, p.ProjectID, p.EndpointID, payload)
	return ctx, &server{db: pool, kube: f.kube}, op, f, input
}

func TestDSGACreationRealMetadataBindingAndImmutableIntent(t *testing.T) {
	ctx, s, op, f, input := dsgaBindingFixture(t)
	var original string
	if err := s.db.QueryRow(ctx, `SELECT payload::text FROM operations WHERE id=$1`, op).Scan(&original); err != nil {
		t.Fatal(err)
	}
	binding, err := s.loadDSGACreationBinding(ctx, op, "worker_a")
	if err != nil {
		t.Fatal(err)
	}
	// 原测试回调仅记录先后顺序；每个成功之前先执行真实元数据库持久CAS。
	traceVM, traceConfig := input.Bind, input.BindConfig
	input.Bind = func(ctx context.Context, vm *vmv1.VirtualMachine) error {
		if err := binding.bindVM(ctx, vm); err != nil {
			return err
		}
		return traceVM(ctx, vm)
	}
	input.BindConfig = func(ctx context.Context, secret *core.Secret) error {
		if err := binding.bindConfig(ctx, secret); err != nil {
			return err
		}
		return traceConfig(ctx, secret)
	}
	vm, err := s.kube.prepareDSGACreation(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.loadDSGACreationBinding(ctx, op, "worker_a")
	if err != nil || loaded.vmUID != string(vm.UID) || loaded.configUID != "config-uid" {
		t.Fatalf("actual UID facts were not persisted: %v", err)
	}
	var xminBefore, xminAfter, after string
	if err := s.db.QueryRow(ctx, `SELECT xmin::text FROM dsga_creation_bindings WHERE operation_id=$1`, op).Scan(&xminBefore); err != nil {
		t.Fatal(err)
	}
	if err := binding.bindVM(ctx, vm); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	secret := f.secret.DeepCopy()
	f.mu.Unlock()
	if err := binding.bindConfig(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(ctx, `SELECT xmin::text FROM dsga_creation_bindings WHERE operation_id=$1`, op).Scan(&xminAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(ctx, `SELECT payload::text FROM operations WHERE id=$1`, op).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if xminBefore != xminAfter || original != after || !strings.Contains(after, "18446744073709551615") {
		t.Fatal("idempotent binding rewrote facts or changed the original uint64 intent")
	}
	for _, query := range []string{
		`UPDATE operations SET payload=jsonb_set(payload,'{audit_counter}','1') WHERE id=$1`,
		`UPDATE operations SET action='create_endpoint' WHERE id=$1`,
		`UPDATE operations SET resource_id='foreign' WHERE id=$1`,
		`UPDATE dsga_creation_bindings SET vm_uid='new-vm' WHERE operation_id=$1`,
		`UPDATE dsga_creation_bindings SET config_uid='new-config' WHERE operation_id=$1`,
		`UPDATE dsga_creation_bindings SET config_uid=NULL WHERE operation_id=$1`,
		`UPDATE dsga_creation_bindings SET intent_digest=decode(repeat('00',32),'hex') WHERE operation_id=$1`,
		`DELETE FROM dsga_creation_bindings WHERE operation_id=$1`,
	} {
		_, err := s.db.Exec(ctx, query, op)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("immutable creation fact mutation not rejected by DB: %v", err)
		}
	}
	otherVM := vm.DeepCopy()
	otherVM.UID, otherVM.Spec.DSGABootstrap.VMUID = "new-vm", "new-vm"
	if err := binding.bindVM(ctx, otherVM); !errors.Is(err, errDSGACreationIdentity) {
		t.Fatalf("VM UID replaced: %v", err)
	}
	secret.UID = "new-config"
	if err := binding.bindConfig(ctx, secret); !errors.Is(err, errDSGACreationIdentity) {
		t.Fatalf("config UID replaced: %v", err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE operations SET lease_owner='worker_b' WHERE id=$1`, op); err != nil {
		t.Fatal(err)
	}
	if err := binding.bindVM(ctx, vm); !errors.Is(err, errLeaseLost) {
		t.Fatalf("cached original worker bypassed current lease: %v", err)
	}
}

func TestDSGACreationBindingLeaseExpiresWhileWaitingForRowLock(t *testing.T) {
	ctx, s, op, _, input := dsgaBindingFixture(t)
	binding, err := s.loadDSGACreationBinding(ctx, op, "worker_a")
	if err != nil {
		t.Fatal(err)
	}
	vm := input.VM.DeepCopy()
	vm.UID, vm.ResourceVersion = "vm-uid", "version-1"
	if _, err := s.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, op); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM operations WHERE id=$1 FOR UPDATE`, op).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- binding.bindVM(ctx, vm) }()
	// 观察真实Lock等待再等数据库时钟过期，不用固定sleep猜测并发交错。
	deadline := time.Now().Add(8 * time.Second)
	for {
		var waiting bool
		err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE application_name=current_setting('application_name') AND wait_event_type='Lock'
 AND query LIKE '%FROM operations WHERE id=$1 FOR UPDATE%')`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("binding did not reach the expected actual row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		var expired bool
		if err := s.db.QueryRow(ctx, `SELECT lease_expires_at<=clock_timestamp() FROM operations WHERE id=$1`, op).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errLeaseLost) {
		t.Fatalf("lease expired during real lock wait was accepted: %v", err)
	}
	var count int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM dsga_creation_bindings WHERE operation_id=$1`, op).Scan(&count); err != nil || count != 0 {
		t.Fatal(fmt.Sprintf("expired worker persisted a binding: count=%d err=%v", count, err))
	}
}

// 新action既要保持旧生命周期约束，也要阻止旧worker忽略新协议。每个case在真实
// 事务内终结原active操作以排除唯一索引的干扰，探针接受后回滚；因此失败必须来自
// 正确的23514协议守卫，而不是重复操作/外键等无关约束。
func TestDSGACreationMetadataAdmissionPreservesProtocolAndTTL(t *testing.T) {
	ctx, s, op, _, _ := dsgaBindingFixture(t)
	cases := []struct {
		name, action, payload, prepare string
		accepted                       bool
	}{
		{name: "versioned_endpoint", action: "create_endpoint_dsga_v1", payload: "payload", accepted: true},
		{name: "legacy_with_creation", action: "create_endpoint", payload: "payload"},
		{name: "legacy_with_null_creation", action: "create_endpoint", payload: "jsonb_set(payload,'{dsga_creation}','null')"},
		{name: "unsupported_protocol", action: "create_endpoint_dsga_v1", payload: "jsonb_set(payload,'{dsga_creation,protocol_version}','2')"},
		{name: "protocol_string", action: "create_endpoint_dsga_v1", payload: `jsonb_set(payload,'{dsga_creation,protocol_version}','"1"')`},
		{name: "unsupported_pg", action: "create_endpoint_dsga_v1", payload: "jsonb_set(payload,'{dsga_creation,postgres_version}','16')"},
		{name: "different_project", action: "create_endpoint_dsga_v1", payload: `jsonb_set(payload,'{project_id}','"foreign"')`},
		{name: "project_pg16", action: "create_endpoint_dsga_v1", payload: "payload", prepare: "UPDATE projects SET postgres_version=16 WHERE id=(SELECT project_id FROM operations WHERE id=$1)"},
		{name: "future_expiration", action: "create_endpoint_dsga_v1", payload: "payload", prepare: "UPDATE branches SET expires_at=clock_timestamp()+interval '1 hour',ttl_interval_seconds=3600 WHERE id=(SELECT payload->>'branch_id' FROM operations WHERE id=$1)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := s.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err = tx.Exec(ctx, `UPDATE operations SET state='failed',lease_owner=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, op); err != nil {
				t.Fatal(err)
			}
			if c.prepare != "" {
				if _, err = tx.Exec(ctx, c.prepare, op); err != nil {
					t.Fatal(err)
				}
			}
			// SQL片段均为上方源码常量，运行时身份仍通过参数绑定。
			query := `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
 SELECT 'op_dsga_probe',project_id,resource_type,resource_id,$2,'queued','test','test',` + c.payload + ` FROM operations WHERE id=$1`
			_, err = tx.Exec(ctx, query, op, c.action)
			if c.accepted {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("unsafe creation admission accepted or rejected for unrelated reason: %v", err)
			}
		})
	}
}
