package control

import (
	"context"
	"encoding/json"
)

// Reader 消费接受时固定的 Writer 来源，不在 Worker 重试时重新选择“最新 Writer”。
// 来源是不可变创建回执，不是 Writer 活跃租约：接受后原 Writer 可独立暂停、
// 唤醒或退役。Reader 的 VM/Owner/Host 仍由自己的供应链认证和结算。
// 这里只读元数据，锁序沿原 Operation→绑定，不持锁做 Kubernetes/存储请求。
func (binding *dsgaCreationBinding) verifyFrozenReaderSource(ctx context.Context) error {
	p := binding.intent.createPayload
	if binding.action != "create_endpoint_dsga_v1" || p.EndpointType != "read_only" ||
		p.ParentTimelineID != "" || p.ParentLSN != "" || p.RestoreSource != "" {
		return errDSGACreationIdentity
	}
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return err
	}
	if !plan.RetainCatalog {
		return errDSGACreationIdentity
	}
	credentials, err := json.Marshal(plan.Credentials)
	if err != nil {
		return err
	}
	tx, err := binding.lockOperation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT draft->'credential_source' FROM dsga_creation_reservations
 WHERE operation_id=$1 AND accepted_id=$1 AND base_action='create_endpoint'
 AND credential_reference=$2::jsonb`, binding.operationID, credentials).Scan(&raw); err != nil {
		return errDSGACredentialSource
	}
	var frozen dsgaEndpointCredentialSource
	if decodeDSGAObject(raw, &frozen) != nil || !frozen.valid(p.EndpointID) || frozen.Fork != (dsgaRetainedFork{}) {
		return errDSGACredentialSource
	}
	// 不将原 Endpoint 的当前 state 当作历史来源身份。源的原创建必须真正结案且
	// 已绑定配置；跨项目/分支/Timeline、错角色或仅同名的来源均不能被收养。
	if err = tx.QueryRow(ctx, `SELECT o.payload#>'{dsga_creation,credentials}'
 FROM operations o JOIN endpoints e ON e.id=o.payload->>'endpoint_id' AND e.project_id=o.project_id
 JOIN dsga_creation_bindings b ON b.operation_id=o.id AND b.vm_uid IS NOT NULL AND b.config_uid IS NOT NULL
 WHERE o.id=$1 AND o.project_id=$2 AND e.id=$3 AND e.branch_id=$4 AND e.endpoint_type='read_write'
 AND o.action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1')
 AND o.state='succeeded' AND o.finished_at IS NOT NULL AND o.lease_owner IS NULL AND o.lease_expires_at IS NULL
 AND o.payload->>'project_id'=$2 AND o.payload->>'branch_id'=$4 AND o.payload->>'endpoint_type'='read_write'
 AND o.payload->>'tenant_id'=$5 AND o.payload->>'timeline_id'=$6
 AND o.payload#>'{dsga_creation,postgres_version}'=to_jsonb($7::integer)`, frozen.Operation, p.ProjectID, frozen.Endpoint,
		p.BranchID, p.TenantID, p.TimelineID, int(plan.PostgresVersion)).Scan(&raw); err != nil {
		return errDSGACredentialSource
	}
	var original dsgaCredentialReference
	if decodeDSGAObject(raw, &original) != nil || original != frozen.Reference {
		return errDSGACredentialSource
	}
	return binding.checkLease(ctx, tx)
}
