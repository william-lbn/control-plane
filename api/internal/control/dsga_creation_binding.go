package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	core "k8s.io/api/core/v1"
)

// 此view只解释绑定所需的身份；完整启动预算/镜像/角色由版本化生产者和Renderer
// 的原生契约校验。指纹覆盖原始jsonb全部字节，包括本view不解释的预算字段，
// 不将payload转成map/float64后重写；绑定事实独立存表，原意图始终保持原样。
type dsgaCreationEnvelope struct {
	createPayload
	Creation struct {
		ProtocolVersion   uint32 `json:"protocol_version"`
		PostgresVersion   uint32 `json:"postgres_version"`
		Namespace         string `json:"namespace"`
		Incarnation       string `json:"incarnation"`
		ComputeInstanceID string `json:"compute_instance_id"`
	} `json:"dsga_creation"`
}

type dsgaCreationBinding struct {
	server                *server
	operationID, workerID string
	action                string
	intent                dsgaCreationEnvelope
	digest                [32]byte
	vmUID, configUID      string
}

func dsgaCreationBaseAction(action string) string {
	switch action {
	case "create_project_dsga_v1":
		return "create_project"
	case "create_branch_dsga_v1":
		return "create_branch"
	case "create_endpoint_dsga_v1":
		return "create_endpoint"
	default:
		return ""
	}
}

// 锁序只有Operation行→该Operation的绑定行；不在持锁时访问Kube/Secret或锁父
// 项目/分支，也不UPDATE Operation.payload触发父准入锁。父生命周期读取只用于
// 保守拒绝，不声称跨数据库/Kubernetes原子授权；最终执行仍须独立fence。
// 行锁取得后再单独读取clock_timestamp()核验lease，避免排队期间now()冻结在
// 事务开始时间而错误放过已到期worker。提交前再次检查，失败回滚所有新绑定。
func (binding *dsgaCreationBinding) lockOperation(ctx context.Context) (pgx.Tx, error) {
	tx, err := binding.server.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tx.Rollback(ctx)
		}
	}()
	var action, project, resourceType, resourceID string
	var payload []byte
	err = tx.QueryRow(ctx, `SELECT action,project_id,resource_type,resource_id,payload
 FROM operations WHERE id=$1 FOR UPDATE`, binding.operationID).Scan(&action, &project, &resourceType, &resourceID, &payload)
	if err != nil {
		return nil, err
	}
	if err := binding.checkLease(ctx, tx); err != nil {
		return nil, err
	}
	if action != binding.action || sha256.Sum256(payload) != binding.digest || project != binding.intent.ProjectID {
		return nil, errDSGACreationIdentity
	}
	base := dsgaCreationBaseAction(action)
	wantedType, wantedID := "project", binding.intent.ProjectID
	if base == "create_branch" {
		wantedType, wantedID = "branch", binding.intent.BranchID
	} else if base == "create_endpoint" {
		wantedType, wantedID = "endpoint", binding.intent.EndpointID
	}
	if base == "" || resourceType != wantedType || resourceID != wantedID {
		return nil, errDSGACreationIdentity
	}
	var tenant, timeline, projectState, branchState string
	var version int
	err = tx.QueryRow(ctx, `SELECT p.tenant_id,p.postgres_version,p.state,b.timeline_id,b.state
 FROM projects p JOIN branches b ON b.project_id=p.id
 WHERE p.id=$1 AND b.id=$2 AND p.deleted_at IS NULL AND b.deleted_at IS NULL`, project, binding.intent.BranchID).
		Scan(&tenant, &version, &projectState, &timeline, &branchState)
	if err != nil {
		return nil, err
	}
	if version != 17 || tenant != binding.intent.TenantID || timeline != binding.intent.TimelineID ||
		(projectState != "ready" && (base != "create_project" || (projectState != "provisioning" && projectState != "error"))) ||
		(branchState != "ready" && (base == "create_endpoint" || (branchState != "creating" && branchState != "error"))) {
		return nil, errDSGACreationIdentity
	}
	var endpointBranch, endpointState string
	err = tx.QueryRow(ctx, `SELECT branch_id,state FROM endpoints WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, binding.intent.EndpointID, project).
		Scan(&endpointBranch, &endpointState)
	if err != nil {
		return nil, err
	}
	if endpointBranch != binding.intent.BranchID || (endpointState != "provisioning" && endpointState != "active" && endpointState != "error") {
		return nil, errDSGACreationIdentity
	}
	ok = true
	return tx, nil
}

func (binding *dsgaCreationBinding) checkLease(ctx context.Context, tx pgx.Tx) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT state='running' AND lease_owner=$2 AND lease_expires_at>clock_timestamp()
 FROM operations WHERE id=$1`, binding.operationID, binding.workerID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errLeaseLost
	}
	return nil
}

func (s *server) loadDSGACreationBinding(ctx context.Context, operationID, workerID string) (*dsgaCreationBinding, error) {
	if s == nil || s.db == nil || s.kube == nil || operationID == "" || workerID == "" {
		return nil, errDSGACreationIdentity
	}
	var action string
	var payload []byte
	if err := s.db.QueryRow(ctx, `SELECT action,payload FROM operations WHERE id=$1`, operationID).Scan(&action, &payload); err != nil {
		return nil, err
	}
	var intent dsgaCreationEnvelope
	// 先做有限大小的身份解析，再在行锁内重读全部原始payload并核对摘要。
	// 首次读取本身不授予创建权；两次读取间的改写、换主和父生命周期变化都要拒绝。
	if len(payload) > 1<<20 || json.Unmarshal(payload, &intent) != nil || dsgaCreationBaseAction(action) == "" || intent.ProjectID == "" || intent.BranchID == "" || intent.EndpointID == "" ||
		intent.Creation.ProtocolVersion != 1 || intent.Creation.PostgresVersion != 17 || intent.Creation.Namespace != s.kube.namespace ||
		dsgapi.ValidateIncarnation(intent.Creation.Incarnation) != nil || dsgapi.ValidateID(intent.Creation.ComputeInstanceID, "Compute") != nil {
		return nil, errDSGACreationIdentity
	}
	binding := &dsgaCreationBinding{server: s, operationID: operationID, workerID: workerID, action: action, intent: intent, digest: sha256.Sum256(payload), vmUID: "", configUID: ""}
	tx, err := binding.lockOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	vmUID, configUID, err := binding.read(ctx, tx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		binding.vmUID, binding.configUID = vmUID, configUID
	}
	if err := binding.checkLease(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return binding, nil
}

func (binding *dsgaCreationBinding) read(ctx context.Context, tx pgx.Tx) (string, string, error) {
	var namespace, name, incarnation, compute, vm string
	var config *string
	var digest []byte
	err := tx.QueryRow(ctx, `SELECT namespace,vm_name,incarnation,compute_instance_id,intent_digest,vm_uid,config_uid
 FROM dsga_creation_bindings WHERE operation_id=$1`, binding.operationID).Scan(&namespace, &name, &incarnation, &compute, &digest, &vm, &config)
	if err != nil {
		return "", "", err
	}
	if namespace != binding.intent.Creation.Namespace || name != kubeName(binding.intent.EndpointID) || incarnation != binding.intent.Creation.Incarnation ||
		compute != binding.intent.Creation.ComputeInstanceID || !bytes.Equal(digest, binding.digest[:]) {
		return "", "", errDSGACreationIdentity
	}
	cfg := ""
	if config != nil {
		cfg = *config
	}
	return vm, cfg, nil
}

func (binding *dsgaCreationBinding) bindVM(ctx context.Context, vm *vmv1.VirtualMachine) error {
	// 回调接收真实API读回对象；只信任原Operation固定的命名空间、标签和随机身份。
	// held与ready都可按原事实恢复，但删除对象、坏bootstrap与同名新UID不能收养。
	if vm == nil || vm.Namespace != binding.intent.Creation.Namespace || vm.Name != kubeName(binding.intent.EndpointID) || vm.ResourceVersion == "" ||
		vm.Labels["neon-control/project-id"] != binding.intent.ProjectID || vm.Labels["neon-control/endpoint-id"] != binding.intent.EndpointID || dsgapi.ValidateID(string(vm.UID), "VM") != nil ||
		vm.Spec.DSGABootstrap == nil || vm.Spec.DSGABootstrap.ProtocolVersion != 1 || vm.Spec.DSGABootstrap.Incarnation != binding.intent.Creation.Incarnation || vm.Spec.DSGABootstrap.ComputeInstanceID != binding.intent.Creation.ComputeInstanceID {
		return errDSGACreationIdentity
	}
	if _, err := vm.DSGABootstrapHeld(); err != nil || vm.DeletionTimestamp != nil {
		return errDSGACreationIdentity
	}
	return binding.bind(ctx, string(vm.UID), "")
}

func (binding *dsgaCreationBinding) bindConfig(ctx context.Context, secret *core.Secret) error {
	// 配置身份与同一VM控制者绑定；原生v2身份必须一致。完整Data的确定性、预算
	// 和角色检查由调用链Renderer/创建执行器承担，此处不把身份view当可启动spec。
	if !dsgaOwnedSecret(secret, binding.intent.Creation.Namespace, kubeName(binding.intent.EndpointID)+"-config", binding.intent.ProjectID, binding.intent.EndpointID) ||
		dsgapi.ValidateID(string(secret.UID), "config Secret") != nil || secret.Immutable == nil || !*secret.Immutable || secret.Type != core.SecretTypeOpaque || len(secret.OwnerReferences) != 1 {
		return errDSGACreationIdentity
	}
	ref := secret.OwnerReferences[0]
	if ref.APIVersion != vmv1.SchemeGroupVersion.String() || ref.Kind != "VirtualMachine" || ref.Name != kubeName(binding.intent.EndpointID) || ref.Controller == nil || !*ref.Controller || dsgapi.ValidateID(string(ref.UID), "VM") != nil {
		return errDSGACreationIdentity
	}
	var identity dsgaCredentialConfig
	if json.Unmarshal(secret.Data["config.json"], &identity) != nil || identity.Spec.Dynamic == nil || identity.Spec.Dynamic.ProtocolVersion != 2 ||
		identity.Spec.Dynamic.VMUID != string(ref.UID) || identity.Spec.Dynamic.ComputeInstanceID != binding.intent.Creation.ComputeInstanceID {
		return errDSGACreationIdentity
	}
	return binding.bind(ctx, string(ref.UID), string(secret.UID))
}

func (binding *dsgaCreationBinding) bind(ctx context.Context, vmUID, configUID string) error {
	tx, err := binding.lockOperation(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	storedVM, storedConfig, err := binding.read(ctx, tx)
	// 同一Operation行锁串行化首次观察，避免两个worker分别INSERT后以冲突兜底
	// 覆盖身份。VM必须先落库，Config只允许空值推进一次；旧值相同完全不UPDATE，
	// 防止幂等恢复改变审计时间/xmin，也不允许删除事实后重新选择另一UID。
	if errors.Is(err, pgx.ErrNoRows) {
		if configUID != "" {
			return errDSGACreationIdentity
		}
		_, err = tx.Exec(ctx, `INSERT INTO dsga_creation_bindings(operation_id,namespace,vm_name,incarnation,compute_instance_id,intent_digest,vm_uid)
 VALUES($1,$2,$3,$4,$5,$6,$7)`, binding.operationID, binding.intent.Creation.Namespace, kubeName(binding.intent.EndpointID), binding.intent.Creation.Incarnation, binding.intent.Creation.ComputeInstanceID, binding.digest[:], vmUID)
	} else if err == nil {
		if storedVM != vmUID || (configUID != "" && storedConfig != "" && storedConfig != configUID) {
			return errDSGACreationIdentity
		}
		if configUID != "" && storedConfig == "" {
			_, err = tx.Exec(ctx, `UPDATE dsga_creation_bindings SET config_uid=$2,updated_at=clock_timestamp() WHERE operation_id=$1 AND config_uid IS NULL`, binding.operationID, configUID)
		}
	}
	if err != nil {
		return err
	}
	if err := binding.checkLease(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
