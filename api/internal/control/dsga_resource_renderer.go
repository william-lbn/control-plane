package control

import (
	"encoding/base64"
	"encoding/json"

	"github.com/neondatabase/autoscaling/pkg/dsga/resourceguard"
)

// 与config.json同一次原Render生成，随后由既有typed创建器整体存入同一immutable
// Secret并固定其UID。只有原计划/真实VM UID/原凭据公钥参与，重试不取当前env、
// 不生成新预算引用/epoch、不补写历史配置；没有私钥、verifier或角色口令输出。
// 数字只证明完整可表示且适合原最低Guest，实际Root/Host约束仍由生产执行器落实。
func renderDSGAResourceBootstrap(p createPayload, plan dsgaCreationPlan, vmUID string, credentials dsgaRenderCredentials) ([]byte, error) {
	if _, err := plan.nativeBootstrap(p, vmUID); err != nil {
		return nil, err
	}
	if plan.VM.validateResourceBudget(plan.Budget) != nil {
		return nil, errDSGARender
	}
	s := resourceguard.SourceConfig{ProtocolVersion: 1, Config: resourceguard.Config{VMUID: vmUID, ComputeInstanceID: plan.ComputeInstanceID, StateDir: resourceguard.FixedStateDir}, IssuerEpoch: 1, BudgetCommitmentID: "bootstrap-" + plan.OperationUUID, Limits: *plan.VM.ResourceLimits, ControlPublicKey: base64.RawURLEncoding.EncodeToString(credentials.ControlPublicKey)}
	if s.Validate() != nil {
		return nil, errDSGARender
	}
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > 16*1024 {
		return nil, errDSGARender
	}
	return raw, nil
}

// 两份文件只有同时成功才交创建器，不返回可被调用者误写的半份配置。
func renderDSGABootstrapFiles(p createPayload, plan dsgaCreationPlan, vmUID string, c dsgaRenderCredentials) (map[string][]byte, error) {
	native, err := renderDSGAComputeBootstrap(p, plan, vmUID, c)
	if err != nil {
		return nil, err
	}
	resource, err := renderDSGAResourceBootstrap(p, plan, vmUID, c)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"config.json": native, "resource.json": resource}, nil
}
