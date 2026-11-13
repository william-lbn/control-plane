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

// DSGA Runner Pod必须有可调度的硬资源预算。内存预留沿用已验证的
// PG17/PG18生产模板：Guest最大内存之外额外保留3Gi，覆盖QEMU/Runner及
// 镜像初始化阶段；CPU request至少覆盖Guest最大CPU，limit由平台发行目录冻结。
const dsgaRunnerPodMemoryOverheadBytes int64 = 3 << 30

// dsgaRunnerPodResources是可信发行目录中的Kubernetes Runner Pod资源合同。
// 不接受租户提交的Pod资源；这些值随创建计划冻结，恢复时不重新估算。
type dsgaRunnerPodResources struct {
	CPURequestMilli    int64 "json:\"cpu_request_milli\""
	CPULimitMilli      int64 "json:\"cpu_limit_milli\""
	MemoryRequestBytes int64 "json:\"memory_request_bytes\""
	MemoryLimitBytes   int64 "json:\"memory_limit_bytes\""
}

func (p *dsgaRunnerPodResources) validate() error {
	if p == nil || p.CPURequestMilli <= 0 || p.CPULimitMilli < p.CPURequestMilli ||
		p.MemoryRequestBytes <= 0 || p.MemoryLimitBytes != p.MemoryRequestBytes {
		return errDSGACreationPlan
	}
	return nil
}

// validateForBounds保证平台硬预算覆盖租户可请求的最大Guest规格。
// 内存request必须等于limit，避免Kubernetes只预留较小request而实际Runner
// 在初始化或Guest运行时使用更高内存；3Gi预留与既有11Gi/8Gi Guest模板一致。
func (p *dsgaRunnerPodResources) validateForBounds(maxCPU, maxMemMiB int) error {
	if p.validate() != nil || maxCPU <= 0 || maxMemMiB <= 0 {
		return errDSGACreationPlan
	}
	requiredMemory := int64(maxMemMiB)*(1<<20) + dsgaRunnerPodMemoryOverheadBytes
	if p.CPURequestMilli < int64(maxCPU) || p.CPULimitMilli > int64(maxCPU)*4 ||
		p.MemoryRequestBytes < requiredMemory {
		return errDSGACreationPlan
	}
	return nil
}

func (p *dsgaRunnerPodResources) requirements() core.ResourceRequirements {
	if p == nil {
		return core.ResourceRequirements{}
	}
	return core.ResourceRequirements{
		Requests: core.ResourceList{
			core.ResourceCPU:    *resource.NewMilliQuantity(p.CPURequestMilli, resource.DecimalSI),
			core.ResourceMemory: *resource.NewQuantity(p.MemoryRequestBytes, resource.BinarySI),
		},
		Limits: core.ResourceList{
			core.ResourceCPU:    *resource.NewMilliQuantity(p.CPULimitMilli, resource.DecimalSI),
			core.ResourceMemory: *resource.NewQuantity(p.MemoryLimitBytes, resource.BinarySI),
		},
	}
}

// 使用官方NeonVM字段，不生成部署/镜像。镜像必须由可信发行配置提供digest，
// 不从租户请求或恢复时env选择。v3冻结节点放置，v4冻结通用Guest启动，v5
// 在原有启动合同上增加不可变Runner Pod硬资源预算；旧版本保持原始投影。
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
	// 只读解码；新生产显式v3/v4。历史v2仅按已绑定同UID核对，不能从env补计划。
	ResourceLimits *resourceguard.Limits `json:"resource_limits,omitempty"`
	// v5显式冻结Runner Pod的request/limit；历史v3/v4不回填该字段，
	// 避免恢复时悄悄改变原VM规格。
	RunnerPodResources *dsgaRunnerPodResources `json:"runner_pod_resources,omitempty"`
	// v3把节点授权作为原创建事实冻结；指针子结构保持profile可比较零值。
	// 旧v1/v2不得从当前env/节点快照补入放置，也不因此取得新VM创建权限。
	Placement    *dsgaVMPlacement         `json:"placement,omitempty"`
	GenericGuest *dsgaGenericGuestProfile `json:"generic_guest,omitempty"`
}

func (v dsgaVMProfile) validate() error {
	if (v.ProtocolVersion != 1 && v.ProtocolVersion != 2 && v.ProtocolVersion != 3 && v.ProtocolVersion != 4 && v.ProtocolVersion != 5) || (v.Architecture != "amd64" && v.Architecture != "arm64") ||
		!dsgaPinnedImage.MatchString(v.RunnerImage) || !dsgaPinnedImage.MatchString(v.KernelImage) ||
		v.RootDiskBytes < 1<<30 || v.RootDiskBytes > 1<<63-1 || v.RootDiskBytes%(1<<20) != 0 {
		return errDSGACreationPlan
	}
	issuers := []string{v.SchedulerName, v.ServiceAccount}
	if v.generic() {
		if v.TLSClusterIssuer != "" || v.GenericGuest == nil {
			return errDSGACreationPlan
		}
		if _, err := v.GenericGuest.templates(); err != nil {
			return err
		}
	} else {
		if v.GenericGuest != nil {
			return errDSGACreationPlan
		}
		issuers = append(issuers, v.TLSClusterIssuer)
	}
	for _, value := range issuers {
		if len(validation.IsDNS1123Subdomain(value)) != 0 {
			return errDSGACreationPlan
		}
	}
	if v.ProtocolVersion == 1 {
		if v.ResourceLimits != nil || v.RunnerPodResources != nil || v.Placement != nil {
			return errDSGACreationPlan
		}
		return nil
	}
	if v.ResourceLimits == nil || v.ResourceLimits.Validate() != nil || uint64(v.ResourceLimits.MemoryMaxBytes)%(64<<10) != 0 {
		return errDSGACreationPlan
	}
	if v.ProtocolVersion == 2 {
		// v2是已发布的历史协议：不能把v3引入的Runner Pod资源合同
		// 回填到旧计划，否则旧计划的重放/恢复会悄悄改变原始Pod规格。
		if v.Placement != nil || v.RunnerPodResources != nil {
			return errDSGACreationPlan
		}
		return nil
	}
	switch v.ProtocolVersion {
	case 3:
		// v3只冻结Placement，不含Runner Pod资源字段。
		if v.generic() || v.RunnerPodResources != nil {
			return errDSGACreationPlan
		}
	case 4:
		// v4是已发布的通用Guest协议；历史计划必须继续按原始Pod规格投影。
		if !v.generic() || v.RunnerPodResources != nil {
			return errDSGACreationPlan
		}
	case 5:
		if v.RunnerPodResources == nil || v.RunnerPodResources.validate() != nil {
			return errDSGACreationPlan
		}
	default:
		return errDSGACreationPlan
	}
	return v.Placement.validate(v.Architecture)
}

// validatePodResourcesForBounds只校验v5新增的Runner Pod硬预算。v3/v4是已
// 发布的不可变历史协议，缺少该字段是其原始语义，不得在恢复时静默回填。
func (v dsgaVMProfile) validatePodResourcesForBounds(maxCPU, maxMemMiB int) error {
	if v.ProtocolVersion != 5 {
		return nil
	}
	return v.RunnerPodResources.validateForBounds(maxCPU, maxMemMiB)
}

func (v dsgaVMProfile) validateResourceBudget(b dsgaBootstrapBudget) error {
	if v.validate() != nil || (v.ProtocolVersion != 2 && v.ProtocolVersion != 3 && v.ProtocolVersion != 4 && v.ProtocolVersion != 5) || v.ResourceLimits == nil || b.validate() != nil {
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
	if v.RunnerPodResources != nil {
		copy := *v.RunnerPodResources
		v.RunnerPodResources = &copy
	}
	v.Placement = v.Placement.clone()
	v.GenericGuest = v.GenericGuest.clone()
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
	if v.ProtocolVersion >= 3 {
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
			PodResources:       v.RunnerPodResources.requirements(),
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
				Args: []string{"--pgdata", "/var/db/postgres/compute", "--connstr", "postgresql://cloud_admin@localhost/postgres?port=" + strconv.Itoa(dsgaPostgresPort), "--pgbin", "/usr/local/bin/postgres", "--compute-id", name, "--config", dsgaConfigMount + "/config.json",
					"--filecache-connstr", "host=/var/db/postgres/compute/dsg_socket port=" + strconv.Itoa(dsgaPostgresPort) + " dbname=postgres user=cloud_admin sslmode=disable application_name=vm-monitor", "--external-http-port", "3080", "--internal-http-port", "3081"},
				Env:   []vmv1.EnvVar{{Name: "PG_VERSION", Value: strconv.FormatUint(uint64(plan.PostgresVersion), 10)}, {Name: "TENANT_ID", Value: p.TenantID}, {Name: "TIMELINE_ID", Value: p.TimelineID}},
				Ports: []vmv1.Port{{Name: "postgres", Port: dsgaPostgresPort, Protocol: vmv1.ProtocolTCP}, {Name: "compute-control", Port: 3080, Protocol: vmv1.ProtocolTCP}, {Name: "monitor", Port: 10301, Protocol: vmv1.ProtocolTCP}},
			},
			// 此处是Guest PG的TLS材料，非中心Agent凭据签发监听器的私钥；
			// 官方NeonVM将tls.key/tls.crt供给至该挂载并轮转，Rust已有证书watch。
			TLS: &vmv1.TLSProvisioning{CertificateIssuer: v.TLSClusterIssuer, ExpireAfter: metav1.Duration{Duration: 90 * 24 * time.Hour}, RenewBefore: metav1.Duration{Duration: 15 * 24 * time.Hour}, ServerName: name + "-management." + plan.Namespace + ".svc", MountPath: dsgaTLSMount},
			Disks: []vmv1.Disk{{Name: "compute-config", MountPath: dsgaConfigMount, ReadOnly: boolean(true), Watch: boolean(false),
				DiskSource: vmv1.DiskSource{Secret: &core.SecretVolumeSource{SecretName: name + "-config", Items: []core.KeyToPath{{Key: "config.json", Path: "config.json"}}}}}},
		}}
	if v.ProtocolVersion >= 3 {
		v.Placement.apply(vm)
	}
	if v.generic() {
		// 通用Guest沿固定镜像启动；配置由原Controller交公钥ISO，再由Worker
		// 调原生认证configure。不能恢复旧command/env/mount脚本绕过Guest检查。
		if v.supply() != nil {
			vm.Spec.ServiceAccountName = dsgaRunnerAccount(name)
		}
		vm.Spec.Guest.Command = nil
		vm.Spec.Guest.Args = nil
		vm.Spec.Guest.Env = nil
		vm.Spec.Disks = nil
		vm.Spec.TLS = nil
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
		// 只有v5冻结了Runner Pod硬预算，允许从未绑定状态创建新VM。
		// v1-v4仍可GET并核对原UID，未知写入的原对象可以恢复；若对象不存在，
		// 必须拒绝POST，不能用新资源预算改写历史Operation。
		RequireExisting: plan.VM.ProtocolVersion != 5,
		ExpectedVMUID:   types.UID(current.vmUID), ExpectedConfigUID: types.UID(current.configUID),
		Bind: func(ctx context.Context, vm *vmv1.VirtualMachine) error {
			if err := check(vm); err != nil {
				return err
			}
			return current.bindVM(ctx, vm)
		},
		BindConfig: current.bindConfig,
		BeforeRelease: func(ctx context.Context, vm *vmv1.VirtualMachine) error {
			if plan.VM.supply() == nil {
				return nil
			}
			// 在原Config绑定后再次核当前Operation lease，随后释放原启动屏障。
			if _, err := current.server.loadDSGACreationBinding(ctx, current.operationID, current.workerID); err != nil {
				return err
			}
			return current.server.kube.ensureDSGASupplyAccess(ctx, vm, plan)
		},
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
