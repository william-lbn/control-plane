package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	"github.com/neondatabase/autoscaling/pkg/dsga/resourceguard"
	"github.com/neondatabase/autoscaling/pkg/util/gzip64"
)

// 纯生产Renderer→实际Runner解码器的字节/身份合约，不调用K8s API或验证其授权。
func TestDSGAResourceRendererOriginalPairAndRunnerContract(t *testing.T) {
	p, plan := dsgaVMFixture(t)
	credentials := dsgaRenderFixture()
	files, err := renderDSGABootstrapFiles(p, plan, "actual-vm-uid", credentials)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatal("incomplete immutable configuration pair")
	}
	var s resourceguard.SourceConfig
	if err := json.Unmarshal(files[resourceguard.ResourceSecretKey], &s); err != nil || s.Validate() != nil {
		t.Fatal("invalid shared source contract", err)
	}
	if s.Config.VMUID != "actual-vm-uid" || s.Config.ComputeInstanceID != plan.ComputeInstanceID || s.BudgetCommitmentID != "bootstrap-"+plan.OperationUUID {
		t.Fatal("source was not derived from original plan")
	}
	for _, value := range []string{credentials.ApplicationVerifier, credentials.ProbeVerifier, "controlSigningSeed", "tls.key"} {
		if bytes.Contains(files[resourceguard.ResourceSecretKey], []byte(value)) {
			t.Fatal("private credential or TLS material entered resource source")
		}
	}
	again, err := renderDSGABootstrapFiles(p, plan, "actual-vm-uid", credentials)
	if err != nil || !bytes.Equal(files["resource.json"], again["resource.json"]) {
		t.Fatal("replay changed original source bytes", err)
	}
	vm, err := renderDSGAVM(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	vm.Spec.DSGABootstrap.Ready = true
	vm.Spec.DSGABootstrap.VMUID = "actual-vm-uid"
	vm.Spec.DSGABootstrap.ConfigSecretUID = "original-config-uid"
	d := resourceguard.RuntimeDocument{ProtocolVersion: 1, Incarnation: plan.Incarnation, ConfigSecretUID: "original-config-uid", SourceJSON: files["resource.json"]}
	sum := sha256.Sum256(d.SourceJSON)
	d.SourceSHA256 = hex.EncodeToString(sum[:])
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := bootstrap.DecodeRuntime(&vm.Spec, gzip64.Encode(raw))
	if err != nil || !bytes.Equal(raw, decoded) {
		t.Fatal("production source incompatible with actual Runner decoder", err)
	}
}

func TestDSGAResourceRendererRejectsPartialAndHistoricalBudget(t *testing.T) {
	for _, name := range []string{"historical", "missing", "over_guest", "undersized", "zero_outside", "duplicate_pool"} {
		t.Run(name, func(t *testing.T) {
			p, plan := dsgaVMFixture(t)
			switch name {
			case "historical":
				plan.VM.ProtocolVersion = 1
				plan.VM.ResourceLimits = nil
				plan.VM.Placement = nil
				if plan.VM.validate() != nil {
					t.Fatal("historical read compatibility lost")
				}
			case "missing":
				plan.VM.ResourceLimits = nil
			case "over_guest":
				plan.VM.ResourceLimits.MemoryMaxBytes = 2 << 30
			case "undersized":
				plan.VM.ResourceLimits.MemoryMaxBytes = 32 << 20
			case "zero_outside":
				plan.VM.ResourceLimits.OutsideUpperBytes = 0
			case "duplicate_pool":
				plan.VM.ResourceLimits.HugePools = append(plan.VM.ResourceLimits.HugePools, plan.VM.ResourceLimits.HugePools[0])
			}
			if files, err := renderDSGABootstrapFiles(p, plan, "actual-vm-uid", dsgaRenderFixture()); err == nil || files != nil {
				t.Fatal("unsafe resource inputs returned partial configuration")
			}
		})
	}
}

func TestDSGAResourcePolicySnapshotOwnsLimitsAndPools(t *testing.T) {
	p, _, base := dsgaPlanFixture(t)
	profile := dsgaVMProfileFixture()
	policy := dsgaCreationPolicy{Namespace: base.Namespace, Image: base.Image, MemorySlotBytes: base.MemorySlotBytes, Budget: base.Budget, VM: profile, Storage: base.Storage}
	plan, err := newDSGACreationPlan(p, policy, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(plan)
	policy.VM.ResourceLimits.MemoryMaxBytes = 64 << 20
	policy.VM.ResourceLimits.HugePools[0].Pages++
	after, _ := json.Marshal(plan)
	if !bytes.Equal(before, after) || strings.Contains(string(after), "controlSigningSeed") {
		t.Fatal("later platform policy mutation changed the frozen original plan")
	}
}

func TestDSGAResourcePublicPolicyRequiresV3PlacementBeforeReservation(t *testing.T) {
	_, base, _ := dsgaPlanFixture(t)
	base.VM = dsgaVMProfileFixture()
	policy := dsgaProjectPolicy{1, "test-region", createBounds{500, 2000, 1024, 4096}, base}
	raw, _ := json.Marshal(policy)
	t.Setenv("NEON_DSGA_CREATION_POLICY", string(raw))
	if _, err := readDSGAProjectPolicy("neon"); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*dsgaVMProfile){
		"historical_v1":                 func(v *dsgaVMProfile) { v.ProtocolVersion = 1; v.ResourceLimits = nil; v.Placement = nil },
		"complete_v2_without_placement": func(v *dsgaVMProfile) { v.ProtocolVersion = 2; v.Placement = nil },
		"v3_missing_placement":          func(v *dsgaVMProfile) { v.Placement = nil },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := policy
			candidate.Creation.VM = dsgaVMProfileFixture()
			mutate(&candidate.Creation.VM)
			if name == "complete_v2_without_placement" && candidate.Creation.VM.validateResourceBudget(candidate.Creation.Budget) != nil {
				t.Fatal("legacy v2 fixture lost its valid original budget")
			}
			raw, _ := json.Marshal(candidate)
			t.Setenv("NEON_DSGA_CREATION_POLICY", string(raw))
			if _, err := readDSGAProjectPolicy("neon"); err == nil {
				t.Fatal("new public reservation accepted missing frozen v3 placement")
			}
		})
	}

}

// 生产Plan clone→JSON持久读者→Renderer→实际Runner Decoder的有限任务数合同。
// 只证明原N未丢失/未补默认，不声称Mac执行了Linux pids控制器或完整私有预算。
func TestDSGAFiniteTaskLimitSurvivesFrozenPlanAndRunner(t *testing.T) {
	p, plan := dsgaVMFixture(t)
	legacyUpper, err := plan.VM.ResourceLimits.TotalUpperBytes()
	if err != nil {
		t.Fatal(err)
	}
	plan.VM.ResourceLimits.PidsMaxTasks = 64
	frozen := plan.VM.clone()
	plan.VM.ResourceLimits.PidsMaxTasks = 65
	if frozen.ResourceLimits.PidsMaxTasks != 64 {
		t.Fatal("source policy alias changed original task limit")
	}
	plan.VM = frozen
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var restored dsgaCreationPlan
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.VM.ResourceLimits.PidsMaxTasks != 64 {
		t.Fatal("persistent reader dropped task limit")
	}
	files, err := renderDSGABootstrapFiles(p, restored, "actual-vm-uid", dsgaRenderFixture())
	if err != nil {
		t.Fatal(err)
	}
	var source resourceguard.SourceConfig
	if err = json.Unmarshal(files[resourceguard.ResourceSecretKey], &source); err != nil || source.Validate() != nil {
		t.Fatal(err)
	}
	if source.Limits.PidsMaxTasks != 64 {
		t.Fatal("source renderer omitted finite tasks")
	}
	upper, err := source.Limits.TotalUpperBytes()
	if err != nil || upper != legacyUpper {
		t.Fatal("task count was added to byte budget", err)
	}
	vm, err := renderDSGAVM(p, restored)
	if err != nil {
		t.Fatal(err)
	}
	vm.Spec.DSGABootstrap.Ready = true
	vm.Spec.DSGABootstrap.VMUID = "actual-vm-uid"
	vm.Spec.DSGABootstrap.ConfigSecretUID = "original-config-uid"
	sum := sha256.Sum256(files[resourceguard.ResourceSecretKey])
	document := resourceguard.RuntimeDocument{ProtocolVersion: 1, Incarnation: restored.Incarnation, ConfigSecretUID: "original-config-uid", SourceJSON: files[resourceguard.ResourceSecretKey], SourceSHA256: hex.EncodeToString(sum[:])}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := bootstrap.DecodeRuntime(&vm.Spec, gzip64.Encode(encoded))
	if err != nil || !bytes.Equal(encoded, decoded) {
		t.Fatal("actual Runner lost frozen task source", err)
	}
	restored.VM.ResourceLimits.PidsMaxTasks = resourceguard.MaxPidsTasks + 1
	if files, err = renderDSGABootstrapFiles(p, restored, "actual-vm-uid", dsgaRenderFixture()); err == nil || files != nil {
		t.Fatal("invalid task policy produced partial files")
	}
}

// 原计划→持久JSON→配置对→实际Runner解码；不执行Linux归组/内核限额。
func TestDSGASystemChargedDomainSurvivesOriginalPlanAndRunner(t *testing.T) {
	p, plan := dsgaVMFixture(t)
	originalUpper, err := plan.VM.ResourceLimits.TotalUpperBytes()
	if err != nil {
		t.Fatal(err)
	}
	plan.VM.ResourceLimits.SystemMemoryMaxBytes = 16 << 20
	frozen := plan.VM.clone()
	plan.VM.ResourceLimits.SystemMemoryMaxBytes = 8 << 20
	if frozen.ResourceLimits.SystemMemoryMaxBytes != 16<<20 {
		t.Fatal("mutable policy changed frozen system cap")
	}
	plan.VM = frozen
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var restored dsgaCreationPlan
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	files, err := renderDSGABootstrapFiles(p, restored, "actual-vm-uid", dsgaRenderFixture())
	if err != nil {
		t.Fatal(err)
	}
	var source resourceguard.SourceConfig
	if err = json.Unmarshal(files[resourceguard.ResourceSecretKey], &source); err != nil || source.Validate() != nil {
		t.Fatal(err)
	}
	if !source.Config.SystemCgroup || source.Limits.SystemMemoryMaxBytes != 16<<20 {
		t.Fatal("source dropped original system mode/cap")
	}
	upper, err := source.Limits.TotalUpperBytes()
	if err != nil || upper != originalUpper {
		t.Fatal("system cap counted twice", err)
	}
	vm, err := renderDSGAVM(p, restored)
	if err != nil {
		t.Fatal(err)
	}
	vm.Spec.DSGABootstrap.Ready = true
	vm.Spec.DSGABootstrap.VMUID = "actual-vm-uid"
	vm.Spec.DSGABootstrap.ConfigSecretUID = "original-config-uid"
	sum := sha256.Sum256(files[resourceguard.ResourceSecretKey])
	document := resourceguard.RuntimeDocument{ProtocolVersion: 1, Incarnation: restored.Incarnation, ConfigSecretUID: "original-config-uid", SourceJSON: files[resourceguard.ResourceSecretKey], SourceSHA256: hex.EncodeToString(sum[:])}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := bootstrap.DecodeRuntime(&vm.Spec, gzip64.Encode(encoded))
	if err != nil || !bytes.Equal(encoded, decoded) {
		t.Fatal("actual Runner changed original source", err)
	}
	restored.VM.ResourceLimits.SystemMemoryMaxBytes = restored.VM.ResourceLimits.OutsideUpperBytes + 1
	if files, err = renderDSGABootstrapFiles(p, restored, "actual-vm-uid", dsgaRenderFixture()); err == nil || files != nil {
		t.Fatal("invalid system cap produced partial files")
	}
	restored.VM.ResourceLimits.SystemMemoryMaxBytes = 0
	files, err = renderDSGABootstrapFiles(p, restored, "actual-vm-uid", dsgaRenderFixture())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(files[resourceguard.ResourceSecretKey], []byte("system_cgroup")) || bytes.Contains(files[resourceguard.ResourceSecretKey], []byte("system_memory_max_bytes")) {
		t.Fatal("legacy source gained guessed system cap")
	}
}
