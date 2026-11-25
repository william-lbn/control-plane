package control

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type dsgaBranchFunctionError struct {
	status        int
	code, message string
}

func (e dsgaBranchFunctionError) Error() string { return e.code }

// 保留 DSGA 原默认 omit，新增显式选择；接受时冻结到原 Operation，不在
// Writer 出生后重新读取父分支的最新代码，也不默认拷贝环境秘密。
func (s *server) dsgaBranchFunctionPolicy(body branchCreateRequest) (string, error) {
	policy := body.FunctionCopyPolicy
	if policy == "" {
		policy = "omit"
	}
	if policy != "omit" && policy != "inherit_paused" {
		return "", dsgaBranchFunctionError{422, "invalid_function_copy_policy", "Choose inherit_paused or omit for branch Functions"}
	}
	if policy == "inherit_paused" && !s.functionBranchCopyEnabled() {
		return "", dsgaBranchFunctionError{503, "function_branch_copy_disabled", "Operator must enable verified branch Function preparation"}
	}
	return policy, nil
}

// 调用者已持原项目/父分支生命周期锁。历史 SQL 只固定数据时间，Functions
// 仍取本次准入时的确切代码/环境，必须另行确认，不能暗示历史代码已恢复。
func checkDSGABranchFunctionConsent(ctx context.Context, tx pgx.Tx, project string, body branchCreateRequest, historical bool) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM function_definitions
 WHERE project_id=$1 AND branch_id=$2 AND state NOT IN ('deleting','deleted'))`, project, body.ParentBranchID).Scan(&exists); err != nil {
		return err
	}
	historical = historical || body.ParentLSN != "" || body.ParentTimestamp != ""
	if exists && (!body.AcknowledgeFunctions || (historical && !body.AcknowledgeCurrentCode)) {
		return dsgaBranchFunctionError{422, "function_copy_acknowledgment_required", "Confirm the branch environment copy; historical SQL restore additionally requires acknowledging current accepted Function code rather than historical code"}
	}
	return nil
}

// 这里只准入暂停副本的元数据；实际 SQL/Secret/Guest 副作用仍由原 Functions
// Worker 逐次验证。DSGA 分支不能把一个 legacy 或尚未完成的 Writer 当作来源。
// 只读核对原记录，不在持项目/分支锁时发网络请求或另取 Operation 写锁。
func (s *server) verifyDSGAFunctionBranchWriter(ctx context.Context, tx pgx.Tx, project, branch, timeline, writer string) error {
	runtime, err := readDSGAEndpointRuntime(ctx, tx, project, writer)
	if err != nil {
		return err
	}
	if runtime == nil || runtime.Payload.BranchID != branch || runtime.Payload.TimelineID != timeline ||
		runtime.Payload.EndpointType != "read_write" || !runtime.Plan.RetainCatalog || runtime.VMUID == "" || runtime.ConfigUID == "" {
		return errFunctionCopy
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoints e
 JOIN operations o ON o.id=$4 AND o.project_id=e.project_id
 JOIN dsga_creation_bindings b ON b.operation_id=o.id
 WHERE e.id=$3 AND e.project_id=$1 AND e.branch_id=$2 AND e.endpoint_type='read_write'
 AND e.workload_kind='neonvm' AND e.state IN ('active','suspended') AND e.deleted_at IS NULL
 AND o.resource_type='endpoint' AND o.resource_id=e.id AND o.state='succeeded'
 AND ((e.dsga_runtime_operation_id IS NULL AND o.action='create_endpoint_dsga_v1') OR
      (e.dsga_runtime_operation_id=o.id AND o.action='resume_endpoint_dsga_v1'))
 AND b.vm_uid=$5 AND b.config_uid=$6
 AND EXISTS(SELECT 1 FROM operation_steps st WHERE st.operation_id=o.id AND st.name='sql_probe_via_proxy' AND st.state='succeeded')
 AND EXISTS(SELECT 1 FROM operation_steps st WHERE st.operation_id=o.id AND st.name='persist_ready_state' AND st.state='succeeded')
 AND NOT EXISTS(SELECT 1 FROM operation_steps st WHERE st.operation_id=o.id AND st.state<>'succeeded')
 AND b.intent_digest=pg_catalog.sha256(pg_catalog.convert_to(o.payload::text,'UTF8')))`,
		project, branch, writer, runtime.Operation, runtime.VMUID, runtime.ConfigUID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errFunctionCopy
	}
	return nil
}
