package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func credentialSourceSecret() *core.Secret {
	c := dsgaRenderFixture()
	immutable := true
	return &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "neon", Name: brokerTestWorkload + "-credentials", UID: "original-credentials", ResourceVersion: "1", Labels: credentialLabels("prj_test", "ep_0123456789abcdef")},
		Immutable: &immutable, Type: core.SecretTypeOpaque, Data: map[string][]byte{
			"applicationVerifier": []byte(c.ApplicationVerifier), "probeVerifier": []byte(c.ProbeVerifier), "controlSigningSeed": bytes.Repeat([]byte{4}, 32),
		}}
}

func TestDSGACredentialSourceSnapshotIsolationAndUnsafeInputs(t *testing.T) {
	original := credentialSourceSecret()
	snapshot, err := snapshotDSGACredentials(original, "neon", original.Name, "prj_test", "ep_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	// 只改RV不改变凭据事实；顺序不同的map仍有相同规范摘要。
	other := original.DeepCopy()
	other.ResourceVersion = "2"
	again, err := snapshotDSGACredentials(other, "neon", original.Name, "prj_test", "ep_0123456789abcdef")
	if err != nil || snapshot.digest != again.digest {
		t.Fatal("metadata RV changed immutable content identity")
	}
	previous := append([]byte(nil), snapshot.credentials.ControlPublicKey...)
	original.Data["controlSigningSeed"][0] ^= 1
	original.Data["applicationVerifier"][0] = 'X'
	if !bytes.Equal(previous, snapshot.credentials.ControlPublicKey) || snapshot.credentials.ApplicationVerifier != dsgaRenderFixture().ApplicationVerifier {
		t.Fatal("snapshot retained mutable source pointers")
	}
	for name, mutate := range map[string]func(*core.Secret){
		"namespace": func(s *core.Secret) { s.Namespace = "foreign" },
		"owner":     func(s *core.Secret) { s.Labels["neon-control/project-id"] = "foreign" },
		"uid":       func(s *core.Secret) { s.UID = "" },
		"rv":        func(s *core.Secret) { s.ResourceVersion = "" },
		"mutable":   func(s *core.Secret) { s.Immutable = nil },
		"deleting":  func(s *core.Secret) { v := metav1.Now(); s.DeletionTimestamp = &v },
		"legacy_admin": func(s *core.Secret) {
			s.Data["adminVerifier"] = s.Data["applicationVerifier"]
			delete(s.Data, "applicationVerifier")
		},
		"probe_md5":     func(s *core.Secret) { s.Data["probeVerifier"] = []byte("md5old") },
		"short_seed":    func(s *core.Secret) { s.Data["controlSigningSeed"] = []byte{1} },
		"extra_private": func(s *core.Secret) { s.Data["password"] = []byte("unregistered") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := credentialSourceSecret()
			mutate(candidate)
			out, err := snapshotDSGACredentials(candidate, "neon", brokerTestWorkload+"-credentials", "prj_test", "ep_0123456789abcdef")
			if !errors.Is(err, errDSGACredentialSource) || out.uid != "" || out.credentials.ApplicationVerifier != "" {
				t.Fatal("unsafe source returned usable credential material")
			}
		})
	}
}

// 使用真实独占PG/schema，Kube部分仅TLS API夹具。新Operation由生产数据库入口
// 接受完整计划，不改旧Operation payload；真实Render回调读取typed Secret并持久
// 来源，真实Bind回调固定VM/Config UID。所有敏感失败信息只报告错误类别。
func credentialSourceFixture(t *testing.T, legacy ...bool) (context.Context, *server, string, *creationTestAPI, dsgaCreationInput, *dsgaCreationBinding, **core.Secret) {
	t.Helper()
	ctx, s, old, f, input := dsgaBindingFixture(t)
	p, policy, _ := dsgaPlanFixture(t)
	p.ProjectID, p.BranchID, p.EndpointID, p.TenantID, p.TimelineID = input.Payload.ProjectID, input.Payload.BranchID, input.Payload.EndpointID, input.Payload.TenantID, input.Payload.TimelineID
	policy.Namespace = f.kube.namespace
	policy.VM = dsgaVMProfileFixture()
	plan, err := newDSGACreationPlan(p, policy, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	input.Payload = p
	input.VM.Spec.DSGABootstrap.Incarnation, input.VM.Spec.DSGABootstrap.ComputeInstanceID = plan.Incarnation, plan.ComputeInstanceID
	op := newID("op_source_")
	var source *core.Secret
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentialsRequest := r.URL.Path == f.kube.path("secret", brokerTestWorkload+"-credentials")
		if r.Method == http.MethodPost && r.URL.Path == f.kube.path("secret", "") {
			// credentials与config共享同一API collection；按实际请求对象分流，
			// 原字节交回Config夹具，不能把第二个Secret误判为凭据重复创建。
			raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			var proposed core.Secret
			if err != nil || json.Unmarshal(raw, &proposed) != nil {
				w.WriteHeader(400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			credentialsRequest = proposed.Name == brokerTestWorkload+"-credentials"
		}
		if credentialsRequest {
			if r.Header.Get("Authorization") != "Bearer creation-kube-identity" {
				t.Error("typed credentials did not use center identity")
				w.WriteHeader(403)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				if source != nil {
					w.WriteHeader(409)
					return
				}
				var proposed core.Secret
				if json.NewDecoder(r.Body).Decode(&proposed) != nil {
					w.WriteHeader(400)
					return
				}
				proposed.UID, proposed.ResourceVersion = "original-credentials", "1"
				source = &proposed
				w.WriteHeader(201)
			} else if r.Method != http.MethodGet {
				w.WriteHeader(405)
				return
			}
			if source == nil {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(source)
			return
		}
		f.serve(w, r)
	}))
	t.Cleanup(api.Close)
	f.kube.base, f.kube.http = api.URL, api.Client()
	// 原Operation接受前通过真实typed生产者写入并读回immutable来源，而非手填
	// UID/摘要。其后元数据库、Renderer和held→ready继续使用真实生产代码。
	plan, err = f.kube.prepareDSGACredentials(ctx, p, plan, op, dsgaCredentialInput{Password: "credential-fixture-password"})
	if err != nil {
		t.Fatal(err)
	}
	input.VM, err = renderDSGAVM(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) > 0 && legacy[0] {
		plan.Credentials = dsgaCredentialReference{}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE operations SET state='failed',lease_owner=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, old); err != nil {
		t.Fatal(err)
	}
	if err := addDSGACreateOperation(ctx, tx, "create_endpoint", "endpoint", p.EndpointID, p.ProjectID, op, "test", "source-test", p, plan, "source-key", strings.Repeat("e", 64), createSteps("create_endpoint", true)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE operations SET state='running',lease_owner='worker_a',lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, op); err != nil {
		t.Fatal(err)
	}
	binding, err := s.loadDSGACreationBinding(ctx, op, "worker_a")
	if err != nil {
		t.Fatal(err)
	}
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
	input.Render = binding.renderDSGABootstrap
	return ctx, s, op, f, input, binding, &source
}

func TestDSGACredentialSourceFrozenBeforeFirstWorkerRead(t *testing.T) {
	ctx, s, op, f, input, binding, source := credentialSourceFixture(t)
	vm := input.VM.DeepCopy()
	vm.UID, vm.ResourceVersion = "vm-uid", "1"
	if err := binding.bindVM(ctx, vm); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	original := (*source).DeepCopy()
	f.mu.Unlock()
	for name, mutate := range map[string]func(*core.Secret){
		"uid":         func(v *core.Secret) { v.UID = "recreated-source" },
		"seed":        func(v *core.Secret) { v.Data["controlSigningSeed"][0] ^= 1 },
		"operation":   func(v *core.Secret) { v.Annotations[dsgaCredentialOperation] = "other-operation" },
		"incarnation": func(v *core.Secret) { v.Annotations[dsgaCredentialIncarnation] = "other-incarnation" },
		"compute":     func(v *core.Secret) { v.Annotations[dsgaCredentialCompute] = "other-compute" },
	} {
		t.Run(name, func(t *testing.T) {
			f.mu.Lock()
			*source = original.DeepCopy()
			mutate(*source)
			f.mu.Unlock()
			if data, err := binding.renderDSGABootstrap(ctx, vm); !errors.Is(err, errDSGACredentialSource) || data != nil {
				t.Fatal("first worker adopted changed accepted source")
			}
			var count int
			if err := s.db.QueryRow(ctx, `SELECT count(*) FROM dsga_creation_credential_sources WHERE operation_id=$1`, op).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejected source changed persisted facts")
			}
		})
	}
	// 历史无ref计划可以读取，但必须阻止自动启动，不从同名Secret补写原意图。
	t.Run("legacy_missing_ref", func(t *testing.T) {
		ctx, _, _, _, input, binding, _ := credentialSourceFixture(t, true)
		vm := input.VM.DeepCopy()
		vm.UID, vm.ResourceVersion = "vm-uid", "1"
		if err := binding.bindVM(ctx, vm); err != nil {
			t.Fatal(err)
		}
		if data, err := binding.renderDSGABootstrap(ctx, vm); !errors.Is(err, errDSGACredentialSource) || data != nil {
			t.Fatal("legacy missing ref was silently adopted")
		}
	})
}

func sourceFact(t *testing.T, ctx context.Context, s *server, op string) (string, string) {
	t.Helper()
	var row, xmin string
	if err := s.db.QueryRow(ctx, `SELECT row_to_json(s)::text,xmin::text FROM dsga_creation_credential_sources s WHERE operation_id=$1`, op).Scan(&row, &xmin); err != nil {
		t.Fatal(err)
	}
	return row, xmin
}

func TestDSGACredentialSourceActualCreationRecoveryAndDatabaseGuards(t *testing.T) {
	ctx, s, op, f, input, binding, source := credentialSourceFixture(t)
	f.lost = "initialize"
	if vm, err := f.kube.prepareDSGACreation(ctx, input); err == nil || vm != nil {
		t.Fatal("lost journal reply fabricated success")
	}
	var originalBinding, bindingXmin string
	if err := s.db.QueryRow(ctx, `SELECT row_to_json(b)::text,xmin::text FROM dsga_creation_bindings b WHERE operation_id=$1`, op).Scan(&originalBinding, &bindingXmin); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	input.ExpectedVMUID = f.vm.UID
	f.mu.Unlock()
	// 模拟旧已登记059库升级：只移除本次新DDL/登记，再追加迁移两次，旧绑定
	// 的行/xmin必须保留。真实新库已由fixture的migrate完整验证。
	if _, err := s.db.Exec(ctx, `DROP TABLE dsga_creation_credential_sources; DROP FUNCTION guard_dsga_creation_credential_source(); DELETE FROM schema_migrations WHERE version=900000060`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	var preservedBinding, preservedXmin string
	if err := s.db.QueryRow(ctx, `SELECT row_to_json(b)::text,xmin::text FROM dsga_creation_bindings b WHERE operation_id=$1`, op).Scan(&preservedBinding, &preservedXmin); err != nil || originalBinding != preservedBinding || bindingXmin != preservedXmin {
		t.Fatal("source forward migration rewrote prior binding")
	}
	vm, err := f.kube.prepareDSGACreation(ctx, input)
	if err != nil || vm == nil || !vm.Spec.DSGABootstrap.Ready {
		t.Fatalf("actual source-bound creation failed: %v", err)
	}
	before, xmin := sourceFact(t, ctx, s, op)
	f.mu.Lock()
	config := append([]byte(nil), f.secret.Data["config.json"]...)
	f.mu.Unlock()
	if bytes.Contains(config, []byte(base64.StdEncoding.EncodeToString((*source).Data["controlSigningSeed"]))) {
		t.Fatal("signing seed entered Guest config")
	}
	if strings.Contains(before, "SCRAM") || strings.Contains(before, "controlSigningSeed") {
		t.Fatal("source row contains private credential fields")
	}
	loaded, err := s.loadDSGACreationBinding(ctx, op, "worker_a")
	if err != nil {
		t.Fatal(err)
	}
	input.ExpectedVMUID, input.ExpectedConfigUID = vm.UID, "config-uid"
	input.Render = loaded.renderDSGABootstrap
	if _, err := f.kube.prepareDSGACreation(ctx, input); err != nil {
		t.Fatal(err)
	}
	after, afterXmin := sourceFact(t, ctx, s, op)
	if before != after || xmin != afterXmin {
		t.Fatal("same original source recovery rewrote facts")
	}
	for _, query := range []string{`UPDATE dsga_creation_credential_sources SET credentials_uid='replacement' WHERE operation_id=$1`, `UPDATE dsga_creation_credential_sources SET content_digest=decode(repeat('00',32),'hex') WHERE operation_id=$1`, `UPDATE dsga_creation_credential_sources SET source_version=source_version WHERE operation_id=$1`, `DELETE FROM dsga_creation_credential_sources WHERE operation_id=$1`} {
		_, err := s.db.Exec(ctx, query, op)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatal("DB allowed replacement/delete/no-op UPDATE of source")
		}
	}
	// 已有来源不依赖缓存字段，恢复时也核对当前lease。
	if _, err := s.db.Exec(ctx, `UPDATE operations SET lease_owner='worker_b' WHERE id=$1`, op); err != nil {
		t.Fatal(err)
	}
	if data, err := binding.renderDSGABootstrap(ctx, vm); !errors.Is(err, errLeaseLost) || data != nil {
		t.Fatal("stale source renderer bypassed current owner")
	}
}

func TestDSGACredentialSourceLostConfigRejectsRecreatedOrChangedSource(t *testing.T) {
	for _, scenario := range []string{"uid", "same_uid_seed", "same_uid_verifier", "mutable"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, s, op, f, input, binding, source := credentialSourceFixture(t)
			f.lost = "config"
			if vm, err := f.kube.prepareDSGACreation(ctx, input); err == nil || vm != nil {
				t.Fatal("lost config reply fabricated success")
			}
			before, xmin := sourceFact(t, ctx, s, op)
			f.mu.Lock()
			original := (*source).DeepCopy()
			switch scenario {
			case "uid":
				(*source).UID = "replacement-credentials"
			case "same_uid_seed":
				(*source).Data["controlSigningSeed"][0] ^= 1
			case "same_uid_verifier":
				(*source).Data["applicationVerifier"] = append([]byte(nil), (*source).Data["probeVerifier"]...)
				(*source).Data["applicationVerifier"][len((*source).Data["applicationVerifier"])-5] = 'A'
			case "mutable":
				(*source).Immutable = nil
			}
			input.ExpectedVMUID = f.vm.UID
			currentVM, changedSource := f.vm.DeepCopy(), (*source).DeepCopy()
			f.mu.Unlock()
			if strings.HasPrefix(scenario, "same_uid_") {
				// 必须是格式合法的新凭据，且来源回调本身拒绝；不能让后续旧配置
				// 字节比较替本摘要门禁兜底，掩盖来源检查被意外删除的问题。
				if _, err := snapshotDSGACredentials(changedSource, "neon", changedSource.Name, input.Payload.ProjectID, input.Payload.EndpointID); err != nil {
					t.Fatal("digest drift case was malformed instead of a valid changed credential")
				}
				if data, err := binding.renderDSGABootstrap(ctx, currentVM); !errors.Is(err, errDSGACredentialSource) || data != nil {
					t.Fatal("source renderer itself accepted changed content under the original UID")
				}
			}
			if vm, err := f.kube.prepareDSGACreation(ctx, input); err == nil || vm != nil {
				t.Fatal("source drift produced ready creation")
			}
			after, afterXmin := sourceFact(t, ctx, s, op)
			f.mu.Lock()
			ready := f.vm.Spec.DSGABootstrap.Ready
			*source = original
			f.mu.Unlock()
			if ready || before != after || xmin != afterXmin {
				t.Fatal("failed source drift changed facts or released held VM")
			}
			loaded, err := s.loadDSGACreationBinding(ctx, op, "worker_a")
			if err != nil {
				t.Fatal(err)
			}
			input.Render = loaded.renderDSGABootstrap
			if _, err := f.kube.prepareDSGACreation(ctx, input); err != nil {
				t.Fatal("original source could not resume", err)
			}
		})
	}
}

func TestDSGACredentialSourceLeaseExpiresWhileWaitingForOperationLock(t *testing.T) {
	ctx, s, op, _, input, binding, _ := credentialSourceFixture(t)
	// 固定夹具VM UID但不配置；这里只证明新来源的真实数据库锁竞争，不声称
	// 这份手工对象经过K8s创建。实际typed创建另由上面主/恢复用例验证。
	vm := input.VM.DeepCopy()
	vm.UID, vm.ResourceVersion = "vm-uid", "1"
	if err := binding.bindVM(ctx, vm); err != nil {
		t.Fatal(err)
	}
	locker, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(ctx) }()
	if _, err := locker.Exec(ctx, `SELECT id FROM operations WHERE id=$1 FOR UPDATE`, op); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := binding.renderDSGABootstrap(ctx, vm); finished <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%SELECT action,project_id,resource_type,resource_id,payload%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new source operation lock wait was not observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := locker.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op); err != nil {
		t.Fatal(err)
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, errLeaseLost) {
		t.Fatalf("expired waiting worker accepted source: %v", err)
	}
	var count int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM dsga_creation_credential_sources WHERE operation_id=$1`, op).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired worker wrote source facts")
	}
}
