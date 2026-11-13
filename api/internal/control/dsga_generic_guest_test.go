package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/neondatabase/autoscaling/pkg/dsga/resourceguard"
)

func genericGuestFixture(version uint32, budget dsgaBootstrapBudget) *dsgaGenericGuestProfile {
	names := []string{"dsga-physical-kernel.json", "dsga-kernel-bootstrap.json", "dsga-prebootstrap-system.json", "dsga-normal-split.json", "dsga-native-birth.json"}
	entries := []map[string]any{}
	for _, name := range names {
		policy := map[string]any{"protocol_version": 1}
		if name == "dsga-native-birth.json" {
			policy["pg_major"] = version
			policy["max_buffers"] = budget.MaximumBufferBytes / 8192
			policy["startup_buffers"] = budget.BootstrapBufferBytes / 8192
		}
		entries = append(entries, map[string]any{"asset": name, "policy": policy})
	}
	raw, _ := json.Marshal(map[string]any{"protocol_version": 1, "scope": "neon-dsga-policy-templates-v1", "templates": entries})
	return &dsgaGenericGuestProfile{ProtocolVersion: 1, PolicyTemplates: raw}
}
func genericVMFixture(t *testing.T, version uint32) (createPayload, dsgaCreationPlan) {
	p, plan := dsgaVMFixture(t)
	plan.PostgresVersion = version
	plan.NativeBootstrapVersion = 3
	plan.VM = dsgaVMProfileV5Fixture()
	plan.VM.TLSClusterIssuer = ""
	plan.VM.ResourceLimits.SystemMemoryMaxBytes = 64 << 20
	plan.VM.GenericGuest = genericGuestFixture(version, plan.Budget)
	return p, plan
}

// 此处仅验证控制面发行/冻结/投影，模板夹具不是可部署资格；真实模板由发行提供。
func TestDSGAGenericGuestUsesCommonPlanAndOriginalPublicStartup(t *testing.T) {
	for _, version := range []uint32{17, 18} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			p, plan := genericVMFixture(t, version)
			vm, err := renderDSGAVM(p, plan)
			if err != nil {
				t.Fatal(err)
			}
			if len(vm.Spec.Guest.Command) != 0 || len(vm.Spec.Guest.Args) != 0 || len(vm.Spec.Guest.Env) != 0 || len(vm.Spec.Disks) != 0 || vm.Spec.TLS != nil {
				t.Fatal("generic Guest bypassed fixed startup")
			}
			if vm.Spec.DSGABootstrap.ComputeInstanceID != plan.ComputeInstanceID || vm.Spec.DSGABootstrap.Ready || vm.Name == plan.ComputeInstanceID {
				t.Fatal("logical and execution identity conflated")
			}
			files, err := renderDSGABootstrapFiles(p, plan, "original-vm", dsgaRenderFixture())
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 4 || !bytes.Equal(files[resourceguard.PolicyTemplatesSecretKey], plan.VM.GenericGuest.PolicyTemplates) {
				t.Fatal("incomplete or changed original immutable startup")
			}
			var source resourceguard.SourceConfig
			if json.Unmarshal(files[resourceguard.ResourceSecretKey], &source) != nil || source.Config.ExecutionModel != resourceguard.TrustedControllerExecutionModel || !source.Config.SystemCgroup {
				t.Fatal("generic Guest lost original execution model")
			}
			var native map[string]any
			if json.Unmarshal(files["config.json"], &native) != nil || native["compute_ctl_config"].(map[string]any)["tls"] != nil {
				t.Fatal("configuration references unmounted Guest certificate")
			}
			var selection resourceguard.ComputeStartupSelection
			if json.Unmarshal(files[resourceguard.ComputeStartupSecretKey], &selection) != nil || selection.Validate() != nil {
				t.Fatal("original controller cannot select generic startup")
			}
			frozen := plan.VM.clone()
			plan.VM.GenericGuest.PolicyTemplates[0] = '['
			if frozen.GenericGuest.PolicyTemplates[0] != '{' {
				t.Fatal("later release mutation changed frozen template")
			}
		})
	}
}
func TestDSGAGenericGuestRejectsPartialOrForeignRelease(t *testing.T) {
	for _, name := range []string{"missing", "old-profile", "missing-runner-pod-resources", "issuer", "lease-contract", "no-system", "wrong-major", "buffer-drift", "duplicate-asset", "unknown-field"} {
		t.Run(name, func(t *testing.T) {
			p, plan := genericVMFixture(t, 17)
			switch name {
			case "missing":
				plan.VM.GenericGuest = nil
			case "old-profile":
				plan.VM.ProtocolVersion = 3
			case "missing-runner-pod-resources":
				plan.VM.RunnerPodResources = nil
			case "issuer":
				plan.VM.TLSClusterIssuer = "unused-issuer"
			case "lease-contract":
				plan.NativeBootstrapVersion = 2
			case "no-system":
				plan.VM.ResourceLimits.SystemMemoryMaxBytes = 0
			case "wrong-major":
				plan.PostgresVersion = 18
			case "buffer-drift":
				plan.Budget.MaximumBufferBytes -= 8192
			case "duplicate-asset":
				plan.VM.GenericGuest.PolicyTemplates = bytes.ReplaceAll(plan.VM.GenericGuest.PolicyTemplates, []byte("dsga-physical-kernel.json"), []byte("dsga-kernel-bootstrap.json"))
			case "unknown-field":
				plan.VM.GenericGuest.PolicyTemplates = bytes.Replace(plan.VM.GenericGuest.PolicyTemplates, []byte(`"protocol_version":1`), []byte(`"protocol_version":1,"extra":true`), 1)
			}
			if _, err := renderDSGAVM(p, plan); err == nil {
				t.Fatal("invalid generic release accepted")
			}
		})
	}
}
func TestDSGAGenericProjectReservationPreservesFrozenRelease(t *testing.T) {
	for _, version := range []uint32{17, 18} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			catalog := dsgaReleaseCatalogFixture(t)
			for i := range catalog.Releases {
				c := &catalog.Releases[i].Creation
				c.VM.ProtocolVersion = 5
				c.VM.TLSClusterIssuer = ""
				c.VM.ResourceLimits.SystemMemoryMaxBytes = 64 << 20
				c.VM.GenericGuest = genericGuestFixture(c.PostgresVersion, c.Budget)
			}
			setDSGACatalog(t, catalog)
			f.body.PostgresVersion = int(version)
			key := "generic-original-project"
			if response := f.request("creation_admin", key, f.body); response.Code != 202 {
				t.Fatalf("generic project not accepted: status=%d body=%s", response.Code, response.Body.String())
			}
			op, before, xmin := f.reservation(key)
			// 冻结发行后环境目录更换不能改原接受意图。
			for i := range catalog.Releases {
				catalog.Releases[i].Creation.Image = "example.invalid/neon@sha256:" + strings.Repeat("f", 64)
			}
			setDSGACatalog(t, catalog)
			if response := f.request("creation_admin", key, f.body); response.Code != 202 {
				t.Fatal("original idempotent request lost", response.Code)
			}
			next, after, afterXmin := f.reservation(key)
			if next != op || before != after || xmin != afterXmin {
				t.Fatal("retry rewrote original release")
			}
		})
	}
}
