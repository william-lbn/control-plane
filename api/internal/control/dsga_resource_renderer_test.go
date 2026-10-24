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

func TestDSGAResourcePublicPolicyRequiresV2BeforeReservation(t *testing.T) {
	_, base, _ := dsgaPlanFixture(t)
	base.VM = dsgaVMProfileFixture()
	policy := dsgaProjectPolicy{1, "test-region", createBounds{500, 2000, 1024, 4096}, base}
	raw, _ := json.Marshal(policy)
	t.Setenv("NEON_DSGA_CREATION_POLICY", string(raw))
	if _, err := readDSGAProjectPolicy("neon"); err != nil {
		t.Fatal(err)
	}
	policy.Creation.VM.ProtocolVersion = 1
	policy.Creation.VM.ResourceLimits = nil
	raw, _ = json.Marshal(policy)
	t.Setenv("NEON_DSGA_CREATION_POLICY", string(raw))
	if _, err := readDSGAProjectPolicy("neon"); err == nil {
		t.Fatal("new public reservation accepted missing original resource budget")
	}
}
