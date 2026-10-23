package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// digest仅为显式测试输入，不声明镜像已构建/可运行；Linux使用实际自有发行
// digest。这个fixture不接触集群、不生成部署清单，直接测试生产VM构造函数。
func dsgaVMProfileFixture() dsgaVMProfile {
	return dsgaVMProfile{ProtocolVersion: 1, Architecture: "amd64", RunnerImage: "ghcr.io/william-lbn/neonvm-runner@sha256:" + strings.Repeat("b", 64),
		KernelImage: "ghcr.io/william-lbn/neonvm-kernel@sha256:" + strings.Repeat("c", 64), RootDiskBytes: 8 << 30,
		SchedulerName: "autoscale-scheduler", ServiceAccount: "neonvm-runner", TLSClusterIssuer: "neon-compute-issuer"}
}

func dsgaVMFixture(t *testing.T) (createPayload, dsgaCreationPlan) {
	t.Helper()
	p, _, plan := dsgaPlanFixture(t)
	p.EndpointID = "ep_0123456789abcdef"
	plan.VM = dsgaVMProfileFixture()
	plan.Credentials = dsgaCredentialReference{1, "frozen-credential-uid", strings.Repeat("d", 64)}
	return p, plan
}

func TestDSGAVMTemplateFreezesOfficialRuntimeAndTLS(t *testing.T) {
	p, plan := dsgaVMFixture(t)
	vm, err := renderDSGAVM(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := vm.DSGABootstrapHeld(); err != nil || !held || vm.UID != "" || vm.ResourceVersion != "" {
		t.Fatal("production template was not born held")
	}
	if vm.Spec.RunnerImage == nil || *vm.Spec.RunnerImage != plan.VM.RunnerImage || vm.Spec.Guest.KernelImage == nil || *vm.Spec.Guest.KernelImage != plan.VM.KernelImage || vm.Spec.Guest.RootDisk.Image != plan.Image {
		t.Fatal("template did not use frozen images")
	}
	if vm.Spec.Guest.CPUs.Min != 500 || vm.Spec.Guest.CPUs.Use != 500 || vm.Spec.Guest.CPUs.Max != 2000 || vm.Spec.Guest.MemorySlotSize.Value() != 1<<30 || vm.Spec.Guest.MemorySlots != (vmv1.MemorySlots{Min: 1, Use: 1, Max: 4}) {
		t.Fatal("CPU/byte/slot conversion changed requested resources")
	}
	if vm.Spec.TLS == nil || vm.Spec.TLS.CertificateIssuer != plan.VM.TLSClusterIssuer || vm.Spec.TLS.MountPath != dsgaTLSMount || vm.Spec.TLS.ServerName != vm.Name+"-management.neon.svc" {
		t.Fatal("official TLS provisioning not connected")
	}
	if len(vm.Spec.Disks) != 1 || vm.Spec.Disks[0].Secret == nil || vm.Spec.Disks[0].Secret.SecretName != vm.Name+"-config" || len(vm.Spec.Disks[0].Secret.Items) != 1 || vm.Spec.Disks[0].Secret.Items[0].Key != "config.json" || vm.Spec.Disks[0].ReadOnly == nil || !*vm.Spec.Disks[0].ReadOnly {
		t.Fatal("private credentials were mounted or config was writable")
	}
	raw, err := renderDSGAComputeBootstrap(p, plan, "actual-vm-uid", dsgaRenderFixture())
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Ctl struct {
			TLS struct {
				Key  string `json:"key_path"`
				Cert string `json:"cert_path"`
			} `json:"tls"`
		} `json:"compute_ctl_config"`
	}
	if json.Unmarshal(raw, &config) != nil || config.Ctl.TLS.Key != vm.Spec.TLS.MountPath+"/tls.key" || config.Ctl.TLS.Cert != vm.Spec.TLS.MountPath+"/tls.crt" {
		t.Fatal("Compute paths differ from actual official TLS mount filenames")
	}
	if strings.Contains(strings.Join(vm.Spec.Guest.Args, " "), "--dev") || !strings.Contains(strings.Join(vm.Spec.Guest.Args, " "), "host=/var/db/postgres/compute/dsg_socket port=55433") {
		t.Fatal("production CLI used development bypass or TCP superuser monitor")
	}
	if len(vm.Spec.Guest.Env) != 3 || vm.Spec.Guest.Env[0].Value != "17" || vm.Spec.EnableSSH == nil || *vm.Spec.EnableSSH {
		t.Fatal("unexpected startup credential/bootstrap/SSH environment")
	}
	before, _ := json.Marshal(vm)
	t.Setenv("NEON_VM_COMPUTE_IMAGE", "changed:latest")
	t.Setenv("NEON_COMPUTE_CONTROL_HOST", "attacker.invalid")
	again, err := renderDSGAVM(p, plan)
	after, _ := json.Marshal(again)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("recovery reloaded environment instead of original frozen plan")
	}
	// 启动契约按原整数milliCPU，支持合法的分数CPU而非向上取整vCPU。
	p.MinCPU = 250
	quarter, err := renderDSGAVM(p, plan)
	if err != nil || quarter.Spec.Guest.CPUs.Use != 250 {
		t.Fatal("fractional CPU changed")
	}
}

func TestDSGAVMTemplateRejectsUnfrozenAndCRDIncompatibleInputs(t *testing.T) {
	for name, mutate := range map[string]func(*createPayload, *dsgaCreationPlan){
		"missing_profile":     func(_ *createPayload, v *dsgaCreationPlan) { v.VM = dsgaVMProfile{} },
		"missing_credentials": func(_ *createPayload, v *dsgaCreationPlan) { v.Credentials = dsgaCredentialReference{} },
		"runner_tag":          func(_ *createPayload, v *dsgaCreationPlan) { v.VM.RunnerImage = "runner:latest" },
		"kernel_tag":          func(_ *createPayload, v *dsgaCreationPlan) { v.VM.KernelImage = "kernel:latest" },
		"architecture":        func(_ *createPayload, v *dsgaCreationPlan) { v.VM.Architecture = "unknown" },
		"disk_overflow":       func(_ *createPayload, v *dsgaCreationPlan) { v.VM.RootDiskBytes = 1 << 63 },
		"disk_rounding":       func(_ *createPayload, v *dsgaCreationPlan) { v.VM.RootDiskBytes++ },
		"scheduler":           func(_ *createPayload, v *dsgaCreationPlan) { v.VM.SchedulerName = "" },
		"account":             func(_ *createPayload, v *dsgaCreationPlan) { v.VM.ServiceAccount = "foreign/account" },
		"issuer":              func(_ *createPayload, v *dsgaCreationPlan) { v.VM.TLSClusterIssuer = "" },
		"slot_513":            func(p *createPayload, _ *dsgaCreationPlan) { p.MaxMem = 513 * 1024 },
		"unroutable_endpoint": func(p *createPayload, _ *dsgaCreationPlan) { p.EndpointID = "ep_other" },
	} {
		t.Run(name, func(t *testing.T) {
			p, plan := dsgaVMFixture(t)
			mutate(&p, &plan)
			vm, err := renderDSGAVM(p, plan)
			if !errors.Is(err, errDSGACreationPlan) || vm != nil {
				t.Fatal("invalid template returned usable VM")
			}
		})
	}
	p, plan := dsgaVMFixture(t)
	p.MaxMem = 512 * 1024
	if vm, err := renderDSGAVM(p, plan); err != nil || vm.Spec.Guest.MemorySlots.Max != 512 {
		t.Fatal("real CRD upper bound rejected")
	}
}

func TestDSGAVMRecoveryRejectsStartupDriftAndAllowsOnlyRuntimeUse(t *testing.T) {
	p, plan := dsgaVMFixture(t)
	wanted, err := renderDSGAVM(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	actual := wanted.DeepCopy()
	actual.Spec.DSGABootstrap.Ready = true
	actual.Spec.DSGABootstrap.VMUID = "vm-uid"
	actual.Spec.DSGABootstrap.ConfigSecretUID = "config-uid"
	actual.Spec.Guest.CPUs.Use = 1000
	actual.Spec.Guest.MemorySlots.Use = 2
	actual.Spec.TargetRevision = &vmv1.RevisionWithTime{Revision: vmv1.Revision{Value: 1}}
	if !sameDSGAVM(actual, wanted) {
		t.Fatal("legitimate runtime resource values rejected")
	}
	for name, mutate := range map[string]func(*vmv1.VirtualMachine){
		"image":         func(v *vmv1.VirtualMachine) { v.Spec.Guest.RootDisk.Image = "changed:latest" },
		"command":       func(v *vmv1.VirtualMachine) { v.Spec.Guest.Args = append(v.Spec.Guest.Args, "--dev") },
		"tls":           func(v *vmv1.VirtualMachine) { v.Spec.TLS.CertificateIssuer = "other-issuer" },
		"account":       func(v *vmv1.VirtualMachine) { v.Spec.ServiceAccountName = "other-account" },
		"mount":         func(v *vmv1.VirtualMachine) { v.Spec.Disks[0].Secret.SecretName = "other-config" },
		"memory_bounds": func(v *vmv1.VirtualMachine) { v.Spec.Guest.MemorySlots.Max++ },
		"memory_use":    func(v *vmv1.VirtualMachine) { v.Spec.Guest.MemorySlots.Use = 5 },
		"cpu_use":       func(v *vmv1.VirtualMachine) { v.Spec.Guest.CPUs.Use = 2001 },
		"bootstrap":     func(v *vmv1.VirtualMachine) { v.Spec.DSGABootstrap.ComputeInstanceID = "other-compute" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := actual.DeepCopy()
			mutate(copy)
			if sameDSGAVM(copy, wanted) {
				t.Fatal("changed startup plan accepted")
			}
		})
	}
}

func TestDSGAVMProductionBindingResumesUnknownConfigAndNeverRecreatesBoundUID(t *testing.T) {
	ctx, s, op, f, _, binding, _ := credentialSourceFixture(t)
	f.lost = "config"
	if vm, err := binding.prepareVM(ctx); err == nil || vm != nil {
		t.Fatal("unknown config write fabricated success")
	}
	var vmUID string
	var configUID *string
	if err := s.db.QueryRow(ctx, `SELECT vm_uid,config_uid FROM dsga_creation_bindings WHERE operation_id=$1`, op).Scan(&vmUID, &configUID); err != nil || vmUID != "vm-uid" || configUID != nil {
		t.Fatal("unknown config lost original VM binding")
	}
	f.mu.Lock()
	original := f.vm.Spec.DeepCopy()
	f.vm.Spec.Guest.Args = append(f.vm.Spec.Guest.Args, "--dev")
	f.mu.Unlock()
	if vm, err := binding.prepareVM(ctx); !errors.Is(err, errDSGACreationIdentity) || vm != nil {
		t.Fatal("production driver adopted changed same-UID VM")
	}
	f.mu.Lock()
	held := !f.vm.Spec.DSGABootstrap.Ready
	f.vm.Spec = *original
	f.mu.Unlock()
	if !held {
		t.Fatal("failed template validation released startup")
	}
	vm, err := binding.prepareVM(ctx)
	if err != nil || vm == nil || !vm.Spec.DSGABootstrap.Ready || vm.Spec.TLS == nil {
		t.Fatal("production binding failed to resume original creation", err)
	}
	if err := s.db.QueryRow(ctx, `SELECT vm_uid,config_uid FROM dsga_creation_bindings WHERE operation_id=$1`, op).Scan(&vmUID, &configUID); err != nil || vmUID != "vm-uid" || configUID == nil || *configUID != "config-uid" {
		t.Fatal("actual Config UID not persisted")
	}
	// 沿用最初binding对象（其缓存原为空）；必须重读持久UID，不能先新建再在Bind拒绝。
	f.mu.Lock()
	f.vm = nil
	trace := append([]string(nil), f.trace...)
	f.mu.Unlock()
	if vm, err := binding.prepareVM(ctx); err == nil || vm != nil {
		t.Fatal("missing bound VM recreated")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vm != nil || !reflect.DeepEqual(trace, f.trace) {
		t.Fatal("missing bound UID triggered replacement POST")
	}
}

func TestDSGACreationWritesNeverTransparentlyRetry429(t *testing.T) {
	for _, stage := range []string{"vm", "config"} {
		t.Run(stage, func(t *testing.T) {
			f, input := newCreationTest(t)
			var attempts atomic.Int32
			api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && ((stage == "vm" && r.URL.Path == f.kube.path("vm", "")) || (stage == "config" && r.URL.Path == f.kube.path("secret", ""))) {
					attempts.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(429)
					_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonTooManyRequests, Code: 429})
					return
				}
				f.serve(w, r)
			}))
			defer api.Close()
			f.kube.base, f.kube.http = api.URL, api.Client()
			if vm, err := f.kube.prepareDSGACreation(context.Background(), input); err == nil || vm != nil || attempts.Load() != 1 {
				t.Fatal("429 write was retried or reported as success")
			}
		})
	}
}
