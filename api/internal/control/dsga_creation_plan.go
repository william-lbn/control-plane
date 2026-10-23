package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

// 计划协议只描述本次创建时冻结的可信输入，不授予普通SQL、预算或VM执行许可。
// 身份view供元数据库绑定复用；完整计划由独立严格解码器校验，不能把缺预算的
// view当成生产启动计划。版本1先支持PG17，其他大版本完成独立适配前明确拒绝。
type dsgaCreationIdentity struct {
	ProtocolVersion   uint32 `json:"protocol_version"`
	PostgresVersion   uint32 `json:"postgres_version"`
	Namespace         string `json:"namespace"`
	Incarnation       string `json:"incarnation"`
	ComputeInstanceID string `json:"compute_instance_id"`
}

// 统一bytes且不经float64；字段与原生DynamicSharedBuffersSpec逐项对应。
// reserve是平台对OS/私有工作/维护/LFC等非共享启动内存的显式承诺，不是从当前
// RSS推测未来上界。真实PG仍须在全部preload登记后核验主共享段实际布局；完整
// 运行时准入/quiesce也须独立执行，语法通过不能解除新boot的关闭状态。
type dsgaBootstrapBudget struct {
	BootstrapBufferBytes    uint64 `json:"bootstrap_buffer_bytes"`
	MaximumBufferBytes      uint64 `json:"maximum_buffer_bytes"`
	MinimumGuestMemoryBytes uint64 `json:"minimum_guest_memory_bytes"`
	NonBufferReserveBytes   uint64 `json:"non_buffer_reserve_bytes"`
}

type dsgaNativeBootstrap struct {
	ProtocolVersion   uint32 `json:"protocol_version"`
	VMUID             string `json:"vm_uid"`
	ComputeInstanceID string `json:"compute_instance_id"`
	dsgaBootstrapBudget
}

type dsgaCreationPlan struct {
	dsgaCreationIdentity
	Image string `json:"image"`
	// UUID与时间只在接受原意图前生成一次，恢复不重新读取时钟或分配随机值。
	OperationUUID   string              `json:"operation_uuid"`
	AcceptedAt      string              `json:"accepted_at"`
	MemorySlotBytes uint64              `json:"memory_slot_bytes"`
	Budget          dsgaBootstrapBudget `json:"budget"`
	// 凭据由接受前的immutable Secret保留，再把实际UID/完整Data摘要冻结进原
	// Operation。纯值避免浅拷贝共享指针；缺省只供历史计划解码，新启动回调拒绝。
	Credentials dsgaCredentialReference `json:"credentials,omitzero"`
	// VM执行所需的平台输入同样在接受时冻结，worker不再读取当前镜像/Issuer
	// 环境变量。零值仅兼容读取历史计划，生产VM构造明确拒绝缺少profile。
	VM dsgaVMProfile `json:"vm,omitzero"`
	// 存储Service/凭据Secret只冻结平台对象引用，不把JWT/CA或任意URL写入
	// Operation。历史零值可只读解码，但真实存储执行器必须拒绝；不能恢复时
	// 从当前env偷偷补齐旧计划，以免原操作落到另一个controller或身份域。
	Storage dsgaStorageProfile `json:"storage,omitzero"`
}

type dsgaCredentialReference struct {
	ProtocolVersion uint32 `json:"protocol_version"`
	UID             string `json:"uid"`
	ContentSHA256   string `json:"content_sha256"`
}

func (ref dsgaCredentialReference) validate() error {
	decoded, err := hex.DecodeString(ref.ContentSHA256)
	if ref.ProtocolVersion != 1 || dsgapi.ValidateID(ref.UID, "credentials Secret") != nil || err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != ref.ContentSHA256 {
		return errDSGACreationPlan
	}
	return nil
}

// 策略来自受信任的平台配置，不是公开创建请求中的可选字段。此值仅在接受前
// 快照；worker只加载已经提交的完整plan，不能重读env选择模式/镜像/内存下限。
type dsgaCreationPolicy struct {
	Namespace       string              `json:"namespace"`
	Image           string              `json:"image"`
	MemorySlotBytes uint64              `json:"memory_slot_bytes"`
	Budget          dsgaBootstrapBudget `json:"budget"`
	VM              dsgaVMProfile       `json:"vm"`
	Storage         dsgaStorageProfile  `json:"storage"`
}

var dsgaPinnedImage = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}$`)
var dsgaPlanUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var dsgaPlanNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var dsgaStorageID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var errDSGACreationPlan = errors.New("DSGA frozen creation plan is invalid or differs from the original intent")

func (b dsgaBootstrapBudget) validate() error {
	const block = uint64(8192)
	const maxBuffers = uint64((1<<31 - 1) / 2)
	for _, size := range []uint64{b.BootstrapBufferBytes, b.MaximumBufferBytes} {
		if size < 16*block || size%block != 0 || size/block > maxBuffers {
			return errDSGACreationPlan
		}
	}
	if b.BootstrapBufferBytes > b.MaximumBufferBytes || b.NonBufferReserveBytes == 0 || b.NonBufferReserveBytes >= b.MinimumGuestMemoryBytes {
		return errDSGACreationPlan
	}
	// 向下取整与Rust/PG的int型KB GUC一致；必须先检查减法，不能uint64下溢或
	// 向上取整侵占reserve。这里通过只证明可表示性，PG实际主共享段仍可能拒绝。
	kb := (b.MinimumGuestMemoryBytes - b.NonBufferReserveBytes) / 1024
	if kb == 0 || kb > 1<<31-1 || b.BootstrapBufferBytes > kb*1024 {
		return errDSGACreationPlan
	}
	return nil
}

func (plan dsgaCreationPlan) validate(p createPayload) error {
	if plan.Credentials != (dsgaCredentialReference{}) && plan.Credentials.validate() != nil {
		return errDSGACreationPlan
	}
	if plan.VM != (dsgaVMProfile{}) && plan.VM.validate() != nil {
		return errDSGACreationPlan
	}
	if plan.Storage != (dsgaStorageProfile{}) && plan.Storage.validate(plan.Namespace) != nil {
		return errDSGACreationPlan
	}
	if plan.ProtocolVersion != 1 || plan.PostgresVersion != 17 || len(plan.Namespace) > 63 || !dsgaPlanNamespace.MatchString(plan.Namespace) ||
		dsgapi.ValidateIncarnation(plan.Incarnation) != nil || dsgapi.ValidateID(plan.ComputeInstanceID, "Compute") != nil ||
		!dsgaPinnedImage.MatchString(plan.Image) || !dsgaPlanUUID.MatchString(plan.OperationUUID) {
		return errDSGACreationPlan
	}
	accepted, err := time.Parse(time.RFC3339Nano, plan.AcceptedAt)
	if err != nil || accepted.Unix() <= 0 || accepted.UTC().Format(time.RFC3339Nano) != plan.AcceptedAt {
		return errDSGACreationPlan
	}
	if p.ProjectID == "" || p.BranchID == "" || p.EndpointID == "" || !dsgaStorageID.MatchString(p.TenantID) || !dsgaStorageID.MatchString(p.TimelineID) ||
		(p.EndpointType != "read_write" && p.EndpointType != "read_only") {
		return errDSGACreationPlan
	}
	// 不沿用实验室createBounds.valid的固定3Gi上限。这里验证NeonVM可表示性和
	// 资源顺序；租户配额/服务规格仍由生产者原有准入在同一事务内检查。
	// 原元数据库的CPU/memory_mib/slot_size_mib列为integer；必须在任何
	// 凭据写入前证明可持久化，不能等VM构造成功后才因SQL int溢出失败。
	if p.MinCPU < 250 || p.MinCPU%250 != 0 || p.MaxCPU < p.MinCPU || p.MaxCPU%250 != 0 || p.MaxCPU > 1<<31-1 ||
		p.MinMem <= 0 || p.MaxMem < p.MinMem || p.MaxMem > 1<<31-1 {
		return errDSGACreationPlan
	}
	minGuest, maxGuest := uint64(p.MinMem)*(1<<20), uint64(p.MaxMem)*(1<<20)
	// 当前NeonVM virtio-mem块为8MiB；CRD的slots上限为512。禁止按MiB整数除法截断，
	// 否则数据库意图与实际Guest内存不一致。未来最低合法Guest也必须落在槽边界。
	slot := plan.MemorySlotBytes
	if slot == 0 || slot%(8<<20) != 0 || minGuest%slot != 0 || maxGuest%slot != 0 || maxGuest/slot > 512 ||
		plan.Budget.MinimumGuestMemoryBytes < slot || plan.Budget.MinimumGuestMemoryBytes%slot != 0 || plan.Budget.MinimumGuestMemoryBytes > minGuest {
		return errDSGACreationPlan
	}
	if err := plan.Budget.validate(); err != nil {
		return err
	}
	if p.ParentTimelineID != "" && (!dsgaStorageID.MatchString(p.ParentTimelineID) || p.ParentTimelineID == p.TimelineID || p.ParentLSN == "") {
		return errDSGACreationPlan
	}
	if p.ParentLSN != "" {
		if p.ParentTimelineID == "" {
			return errDSGACreationPlan
		}
		if _, err := parseLSN(p.ParentLSN); err != nil {
			return errDSGACreationPlan
		}
	}
	return nil
}

func newDSGACreationPlan(p createPayload, policy dsgaCreationPolicy, now time.Time) (dsgaCreationPlan, error) {
	return makeDSGACreationPlan(p, policy, now, rand.Reader)
}

func makeDSGACreationPlan(p createPayload, policy dsgaCreationPolicy, now time.Time, random io.Reader) (dsgaCreationPlan, error) {
	var entropy [48]byte
	if _, err := io.ReadFull(random, entropy[:]); err != nil {
		return dsgaCreationPlan{}, errDSGACreationPlan
	}
	// 三段独立随机性：创建incarnation、Compute实例和原生操作UUID不从endpoint
	// 名字推导；随机失败/非法策略只能在意图提交前拒绝，不返回半个可用plan。
	entropy[32+6] = (entropy[32+6] & 0x0f) | 0x40
	entropy[32+8] = (entropy[32+8] & 0x3f) | 0x80
	uuid := hex.EncodeToString(entropy[32:])
	plan := dsgaCreationPlan{dsgaCreationIdentity: dsgaCreationIdentity{1, 17, policy.Namespace, hex.EncodeToString(entropy[:16]), "ci_" + hex.EncodeToString(entropy[16:32])},
		Image: policy.Image, OperationUUID: fmt.Sprintf("%s-%s-%s-%s-%s", uuid[:8], uuid[8:12], uuid[12:16], uuid[16:20], uuid[20:]),
		AcceptedAt: now.UTC().Format(time.RFC3339Nano), MemorySlotBytes: policy.MemorySlotBytes, Budget: policy.Budget, VM: policy.VM, Storage: policy.Storage}
	if err := plan.validate(p); err != nil {
		return dsgaCreationPlan{}, err
	}
	return plan, nil
}

func (plan dsgaCreationPlan) nativeBootstrap(p createPayload, vmUID string) (dsgaNativeBootstrap, error) {
	if err := plan.validate(p); err != nil {
		return dsgaNativeBootstrap{}, err
	}
	if dsgapi.ValidateID(vmUID, "VM") != nil {
		return dsgaNativeBootstrap{}, errDSGACreationPlan
	}
	return dsgaNativeBootstrap{ProtocolVersion: 2, VMUID: vmUID, ComputeInstanceID: plan.ComputeInstanceID, dsgaBootstrapBudget: plan.Budget}, nil
}

// 读取原Operation整份字节并与已验证binding摘要比对，再严格解析完整plan。
// 不借匿名身份view忽略未来字段；未知plan版本/字段必须等新worker明确支持。
// 返回值无可变引用；加载不持本地锁跨网络，也不会生成/更新任何随机身份。
func (binding *dsgaCreationBinding) creationPlan(ctx context.Context) (dsgaCreationPlan, error) {
	var raw []byte
	if err := binding.server.db.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1`, binding.operationID).Scan(&raw); err != nil {
		return dsgaCreationPlan{}, err
	}
	if len(raw) > 1<<20 || sha256.Sum256(raw) != binding.digest {
		return dsgaCreationPlan{}, errDSGACreationPlan
	}
	var envelope struct {
		Creation json.RawMessage `json:"dsga_creation"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return dsgaCreationPlan{}, errDSGACreationPlan
	}
	var plan dsgaCreationPlan
	decoder := json.NewDecoder(bytes.NewReader(envelope.Creation))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&plan) != nil || plan.dsgaCreationIdentity != binding.intent.Creation || plan.validate(binding.intent.createPayload) != nil {
		return dsgaCreationPlan{}, errDSGACreationPlan
	}
	return plan, nil
}

// 新生产者的数据库入口：与原资源/idempotency事务一起提交，旧worker不能按旧
// action执行。调用者须先完成配额/父资源准入和凭据保留；此函数不访问Kube。
// 目前公开handler尚未切换，待完整Renderer/角色/预算链接齐后才启用。
func addDSGACreateOperation(ctx context.Context, tx pgx.Tx, baseAction, resourceType, resourceID, projectID, operationID, actor, requestID string, p createPayload, plan dsgaCreationPlan, key, hash string, steps []string) error {
	action := baseAction + "_dsga_v1"
	if baseAction == "" || dsgaCreationBaseAction(action) != baseAction || p.ProjectID != projectID || plan.validate(p) != nil {
		return errDSGACreationPlan
	}
	wantedType, wantedID := "project", p.ProjectID
	if baseAction == "create_branch" {
		wantedType, wantedID = "branch", p.BranchID
	} else if baseAction == "create_endpoint" {
		wantedType, wantedID = "endpoint", p.EndpointID
	}
	if resourceType != wantedType || resourceID != wantedID {
		return errDSGACreationPlan
	}
	// 与现有准入一致按父项目先取锁；后面的INSERT触发生命周期/TTL守卫。这里只
	// 插入新Operation，不与已有worker的Operation→绑定行锁链交叉获取同一旧行。
	var version int
	var tenant string
	if err := tx.QueryRow(ctx, `SELECT postgres_version,tenant_id FROM projects WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, projectID).Scan(&version, &tenant); err != nil {
		return err
	}
	if version != int(plan.PostgresVersion) || tenant != p.TenantID {
		return errDSGACreationPlan
	}
	raw, err := json.Marshal(struct {
		createPayload
		Creation dsgaCreationPlan `json:"dsga_creation"`
	}{p, plan})
	if err != nil {
		return err
	}
	return addCreateOperationJSON(ctx, tx, action, resourceType, resourceID, projectID, operationID, actor, requestID, raw, key, hash, steps)
}
