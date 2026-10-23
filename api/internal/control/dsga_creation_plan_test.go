package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func dsgaPlanFixture(t *testing.T) (createPayload, dsgaCreationPolicy, dsgaCreationPlan) {
	t.Helper()
	p := createPayload{ProjectID: "prj_test", BranchID: "br_test", EndpointID: "ep_test", EndpointType: "read_write", TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("b", 32), MinCPU: 500, MaxCPU: 2000, MinMem: 1024, MaxMem: 4096}
	policy := dsgaCreationPolicy{Storage: dsgaStorageProfileFixture(), Namespace: "neon", Image: "ghcr.io/william-lbn/neon-compute@sha256:" + strings.Repeat("a", 64), MemorySlotBytes: 1 << 30, Budget: dsgaBootstrapBudget{64 << 20, 2 << 30, 1 << 30, 512 << 20}}
	plan, err := makeDSGACreationPlan(p, policy, time.Date(2026, 10, 21, 12, 0, 0, 1234, time.UTC), bytes.NewReader(bytes.Repeat([]byte{0x41}, 48)))
	if err != nil {
		t.Fatal(err)
	}
	return p, policy, plan
}

func TestDSGACreationPlanFreezesIndependentIdentityAndNativeBytes(t *testing.T) {
	p, policy, plan := dsgaPlanFixture(t)
	if plan.OperationUUID != "41414141-4141-4141-8141-414141414141" || plan.AcceptedAt != "2026-10-21T12:00:00.000001234Z" {
		t.Fatal("original UUID/time not canonical")
	}
	other, err := newDSGACreationPlan(p, policy, time.Now())
	if err != nil || other.Incarnation == plan.Incarnation || other.ComputeInstanceID == plan.ComputeInstanceID || other.OperationUUID == plan.OperationUUID {
		t.Fatal("new original intent reused an identity")
	}
	frozen := plan
	policy.Image = "changed:latest"
	policy.Budget.BootstrapBufferBytes = 0
	if plan != frozen {
		t.Fatal("later policy changes rewrote original value")
	}
	for _, mode := range []string{"read_write", "read_only"} {
		p.EndpointType = mode
		native, err := plan.nativeBootstrap(p, "actual-vm-uid")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(native)
		if err != nil {
			t.Fatal(err)
		}
		// 完整原生启动契约，不夹带创建镜像/时间等只属于控制面的字段。
		expected := `{"protocol_version":2,"vm_uid":"actual-vm-uid","compute_instance_id":"ci_41414141414141414141414141414141","bootstrap_buffer_bytes":67108864,"maximum_buffer_bytes":2147483648,"minimum_guest_memory_bytes":1073741824,"non_buffer_reserve_bytes":536870912}`
		if string(raw) != expected {
			t.Fatalf("native contract bytes differ: %s", raw)
		}
	}
	if _, err := plan.nativeBootstrap(p, ""); !errors.Is(err, errDSGACreationPlan) {
		t.Fatal("native bootstrap accepted absent actual VM UID")
	}
	// serde/Rust也必须保持大整数；这里只验证编码，不声称超大Guest可在NeonVM运行。
	native := dsgaNativeBootstrap{2, "actual-vm-uid", plan.ComputeInstanceID, dsgaBootstrapBudget{1 << 20, 2 << 20, ^uint64(0), ^uint64(0) - (1 << 20)}}
	raw, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	var decoded dsgaNativeBootstrap
	if err = json.Unmarshal(raw, &decoded); err != nil || decoded != native || !bytes.Contains(raw, []byte("18446744073709551615")) {
		t.Fatal("native uint64 precision lost")
	}
}

func TestDSGACreationPlanRejectsUnsafeOrUnrepresentableIntent(t *testing.T) {
	p, _, plan := dsgaPlanFixture(t)
	cases := map[string]func(*dsgaCreationPlan, *createPayload){
		"protocol":            func(v *dsgaCreationPlan, _ *createPayload) { v.ProtocolVersion = 2 },
		"pg16":                func(v *dsgaCreationPlan, _ *createPayload) { v.PostgresVersion = 16 },
		"namespace":           func(v *dsgaCreationPlan, _ *createPayload) { v.Namespace = "Other/namespace" },
		"namespace_length":    func(v *dsgaCreationPlan, _ *createPayload) { v.Namespace = strings.Repeat("a", 64) },
		"zero_incarnation":    func(v *dsgaCreationPlan, _ *createPayload) { v.Incarnation = strings.Repeat("0", 32) },
		"missing_compute":     func(v *dsgaCreationPlan, _ *createPayload) { v.ComputeInstanceID = "" },
		"mutable_image":       func(v *dsgaCreationPlan, _ *createPayload) { v.Image = "ghcr.io/william-lbn/neon-compute:latest" },
		"short_digest":        func(v *dsgaCreationPlan, _ *createPayload) { v.Image = "ghcr.io/william-lbn/neon@sha256:abcd" },
		"uuid_version":        func(v *dsgaCreationPlan, _ *createPayload) { v.OperationUUID = "41414141-4141-5141-8141-414141414141" },
		"non_utc_time":        func(v *dsgaCreationPlan, _ *createPayload) { v.AcceptedAt = "2026-10-21T20:00:00+08:00" },
		"zero_time":           func(v *dsgaCreationPlan, _ *createPayload) { v.AcceptedAt = "0001-01-01T00:00:00Z" },
		"missing_scope":       func(_ *dsgaCreationPlan, p *createPayload) { p.ProjectID = "" },
		"bad_tenant":          func(_ *dsgaCreationPlan, p *createPayload) { p.TenantID = "tenant" },
		"bad_timeline":        func(_ *dsgaCreationPlan, p *createPayload) { p.TimelineID = "timeline" },
		"missing_mode":        func(_ *dsgaCreationPlan, p *createPayload) { p.EndpointType = "" },
		"unknown_mode":        func(_ *dsgaCreationPlan, p *createPayload) { p.EndpointType = "standby" },
		"negative_cpu":        func(_ *dsgaCreationPlan, p *createPayload) { p.MinCPU = -1 },
		"fractional_cpu_step": func(_ *dsgaCreationPlan, p *createPayload) { p.MinCPU = 251 },
		"cpu_order":           func(_ *dsgaCreationPlan, p *createPayload) { p.MaxCPU = 250 },
		// 仍为合法250m步长，只违反uint32上界，不能由另一项约束替代覆盖。
		"cpu_overflow":             func(_ *dsgaCreationPlan, p *createPayload) { p.MaxCPU = 4_294_967_500 },
		"memory_order":             func(_ *dsgaCreationPlan, p *createPayload) { p.MaxMem = 512 },
		"memory_overflow":          func(_ *dsgaCreationPlan, p *createPayload) { p.MaxMem = int(^uint64(0) >> 1) },
		"memory_rounding":          func(_ *dsgaCreationPlan, p *createPayload) { p.MinMem = 1025 },
		"slot_zero":                func(v *dsgaCreationPlan, _ *createPayload) { v.MemorySlotBytes = 0 },
		"virtio_block":             func(v *dsgaCreationPlan, _ *createPayload) { v.MemorySlotBytes = (1 << 30) + 4096 },
		"slot_count":               func(v *dsgaCreationPlan, p *createPayload) { v.MemorySlotBytes = 8 << 20; p.MaxMem = (1 << 31) * 8 },
		"future_floor_above_start": func(v *dsgaCreationPlan, _ *createPayload) { v.Budget.MinimumGuestMemoryBytes = 2 << 30 },
		"floor_not_slot":           func(v *dsgaCreationPlan, _ *createPayload) { v.Budget.MinimumGuestMemoryBytes = (1 << 30) - 1 },
		"bootstrap_not_block":      func(v *dsgaCreationPlan, _ *createPayload) { v.Budget.BootstrapBufferBytes++ },
		"pg_minimum":               func(v *dsgaCreationPlan, _ *createPayload) { v.Budget.BootstrapBufferBytes = 15 * 8192 },
		"bootstrap_over_max":       func(v *dsgaCreationPlan, _ *createPayload) { v.Budget.MaximumBufferBytes = 32 << 20 },
		"max_overflow":             func(v *dsgaCreationPlan, _ *createPayload) { v.Budget.MaximumBufferBytes = uint64(1<<31) / 2 * 8192 },
		"reserve_missing":          func(v *dsgaCreationPlan, _ *createPayload) { v.Budget.NonBufferReserveBytes = 0 },
		"reserve_underflow":        func(v *dsgaCreationPlan, _ *createPayload) { v.Budget.NonBufferReserveBytes = ^uint64(0) },
		"budget_round_down": func(v *dsgaCreationPlan, _ *createPayload) {
			v.Budget.NonBufferReserveBytes = v.Budget.MinimumGuestMemoryBytes - v.Budget.BootstrapBufferBytes + 1
		},
		"self_ancestor":        func(_ *dsgaCreationPlan, p *createPayload) { p.ParentTimelineID = p.TimelineID },
		"lsn_without_ancestor": func(_ *dsgaCreationPlan, p *createPayload) { p.ParentLSN = "0/1234" },
		"bad_lsn": func(_ *dsgaCreationPlan, p *createPayload) {
			p.ParentTimelineID = strings.Repeat("c", 32)
			p.ParentLSN = "not-an-lsn"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v, q := plan, p
			mutate(&v, &q)
			if !errors.Is(v.validate(q), errDSGACreationPlan) {
				t.Fatal("unsafe or unrepresentable plan was accepted")
			}
		})
	}
	for name, reader := range map[string]io.Reader{"short_random": bytes.NewReader([]byte{1}), "zero_incarnation": bytes.NewReader(make([]byte, 48))} {
		t.Run(name, func(t *testing.T) {
			_, policy, _ := dsgaPlanFixture(t)
			v, err := makeDSGACreationPlan(p, policy, time.Now(), reader)
			if !errors.Is(err, errDSGACreationPlan) || v != (dsgaCreationPlan{}) {
				t.Fatal("failed creation returned a usable partial plan")
			}
		})
	}
	// 固定M可以大于当前可用RAM，真正可提交H仍受预算与VM闭环限制；不是按比例裁剪。
	plan.Budget.MaximumBufferBytes = uint64((1<<31-1)/2) * 8192
	p.ParentTimelineID, p.ParentLSN = strings.Repeat("c", 32), "0/1234"
	if err := plan.validate(p); err != nil {
		t.Fatal(err)
	}
}

func TestDSGACreationPlanPersistsVersionedOperationAndRecoversOriginal(t *testing.T) {
	ctx, s, old, _, input := dsgaBindingFixture(t)
	oldBinding, err := s.loadDSGACreationBinding(ctx, old, "worker_a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldBinding.creationPlan(ctx); !errors.Is(err, errDSGACreationPlan) {
		t.Fatal("identity-only view accepted as a complete frozen plan")
	}
	p, policy, _ := dsgaPlanFixture(t)
	p = input.Payload
	p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem = 500, 2000, 1024, 4096
	policy.Namespace = s.kube.namespace
	plan, err := newDSGACreationPlan(p, policy, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `UPDATE operations SET state='failed',lease_owner=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, old); err != nil {
		t.Fatal(err)
	}
	op := newID("op_dsga_")
	steps := createSteps("create_endpoint", true)
	if err = addDSGACreateOperation(ctx, tx, "create_endpoint", "endpoint", p.EndpointID, p.ProjectID, op, "test", "test", p, plan, "original-key", strings.Repeat("f", 64), steps); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var action string
	var count int
	if err = s.db.QueryRow(ctx, `SELECT action,(SELECT count(*) FROM operation_steps WHERE operation_id=$1) FROM operations WHERE id=$1`, op).Scan(&action, &count); err != nil || action != "create_endpoint_dsga_v1" || count != len(steps) {
		t.Fatalf("versioned operation/steps not atomically written: %s %d %v", action, count, err)
	}
	var idem string
	if err = s.db.QueryRow(ctx, `SELECT operation_id FROM idempotency_keys WHERE actor_id='test' AND key_hash=$1`, keyHash("original-key")).Scan(&idem); err != nil || idem != op {
		t.Fatal("idempotency does not refer to the original operation", err)
	}
	if _, err = s.db.Exec(ctx, `UPDATE operations SET state='running',lease_owner='worker_a',lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, op); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEON_DSGA_COMPUTE_IMAGE", "changed:latest")
	t.Setenv("NEON_DSGA_BOOTSTRAP_BYTES", "0")
	binding, err := s.loadDSGACreationBinding(ctx, op, "worker_a")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := binding.creationPlan(ctx)
	if err != nil || !reflect.DeepEqual(loaded, plan) {
		t.Fatal("recovery re-selected frozen inputs", err)
	}
	stale := *binding
	stale.digest[0] ^= 1
	if _, err := stale.creationPlan(ctx); !errors.Is(err, errDSGACreationPlan) {
		t.Fatal("plan reader ignored the original complete intent digest")
	}
	if _, err := s.db.Exec(ctx, `UPDATE operations SET payload=jsonb_set(payload,'{dsga_creation,image}','"changed:latest"') WHERE id=$1`, op); err == nil {
		t.Fatal("original image was mutable after acceptance")
	}
	// 新action的调用参数错误不能先写出半个Operation；原父资源事务仍由调用者回滚。
	for _, base := range []string{"", "create_unknown", "create_endpoint_dsga_v1"} {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = addDSGACreateOperation(ctx, tx, base, "endpoint", p.EndpointID, p.ProjectID, "op_invalid", "test", "test", p, plan, "other-key", "hash", steps)
		_ = tx.Rollback(ctx)
		if !errors.Is(err, errDSGACreationPlan) {
			t.Fatal("unknown action entered legacy production", err)
		}
	}
}

// 显式启用时输出本次Go生产契约，供原生Rust消费者逐字节解析/validate/settings；
// 普通CI无需该文件也执行全部Go断言。输出只含公开身份/预算，绝无密钥或密码。
func TestDSGACreationPlanNativeContractExport(t *testing.T) {
	p, _, plan := dsgaPlanFixture(t)
	native, err := plan.nativeBootstrap(p, "actual-vm-uid")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		Name  string              `json:"name"`
		Spec  dsgaNativeBootstrap `json:"spec"`
		Valid bool                `json:"valid"`
	}{
		{"production", native, true},
	}
	big := native
	big.MinimumGuestMemoryBytes = ^uint64(0)
	big.NonBufferReserveBytes = ^uint64(0) - (128 << 20)
	cases = append(cases, struct {
		Name  string              `json:"name"`
		Spec  dsgaNativeBootstrap `json:"spec"`
		Valid bool                `json:"valid"`
	}{"uint64", big, true})
	for _, kind := range []string{"version", "vm", "compute", "block", "maximum", "reserve", "floor", "rounding", "kb_overflow"} {
		v := native
		switch kind {
		case "version":
			v.ProtocolVersion = 1
		case "vm":
			v.VMUID = ""
		case "compute":
			v.ComputeInstanceID = ""
		case "block":
			v.BootstrapBufferBytes++
		case "maximum":
			v.MaximumBufferBytes = 16 * 8192
		case "reserve":
			v.NonBufferReserveBytes = 0
		case "floor":
			v.NonBufferReserveBytes = v.MinimumGuestMemoryBytes + 1
		case "rounding":
			v.NonBufferReserveBytes = v.MinimumGuestMemoryBytes - v.BootstrapBufferBytes + 1
		case "kb_overflow":
			v.MinimumGuestMemoryBytes = (uint64(1)<<31)*1024 + 1
			v.NonBufferReserveBytes = 1
		}
		cases = append(cases, struct {
			Name  string              `json:"name"`
			Spec  dsgaNativeBootstrap `json:"spec"`
			Valid bool                `json:"valid"`
		}{kind, v, false})
	}
	if output := os.Getenv("NEON_DSGA_NATIVE_PLAN_CONTRACT"); output != "" {
		if !filepath.IsAbs(output) {
			t.Fatal("contract output must be an explicit absolute path")
		}
		raw, err := json.MarshalIndent(cases, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// 这些是Go/Rust启动预算的整数可表示边界，独立于VM物理规格。通过不代表真实
// PG主共享段能装下；PG必须另计固定hash/preload/控制结构，不能据此直接运行VM。
func TestDSGABootstrapBudgetNumericBoundaries(t *testing.T) {
	base := dsgaBootstrapBudget{1 << 20, 2 << 20, uint64(1<<31-1)*1024 + 1, 1}
	for _, c := range []struct {
		name   string
		budget dsgaBootstrapBudget
		valid  bool
	}{
		{"largest_int_kb", base, true},
		{"int_kb_overflow", dsgaBootstrapBudget{base.BootstrapBufferBytes, base.MaximumBufferBytes, base.MinimumGuestMemoryBytes + 1024, 1}, false},
		{"sub_kb_budget", dsgaBootstrapBudget{base.BootstrapBufferBytes, base.MaximumBufferBytes, 1024, 1}, false},
		{"exact_buffer_budget", dsgaBootstrapBudget{base.BootstrapBufferBytes, base.MaximumBufferBytes, base.BootstrapBufferBytes + 1, 1}, true},
		{"rounding_does_not_expand", dsgaBootstrapBudget{base.BootstrapBufferBytes, base.MaximumBufferBytes, base.BootstrapBufferBytes + 1023, 1024}, false},
		{"uint64_subtraction", dsgaBootstrapBudget{base.BootstrapBufferBytes, base.MaximumBufferBytes, ^uint64(0), ^uint64(0) - (128 << 20)}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if (c.budget.validate() == nil) != c.valid {
				t.Fatal("Go budget differs from native integer representability boundary")
			}
		})
	}
}
