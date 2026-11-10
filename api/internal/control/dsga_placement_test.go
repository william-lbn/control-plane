package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	core "k8s.io/api/core/v1"
)

func dsgaVMPlacementFixture() *dsgaVMPlacement {
	return &dsgaVMPlacement{NodeSelector: map[string]string{
		core.LabelOSStable: "linux", core.LabelArchStable: "amd64",
		dsgaMemoryPoolLabel: "worker-117-worker-118-worker-119",
	}, AllowedNodeNames: []string{"worker-119", "worker-117", "worker-118"}}
}

func TestDSGAVM3FreezesSysfsPlacementAndRejectsRuntimeDrift(t *testing.T) {
	p, plan := dsgaVMFixture(t)
	before, _ := json.Marshal(plan)
	vm, err := renderDSGAVM(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	if vm.Spec.CpuScalingMode == nil || *vm.Spec.CpuScalingMode != vmv1.CpuScalingModeSysfs ||
		!reflect.DeepEqual(vm.Spec.NodeSelector, plan.VM.Placement.NodeSelector) || vm.Spec.Affinity == nil || vm.Spec.Affinity.NodeAffinity == nil {
		t.Fatal("production profile did not render the frozen Sysfs/placement contract")
	}
	terms := vm.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
		t.Fatal("placement introduced an alternative OR term")
	}
	e := terms[0].MatchExpressions[0]
	if e.Key != core.LabelHostname || e.Operator != core.NodeSelectorOpIn || !reflect.DeepEqual(e.Values, []string{"worker-117", "worker-118", "worker-119"}) {
		t.Fatal("exact hostname allowlist was lost")
	}
	t.Setenv("NEON_DSGA_CREATION_POLICY", `{"vm":{"placement":{"allowed_node_names":["ubuntu"]}}}`)
	again, err := renderDSGAVM(p, plan)
	a, _ := json.Marshal(vm)
	b, _ := json.Marshal(again)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatal("recovery adopted current environment")
	}
	for name, mutate := range map[string]func(*vmv1.VirtualMachine){
		"pool":             func(v *vmv1.VirtualMachine) { v.Spec.NodeSelector[dsgaMemoryPoolLabel] = "foreign" },
		"missing_selector": func(v *vmv1.VirtualMachine) { v.Spec.NodeSelector = nil },
		"host": func(v *vmv1.VirtualMachine) {
			v.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0] = "ubuntu"
		},
		"or_arch_bypass": func(v *vmv1.VirtualMachine) {
			q := v.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			q.NodeSelectorTerms = append(q.NodeSelectorTerms, core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{{Key: core.LabelArchStable, Operator: core.NodeSelectorOpIn, Values: []string{"amd64"}}}})
		},
		"qmp": func(v *vmv1.VirtualMachine) { mode := vmv1.CpuScalingModeQMP; v.Spec.CpuScalingMode = &mode },
	} {
		t.Run(name, func(t *testing.T) {
			actual := vm.DeepCopy()
			mutate(actual)
			if sameDSGAVM(actual, vm) {
				t.Fatal("startup placement drift accepted")
			}
		})
	}
	vm.Spec.NodeSelector[dsgaMemoryPoolLabel] = "mutated_output"
	vm.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0] = "mutated-output"
	after, _ := json.Marshal(plan)
	if !bytes.Equal(before, after) {
		t.Fatal("rendered API object aliased original plan")
	}
}

func TestDSGAVM3SnapshotOwnsPlacementAndHistoricalVersionsStayFrozen(t *testing.T) {
	p, policy, _ := dsgaPlanFixture(t)
	p.EndpointID = "ep_0123456789abcdef"
	policy.VM = dsgaVMProfileFixture()
	plan, err := newDSGACreationPlan(p, policy, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(plan)
	policy.VM.Placement.NodeSelector[dsgaMemoryPoolLabel] = "changed"
	policy.VM.Placement.AllowedNodeNames[0] = "ubuntu"
	after, _ := json.Marshal(plan)
	if !bytes.Equal(before, after) {
		t.Fatal("policy mutation changed original placement")
	}
	var restored dsgaCreationPlan
	if decodeDSGAObject(before, &restored) != nil || restored.validate(p) != nil || !reflect.DeepEqual(restored.VM.Placement, plan.VM.Placement) {
		t.Fatal("persistent plan lost placement")
	}
	// 旧v2只能按其原字段/原QMP/无节点授权投影，不能因env新名单被默默升级。
	old := plan
	old.VM = plan.VM.clone()
	old.VM.ProtocolVersion = 2
	old.VM.Placement = nil
	old.Credentials = dsgaCredentialReference{1, "original-credentials", strings.Repeat("d", 64)}
	t.Setenv("NEON_DSGA_CREATION_POLICY", string(before))
	vm, err := renderDSGAVM(p, old)
	if err != nil || vm.Spec.NodeSelector != nil || vm.Spec.Affinity != nil || vm.Spec.CpuScalingMode == nil || *vm.Spec.CpuScalingMode != vmv1.CpuScalingModeQMP {
		t.Fatal("historical v2 gained new execution placement")
	}
	old.VM.ProtocolVersion = 1
	old.VM.ResourceLimits = nil
	if old.VM.validate() != nil {
		t.Fatal("historical v1 read compatibility lost")
	}
	old.VM.Placement = dsgaVMPlacementFixture()
	if old.VM.validate() == nil {
		t.Fatal("legacy version accepted hidden placement")
	}
}

func TestDSGAVM3RejectsIncompletePlacementBeforeProducingTemplate(t *testing.T) {
	for name, mutate := range map[string]func(*dsgaVMProfile){
		"missing":             func(v *dsgaVMProfile) { v.Placement = nil },
		"missing_selector":    func(v *dsgaVMProfile) { v.Placement.NodeSelector = nil },
		"missing_pool":        func(v *dsgaVMProfile) { delete(v.Placement.NodeSelector, dsgaMemoryPoolLabel) },
		"wrong_os":            func(v *dsgaVMProfile) { v.Placement.NodeSelector[core.LabelOSStable] = "windows" },
		"wrong_arch":          func(v *dsgaVMProfile) { v.Placement.NodeSelector[core.LabelArchStable] = "arm64" },
		"bad_label":           func(v *dsgaVMProfile) { v.Placement.NodeSelector["invalid key"] = "value" },
		"bad_value":           func(v *dsgaVMProfile) { v.Placement.NodeSelector[dsgaMemoryPoolLabel] = "bad value" },
		"no_hosts":            func(v *dsgaVMProfile) { v.Placement.AllowedNodeNames = nil },
		"two_hosts":           func(v *dsgaVMProfile) { v.Placement.AllowedNodeNames = v.Placement.AllowedNodeNames[:2] },
		"four_hosts":          func(v *dsgaVMProfile) { v.Placement.AllowedNodeNames = append(v.Placement.AllowedNodeNames, "ubuntu") },
		"duplicate":           func(v *dsgaVMProfile) { v.Placement.AllowedNodeNames[0] = v.Placement.AllowedNodeNames[1] },
		"invalid_host":        func(v *dsgaVMProfile) { v.Placement.AllowedNodeNames[0] = "bad/host" },
		"v2_hidden_placement": func(v *dsgaVMProfile) { v.ProtocolVersion = 2 },
		"unknown_version":     func(v *dsgaVMProfile) { v.ProtocolVersion = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			p, plan := dsgaVMFixture(t)
			mutate(&plan.VM)
			vm, err := renderDSGAVM(p, plan)
			if !errors.Is(err, errDSGACreationPlan) || vm != nil {
				t.Fatal("invalid placement produced usable VM")
			}
		})
	}
}

func TestDSGAVM2UnboundRecoveryNeverPostsAndBoundRecoveryNeverAdoptsPlacement(t *testing.T) {
	old := dsgaVMProfileFixture()
	old.ProtocolVersion = 2
	old.Placement = nil
	ctx, _, _, f, input, binding, _ := credentialSourceFixtureWithVM(t, old)
	f.mu.Lock()
	before := append([]string(nil), f.trace...)
	f.mu.Unlock()
	if vm, err := binding.prepareVM(ctx); !errors.Is(err, errDSGACreationPlan) || vm != nil {
		t.Fatal("unbound v2 created VM without original placement")
	}
	f.mu.Lock()
	unchanged := reflect.DeepEqual(before, f.trace) && f.vm == nil
	f.mu.Unlock()
	if !unchanged {
		t.Fatal("unbound rejection reached a Kube side effect")
	}
	// 显式由旧生产者的底层夹具建立已接受同UID事实，不能UPDATE原Operation补v3。
	if _, err := f.kube.prepareDSGACreation(ctx, input); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	before = append([]string(nil), f.trace...)
	f.mu.Unlock()
	t.Setenv("NEON_DSGA_CREATION_POLICY", `{"vm":{"protocol_version":3,"placement":{"allowed_node_names":["ubuntu"]}}}`)
	vm, err := binding.prepareVM(ctx)
	if err != nil || vm == nil || vm.UID != "vm-uid" || vm.Spec.NodeSelector != nil || vm.Spec.Affinity != nil || *vm.Spec.CpuScalingMode != vmv1.CpuScalingModeQMP {
		t.Fatal("bound v2 changed original execution contract", err)
	}
	f.mu.Lock()
	unchanged = reflect.DeepEqual(before, f.trace)
	f.vm = nil
	f.mu.Unlock()
	if !unchanged {
		t.Fatal("bound recovery performed unexpected Kube write")
	}
	if vm, err = binding.prepareVM(ctx); err == nil || vm != nil {
		t.Fatal("missing same-UID v2 VM was recreated")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vm != nil || !reflect.DeepEqual(before, f.trace) {
		t.Fatal("missing v2 UID caused replacement POST")
	}
}
