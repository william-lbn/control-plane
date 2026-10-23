package control

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var errDSGATenantUnconfirmed = errors.New("original confirmed DSGA Tenant cannot be confirmed; automatic recreation denied")

// 仅记录“原Tenant存在”，不冻结控制器可以合法调整的shard拓扑，不宣称存储、
// Guest或SQL已就绪。Operation→Tenant事实；恢复同源完全只读，不改xmin/时间。
func (binding *dsgaCreationBinding) tenantIdentity(ctx context.Context, confirm bool) (bool, error) {
	tx, err := binding.lockOperation(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if binding.action != "create_project_dsga_v1" {
		return false, errDSGACreationIdentity
	}
	var tenant string
	err = tx.QueryRow(ctx, `SELECT tenant_id FROM dsga_creation_tenants WHERE operation_id=$1 FOR SHARE`, binding.operationID).Scan(&tenant)
	known := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
		if confirm {
			_, err = tx.Exec(ctx, `INSERT INTO dsga_creation_tenants(operation_id,tenant_id) VALUES($1,$2)`, binding.operationID, binding.intent.TenantID)
			known = err == nil
		}
	} else if err == nil && tenant != binding.intent.TenantID {
		err = errDSGACreationIdentity
	}
	if err != nil {
		return false, err
	}
	if err := binding.checkLease(ctx, tx); err != nil {
		return false, err
	}
	return known, tx.Commit(ctx)
}

// prepareTenant只供版本化根项目创建使用，真实API的租户UUID来自原不可变计划。
// 原事实独立于可被重试改回running的operation_steps；已确认后404一律拒绝，
// 不能自动重建空Tenant。首次POST未知结果只交下一次原意图GET，不透明重发。
// 先读可信controller，再SQL确认；SQL锁不跨网络，丢COMMIT也只读原事实恢复。
func (binding *dsgaCreationBinding) prepareTenant(ctx context.Context) error {
	return binding.prepareTenantWithStorage(ctx, nil)
}

// 私有入口允许模块测试提供同原计划绑定的专用TLS客户端；生产入口始终由可信
// Kube快照构造。这里不接受request函数/调用方URL，也不沿用SA服务代理作为原生
// 存储JWT。任何路径都重载原Operation及lease，HTTP期间不持元数据库锁。
func (binding *dsgaCreationBinding) prepareTenantWithStorage(ctx context.Context, storage *dsgaStorageClient) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	current, err := binding.server.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
	if err != nil {
		return err
	}
	plan, err := current.creationPlan(ctx)
	p := current.intent.createPayload
	if err != nil || current.action != "create_project_dsga_v1" || plan.validate(p) != nil || p.EndpointType != "read_write" || p.ParentTimelineID != "" || p.RestoreSource != "" {
		return errDSGACreationIdentity
	}
	if plan.Storage.validate(plan.Namespace) != nil {
		return errDSGAStorageManagement
	}
	known, err := current.tenantIdentity(ctx, false)
	if err != nil {
		return err
	}
	if storage == nil {
		storage, err = newDSGAStorageClient(ctx, current.server.kube, plan.Storage)
		if err != nil {
			return err
		}
		defer storage.CloseIdleConnections()
	}
	if storage.profile != plan.Storage || storage.namespace != plan.Namespace {
		return errDSGAStorageManagement
	}
	path := "control/v1/tenant/" + p.TenantID
	fact, err := storage.request(ctx, http.MethodGet, path, nil)
	if apierrors.IsNotFound(err) {
		if known {
			// 404也可能来自服务依赖，不擅自宣称数据已丢失；无论哪种
			// 情况都只能保留原事实等待原目标恢复，绝不重建同ID空Tenant。
			return errDSGATenantUnconfirmed
		}
		_, err = storage.request(ctx, http.MethodPost, "v1/tenant", struct {
			TenantID string `json:"new_tenant_id"`
		}{p.TenantID})
		if err != nil {
			// controller的409只表示冲突，不能证明原创建意图已成功。
			// 只按有类型的实际409进入GET对账；即使返回409，也必须
			// 核对同一tenant及完整shard集合，不靠错误正文猜测执行结果。
			var status apierrors.APIStatus
			if !errors.As(err, &status) || status.Status().Code != http.StatusConflict {
				return err
			}
		}
		fact, err = storage.request(ctx, http.MethodGet, path, nil)
	}
	if err != nil {
		return err
	}
	if err := validateDSGATenant(fact, p.TenantID); err != nil {
		return err
	}
	_, err = current.tenantIdentity(ctx, true)
	return err
}

// 官方TenantDescribeResponse允许背景分片/迁移，不能把固定单pageserver当Tenant
// 身份。核对完整无重/无缺的同租户shard集合，兼容旧unsharded与正式count=1。
// attached/reconciling等状态不在此冒充ready；后续timeline/Compute仍须各自证明。
func validateDSGATenant(item map[string]any, tenant string) error {
	described, ok := item["tenant_id"].(string)
	raw, err := json.Marshal(item["shards"])
	var shards []map[string]json.RawMessage
	if err != nil || !ok || described != tenant || !dsgaStorageID.MatchString(tenant) || json.Unmarshal(raw, &shards) != nil || len(shards) == 0 || len(shards) > 255 {
		return errDSGACreationIdentity
	}
	seen := map[string]bool{}
	for _, shard := range shards {
		var id string
		// 只接受官方精确字段名，避免encoding/json大小写折叠把其他字段当身份。
		if json.Unmarshal(shard["tenant_shard_id"], &id) != nil || seen[id] {
			return errDSGACreationIdentity
		}
		seen[id] = true
		if id == tenant {
			if len(shards) != 1 {
				return errDSGACreationIdentity
			}
			continue
		}
		if len(id) != 37 || !strings.HasPrefix(id, tenant+"-") {
			return errDSGACreationIdentity
		}
		slug := id[33:]
		parts, err := hex.DecodeString(slug)
		if err != nil || hex.EncodeToString(parts) != slug || len(parts) != 2 || parts[1] == 0 || int(parts[1]) != len(shards) || parts[0] >= parts[1] {
			return errDSGACreationIdentity
		}
	}
	return nil
}
