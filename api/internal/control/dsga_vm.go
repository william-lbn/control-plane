package control

import (
	"context"
	"encoding/json"
	"github.com/neondatabase/autoscaling/pkg/dsga/resourceguard"
	"strconv"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	core "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// 使用官方NeonVM字段，不生成部署/镜像。镜像必须由可信发行配置提供digest，
// 不从租户请求或恢复时env选择。ClusterIssuer由平台已有cert-manager链签发，
// 复用官方TLS供给/轮转；不把管理签名seed、CA私钥或业务凭据挂载给Guest。
type dsgaVMProfile struct {
	ProtocolVersion  uint32 `json:"protocol_version"`
	Architecture     string `json:"architecture"`
	RunnerImage      string `json:"runner_image"`
	KernelImage      string `json:"kernel_image"`
	RootDiskBytes    uint64 `json:"root_disk_bytes"`
	SchedulerName    string `json:"scheduler_name"`
	ServiceAccount   string `json:"service_account"`
	TLSClusterIssuer string `json:"tls_cluster_issuer"`
	// v2显式冻结整个Guest初始聚合额度，不从RSS或固定比例猜预算。v1仅供历史
	// 只读解码；新生产必须v3。历史v2仅按已绑定同UID核对，不能从env补计划。
	ResourceLimits *resourceguard.Limits `json:"resource_limits,omitempty"`
	// v3把节点授权作为原创建事实冻结；指针子结构保持profile可比较零值。
	// 旧v1/v2不得从当前env/节点快照补入放置，也不因此取得新VM创建权限。
	Placement *dsgaVMPlacement `json:"placement,omitempty"`
}

func (v dsgaVMProfile) validate() error {
	if (v.ProtocolVersion != 1 && v.ProtocolVersion != 2 && v.ProtocolVersion != 3) || (v.Architecture != "amd64" && v.Architecture != "arm64") ||
		!dsgaPinnedImage.MatchString(v.RunnerImage) || !dsgaPinnedImage.MatchString(v.KernelImage) ||
		v.RootDiskBytes < 1<<30 || v.RootDiskBytes > 1<<63-1 || v.RootDiskBytes%(1<<20) != 0 {
		return errDSGACreationPlan
	}
	for _, value := range []string{v.SchedulerName, v.ServiceAccount, v.TLSClusterIssuer} {
		if len(validation.IsDNS1123Subdomain(value)) != 0 {
			return errDSGACreationPlan
		}
	}
	if v.ProtocolVersion == 1 {
		if v.ResourceLimits != nil || v.Placement != nil {
			return errDSGACreationPlan
		}
		return nil
	}
	if v.ResourceLimits == nil || v.ResourceLimits.Validate() != nil || uint64(v.ResourceLimits.MemoryMaxBytes)%(64<<10) != 0 {
		return errDSGACreationPlan
	}
	if v.ProtocolVersion == 2 {
		if v.Placement != nil {
			return errDSGACreationPlan
		}
		return nil
	}
	return v.Placement.validate(v.Architecture)
}

func (v dsgaVMProfile) validateResourceBudget(b dsgaBootstrapBudget) error {
	if v.validate() != nil || (v.ProtocolVersion != 2 && v.ProtocolVersion != 3) || v.ResourceLimits == nil || b.validate() != nil {
		return errDSGACreationPlan
	}
	upper, err := v.ResourceLimits.TotalUpperBytes()
	if err != nil || upper > b.MinimumGuestMemoryBytes || uint64(v.ResourceLimits.MemoryMaxBytes) < b.BootstrapBufferBytes {
		return errDSGACreationPlan
	}
	return nil
}

func (v dsgaVMProfile) clone() dsgaVMProfile {
	if v.ResourceLimits != nil {
		copy := *v.ResourceLimits
		copy.HugePools = append([]resourceguard.HugePool(nil), copy.HugePools...)
		v.ResourceLimits = &copy
	}
	v.Placement = v.Placement.clone()
	return v
}

const dsgaConfigMount = "/run/neon/config"
const dsgaTLSMount = "/run/neon/management"
const dsgaTLSKey = dsgaTLSMount + "/tls.key"
const dsgaTLSCert = dsgaTLSMount + "/tls.crt"

// renderDSGAVM是原冻结计划的无副作用投影。完整模板从CREATE即held，实际
// VM UID/Config UID仍由prepareDSGACreation后续绑定；不得用同名对象补身份。
// bytes→slot先由plan核验整除，按CRD真实上限512而非仅int32上限验证。CPU
// 按整数milliCPU传递，不经float；启动Use为原最小规格，运行期另经唯一闭环。
func renderDSGAVM(p createPayload, plan dsgaCreationPlan) (*vmv1.VirtualMachine, error) {
	if plan.validate(p) != nil || plan.VM.validateResourceBudget(plan.Budget) != nil || plan.Credentials.validate() != nil {
		return nil, errDSGACreationPlan
	}
	name := kubeName(p.EndpointID)
	if !computeWorkloadPattern.MatchString(name) || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, errDSGACreationPlan
	}
	v := plan.VM
	architecture := vmv1.CPUArchitecture(v.Architecture)
	// v2原投影保持QMP供同UID历史核对；v3生产固定Sysfs，不能由env切换。
	scaling := vmv1.CpuScalingModeQMP
	if v.ProtocolVersion == 3 {
		scaling = vmv1.CpuScalingModeSysfs
	}
	grace := int64(5)
	boolean := func(value bool) *bool { return &value }
	quantity := func(bytes uint64) resource.Quantity { return *resource.NewQuantity(int64(bytes), resource.BinarySI) }
	minMemory, maxMemory := uint64(p.MinMem)*(1<<20), uint64(p.MaxMem)*(1<<20)
	bounds, err := json.Marshal(map[string]map[string]string{
		"min": {"cpu": vmv1.MilliCPU(p.MinCPU).ToResourceQuantity().String(), "mem": strconv.FormatUint(minMemory, 10)},
		"max": {"cpu": vmv1.MilliCPU(p.MaxCPU).ToResourceQuantity().String(), "mem": strconv.FormatUint(maxMemory, 10)},
	})
	if err != nil {
		return nil, errDSGACreationPlan
	}
	labels := credentialLabels(p.ProjectID, p.EndpointID)
	labels["autoscaling.neon.tech/enabled"] = "true"
	vm := &vmv1.VirtualMachine{TypeMeta: metav1.TypeMeta{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: plan.Namespace, Labels: labels, Annotations: map[string]string{"autoscaling.neon.tech/bounds": string(bounds)}},
		Spec: vmv1.VirtualMachineSpec{
			DSGABootstrap: &vmv1.DSGABootstrap{ProtocolVersion: 1, Incarnation: plan.Incarnation, ComputeInstanceID: plan.ComputeInstanceID},
			QMP:           20183, QMPManual: 20184, RunnerPort: 25183, TerminationGracePeriodSeconds: &grace,
			SchedulerName: v.SchedulerName, ServiceAccountName: v.ServiceAccount, RestartPolicy: vmv1.RestartPolicyAlways,
			TargetArchitecture: &architecture, EnableAcceleration: boolean(true), EnableSSH: boolean(false), ServiceLinks: boolean(false), EnableNetworkMonitoring: boolean(false),
			RunnerImage: &v.RunnerImage, CpuScalingMode: &scaling,
			Guest: vmv1.Guest{
				KernelImage: &v.KernelImage, CPUs: vmv1.CPUs{Min: vmv1.MilliCPU(p.MinCPU), Use: vmv1.MilliCPU(p.MinCPU), Max: vmv1.MilliCPU(p.MaxCPU)},
				MemorySlotSize: quantity(plan.MemorySlotBytes), MemorySlots: vmv1.MemorySlots{Min: int32(minMemory / plan.MemorySlotBytes), Use: int32(minMemory / plan.MemorySlotBytes), Max: int32(maxMemory / plan.MemorySlotBytes)},
				RootDisk: vmv1.RootDisk{Image: plan.Image, Size: quantity(v.RootDiskBytes), ImagePullPolicy: core.PullIfNotPresent},
				Command:  []string{"/usr/local/bin/compute_ctl"},
				// 使用已核对的官方CLI与镜像布局。禁止--dev跳过生产VM准备；管理
				// connstr无口令，由Compute改走私有peer socket。Linux Monitor也
				// 显式走同一socket，避免默认TCP5432或对内部SU开放网络HBA。
				Args: []string{"--pgdata", "/var/db/postgres/compute", "--connstr", "postgresql://cloud_admin@localhost/postgres?port=55433", "--pgbin", "/usr/local/bin/postgres", "--compute-id", name, "--config", dsgaConfigMount + "/config.json",
					"--filecache-connstr", "host=/var/db/postgres/compute/dsg_socket port=55433 dbname=postgres user=cloud_admin sslmode=disable application_name=vm-monitor", "--external-http-port", "3080", "--internal-http-port", "3081"},
				Env:   []vmv1.EnvVar{{Name: "PG_VERSION", Value: "17"}, {Name: "TENANT_ID", Value: p.TenantID}, {Name: "TIMELINE_ID", Value: p.TimelineID}},
				Ports: []vmv1.Port{{Name: "postgres", Port: 55433, Protocol: vmv1.ProtocolTCP}, {Name: "compute-control", Port: 3080, Protocol: vmv1.ProtocolTCP}, {Name: "monitor", Port: 10301, Protocol: vmv1.ProtocolTCP}},
			},
			// 此处是Guest PG的TLS材料，非中心Agent凭据签发监听器的私钥；
			// 官方NeonVM将tls.key/tls.crt供给至该挂载并轮转，Rust已有证书watch。
			TLS: &vmv1.TLSProvisioning{CertificateIssuer: v.TLSClusterIssuer, ExpireAfter: metav1.Duration{Duration: 90 * 24 * time.Hour}, RenewBefore: metav1.Duration{Duration: 15 * 24 * time.Hour}, ServerName: name + "-management." + plan.Namespace + ".svc", MountPath: dsgaTLSMount},
			Disks: []vmv1.Disk{{Name: "compute-config", MountPath: dsgaConfigMount, ReadOnly: boolean(true), Watch: boolean(false),
				DiskSource: vmv1.DiskSource{Secret: &core.SecretVolumeSource{SecretName: name + "-config", Items: []core.KeyToPath{{Key: "config.json", Path: "config.json"}}}}}},
		}}
	if v.ProtocolVersion == 3 {
		v.Placement.apply(vm)
	}
	if _, err := vm.ValidateCreate(); err != nil {
		return nil, errDSGACreationPlan
	}
	return vm, nil
}

// 恢复核对整个安全模板，而不只看bootstrap/labels。ready后仅允许既有伸缩
// 域变更Use/TargetRevision；范围必须仍在原上下限内。镜像/命令/环境/挂载/
// 服务账号/TLS/架构等任何漂移均拒绝，绝不PATCH修补。held不允许这些运行字段
// 提前变更。Semantic比较按Quantity值处理，兼容apiserver的规范单位表示。
func sameDSGAVM(actual, wanted *vmv1.VirtualMachine) bool {
	if actual == nil || wanted == nil || actual.Spec.DSGABootstrap == nil {
		return false
	}
	copy := actual.DeepCopy()
	b := copy.Spec.DSGABootstrap
	if b.Ready {
		if copy.Spec.Guest.CPUs.Use < wanted.Spec.Guest.CPUs.Min || copy.Spec.Guest.CPUs.Use > wanted.Spec.Guest.CPUs.Max || copy.Spec.Guest.MemorySlots.Use < wanted.Spec.Guest.MemorySlots.Min || copy.Spec.Guest.MemorySlots.Use > wanted.Spec.Guest.MemorySlots.Max {
			return false
		}
		copy.Spec.Guest.CPUs.Use = wanted.Spec.Guest.CPUs.Use
		copy.Spec.Guest.MemorySlots.Use = wanted.Spec.Guest.MemorySlots.Use
		copy.Spec.TargetRevision = nil
	}
	b.VMUID, b.ConfigSecretUID, b.Ready = "", "", false
	return apiequality.Semantic.DeepEqual(copy.Spec, wanted.Spec) && copy.Annotations["autoscaling.neon.tech/bounds"] == wanted.Annotations["autoscaling.neon.tech/bounds"]
}

// prepareVM是版本化Driver的单一创建接线点：每次从原Operation及当前lease
// 重载UID，不依赖旧binding对象的空UID缓存；防止未知响应后重建已绑定资源。
// 不接受外部任意VM/Render回调。计划读取/Kube均不持SQL锁；每次副作用前后的
// 绑定仍由原Operation锁、当前lease与immutable来源核验。这里不发布SQL路由，
// 也不把启动屏障ready当成初始化/预算/物理缩容许可。
func (binding *dsgaCreationBinding) prepareVM(ctx context.Context) (*vmv1.VirtualMachine, error) {
	current, err := binding.server.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
	if err != nil {
		return nil, err
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		return nil, err
	}
	// 历史v2只允许已绑定同UID的原对象恢复核对；未绑定不能借兼容路径
	// 新POST一个没有冻结节点授权的VM。检查先于任何Kube请求，不补env。
	if plan.VM.ProtocolVersion != 3 && current.vmUID == "" {
		return nil, errDSGACreationPlan
	}
	wanted, err := renderDSGAVM(current.intent.createPayload, plan)
	if err != nil {
		return nil, err
	}
	check := func(vm *vmv1.VirtualMachine) error {
		if !sameDSGAVM(vm, wanted) {
			return errDSGACreationIdentity
		}
		return nil
	}
	vm, err := current.server.kube.prepareDSGACreation(ctx, dsgaCreationInput{Payload: current.intent.createPayload, VM: wanted,
		ExpectedVMUID: types.UID(current.vmUID), ExpectedConfigUID: types.UID(current.configUID),
		Bind: func(ctx context.Context, vm *vmv1.VirtualMachine) error {
			if err := check(vm); err != nil {
				return err
			}
			return current.bindVM(ctx, vm)
		},
		BindConfig: current.bindConfig,
		Render: func(ctx context.Context, vm *vmv1.VirtualMachine) (map[string][]byte, error) {
			if err := check(vm); err != nil {
				return nil, err
			}
			return current.renderDSGABootstrap(ctx, vm)
		},
	})
	if err != nil {
		return nil, err
	}
	// 通用创建器返回前已经直接读回同一journal/Config事实；仍核对该次
	// VM完整模板，防止只对Render前快照检查。Release的RV CAS保护其线性化点；
	// 这里不替代Controller/Runner后续对当前对象的最终执行fence。
	if err := check(vm); err != nil {
		return nil, err
	}
	return vm, nil
}
