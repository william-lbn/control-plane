package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

var errDSGATimelineUnconfirmed = errors.New("original confirmed DSGA timeline cannot be confirmed; automatic recreation denied")

// 与官方Bootstrap/Branch请求对应；结构只保留本操作允许使用的字段。祖先LSN
// 来自原计划，不在恢复时重新选择最新LSN；branch实际PG版本仍须读取祖先核验，
// 因为官方Branch模式的pg_version兼容字段并不覆盖祖先版本。
type dsgaTimelineRequest struct {
	TimelineID       string `json:"new_timeline_id"`
	PostgresVersion  uint32 `json:"pg_version"`
	ParentTimelineID string `json:"ancestor_timeline_id,omitempty"`
	ParentLSN        string `json:"ancestor_start_lsn,omitempty"`
}

func (binding *dsgaCreationBinding) timelineRequest(plan dsgaCreationPlan) (dsgaTimelineRequest, error) {
	p := binding.intent.createPayload
	if plan.validate(p) != nil || p.EndpointType != "read_write" ||
		(binding.action != "create_project_dsga_v1" && binding.action != "create_branch_dsga_v1") ||
		(binding.action == "create_project_dsga_v1" && (p.ParentTimelineID != "" || p.RestoreSource != "")) ||
		(binding.action == "create_branch_dsga_v1" && p.ParentTimelineID == "") {
		return dsgaTimelineRequest{}, errDSGACreationIdentity
	}
	return dsgaTimelineRequest{p.TimelineID, plan.PostgresVersion, p.ParentTimelineID, p.ParentLSN}, nil
}

// 原Operation→timeline记录，SQL锁不跨任何HTTP。首次先保留完整请求而非声称
// 已发送/成功；未知写入仍留原请求供显式对账，绝不改ID/祖先/LSN再试。唯一可变
// 字段是首次确认时间，确认后恢复只读，不改xmin；失去lease不能写入确认。
func (binding *dsgaCreationBinding) timelineIdentity(ctx context.Context, request dsgaTimelineRequest, confirm bool) (bool, error) {
	tx, err := binding.lockOperation(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenant, timeline string
	var raw []byte
	var confirmed bool
	err = tx.QueryRow(ctx, `SELECT tenant_id,timeline_id,request,confirmed_at IS NOT NULL FROM dsga_creation_timelines WHERE operation_id=$1 FOR UPDATE`, binding.operationID).Scan(&tenant, &timeline, &raw, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) && !confirm {
		raw, err = json.Marshal(request)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO dsga_creation_timelines(operation_id,tenant_id,timeline_id,request) VALUES($1,$2,$3,$4::jsonb)`, binding.operationID, binding.intent.TenantID, request.TimelineID, string(raw))
		}
	} else if err == nil {
		var stored dsgaTimelineRequest
		if tenant != binding.intent.TenantID || timeline != request.TimelineID || decodeDSGAObject(raw, &stored) != nil || stored != request {
			return false, errDSGACreationIdentity
		}
		if confirm && !confirmed {
			_, err = tx.Exec(ctx, `UPDATE dsga_creation_timelines SET confirmed_at=clock_timestamp() WHERE operation_id=$1 AND confirmed_at IS NULL`, binding.operationID)
			confirmed = err == nil
		}
	}
	if err != nil {
		return false, err
	}
	if err = binding.checkLease(ctx, tx); err != nil {
		return false, err
	}
	return confirmed, tx.Commit(ctx)
}

func (binding *dsgaCreationBinding) prepareTimeline(ctx context.Context) error {
	return binding.prepareTimelineWithStorage(ctx, nil)
}

// 只读控制器正式全分片聚合接口，移除固定单pageserver的旧候选路径。首次与未知
// 恢复都先登记原请求，再GET观察；503从不解释为不存在。对未确认原意图，只在
// 官方创建接口持久幂等（Bootstrap版本或Branch祖先/LSN完全相同）的前提下显式
// 重放一次。确认后任何读取失败都保留原事实，不能重建已确认后丢失的空timeline。
// 这不授予清理/删除许可；旧HTTP可能迟到，不能靠lease过期断言远端已取消。
func (binding *dsgaCreationBinding) prepareTimelineWithStorage(ctx context.Context, storage *dsgaStorageClient) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	current, err := binding.server.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
	if err != nil {
		return err
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		return err
	}
	request, err := current.timelineRequest(plan)
	if err != nil {
		return err
	}
	if plan.Storage.validate(plan.Namespace) != nil {
		return errDSGAStorageManagement
	}
	if current.action == "create_project_dsga_v1" {
		known, err := current.tenantIdentity(ctx, false)
		if err != nil {
			return err
		}
		if !known {
			return errDSGACreationIdentity
		}
	}
	confirmed, err := current.timelineIdentity(ctx, request, false)
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
	p := current.intent.createPayload
	path := "control/v1/tenant/" + p.TenantID + "/timeline/" + p.TimelineID
	fact, err := storage.request(ctx, http.MethodGet, path, nil)
	if err == nil {
		if err = validateDSGATimelineAggregate(fact, p.TenantID, p.TimelineID, plan.PostgresVersion, p.ParentTimelineID, p.ParentLSN, true); err != nil {
			return err
		}
		_, err = current.timelineIdentity(ctx, request, true)
		return err
	}
	if confirmed {
		return errors.Join(errDSGATimelineUnconfirmed, err)
	}
	var status dsgaStorageHTTPError
	if !errors.As(err, &status) || (status.code != http.StatusNotFound && status.code != http.StatusServiceUnavailable) {
		return err
	}
	// 允许重放的依据是已保留的原创建意图与原生强幂等，不是404/503“证明缺失”。
	// 每次写前仍实际读取原Tenant/祖先；鉴权、限流、解析或其他失败不会触发POST。
	tenant, err := storage.request(ctx, http.MethodGet, "control/v1/tenant/"+p.TenantID, nil)
	if err != nil {
		return err
	}
	if err = validateDSGATenant(tenant, p.TenantID); err != nil {
		return err
	}
	if p.ParentTimelineID != "" {
		parent, err := storage.request(ctx, http.MethodGet, "control/v1/tenant/"+p.TenantID+"/timeline/"+p.ParentTimelineID, nil)
		if err != nil {
			return err
		}
		if err = validateDSGATimelineAggregate(parent, p.TenantID, p.ParentTimelineID, plan.PostgresVersion, "", "", false); err != nil {
			return err
		}
	}
	// 排队/祖先HTTP可跨过lease边界。此处再按原payload/lease读锁对账，仍不持锁
	// 跨POST；无法原子覆盖网络窗口，故未知状态不能被后续清理误认为未执行。
	if _, err = current.timelineIdentity(ctx, request, false); err != nil {
		return err
	}
	_, err = storage.request(ctx, http.MethodPost, "v1/tenant/"+p.TenantID+"/timeline", request)
	if err != nil && (!errors.As(err, &status) || status.code != http.StatusConflict) {
		return err
	}
	fact, err = storage.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if err = validateDSGATimelineAggregate(fact, p.TenantID, p.TimelineID, plan.PostgresVersion, p.ParentTimelineID, p.ParentLSN, true); err != nil {
		return err
	}
	_, err = current.timelineIdentity(ctx, request, true)
	return err
}

// TenantTimelineDescribeResponse.shards中的tenant_id实际为TenantShardId。
// 验证完整同租户、无重/无缺的集合，再逐片验证PG/祖先/状态；不把shard0
// 或POST返回的一片当全量成功，也不冻结后续可合法变化的分片拓扑。
func validateDSGATimelineAggregate(item map[string]any, tenant, timeline string, version uint32, parent, lsn string, ancestry bool) error {
	raw, err := json.Marshal(item["shards"])
	var shards []map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &shards) != nil || len(shards) == 0 || len(shards) > 255 {
		return errDSGACreationIdentity
	}
	identities := make([]any, 0, len(shards))
	for _, shard := range shards {
		var id, actualTimeline, state string
		var pg uint32
		if json.Unmarshal(shard["tenant_id"], &id) != nil || json.Unmarshal(shard["timeline_id"], &actualTimeline) != nil || actualTimeline != timeline ||
			json.Unmarshal(shard["pg_version"], &pg) != nil || pg != version || json.Unmarshal(shard["state"], &state) != nil || state != "Active" {
			return errDSGACreationIdentity
		}
		identities = append(identities, map[string]any{"tenant_shard_id": id})
		if ancestry {
			if parent == "" {
				// 官方Option的None为null；缺字段/空字符串不构成根timeline证明。
				if !bytes.Equal(bytes.TrimSpace(shard["ancestor_timeline_id"]), []byte("null")) || !bytes.Equal(bytes.TrimSpace(shard["ancestor_lsn"]), []byte("null")) {
					return errDSGACreationIdentity
				}
			} else {
				var actualParent, actualLSN string
				if json.Unmarshal(shard["ancestor_timeline_id"], &actualParent) != nil || actualParent != parent || json.Unmarshal(shard["ancestor_lsn"], &actualLSN) != nil {
					return errDSGACreationIdentity
				}
				a, err := parseLSN(actualLSN)
				b, expectedErr := parseLSN(lsn)
				if err != nil || expectedErr != nil || a != b {
					return errDSGACreationIdentity
				}
			}
		}
	}
	if !dsgaStorageID.MatchString(timeline) || version != 17 {
		return errDSGACreationIdentity
	}
	return validateDSGATenant(map[string]any{"tenant_id": tenant, "shards": identities}, tenant)
}
