package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// 失败只携带固定阶段；不得把HTTP正文、签名、verifier或凭据带到CLI日志。
type managedSQLStageError struct{ stage string }

func (e managedSQLStageError) Error() string { return "managed_sql_route_" + e.stage }
func (e managedSQLStageError) Unwrap() error { return errManagedSQLPublish }
func managedSQLRefuse(stage string) error    { return managedSQLStageError{stage: stage} }

// ManagedSQLRouteFailureCode仅输出producer自己定义的固定枚举。
func ManagedSQLRouteFailureCode(err error) string {
	var stage managedSQLStageError
	if errors.As(err, &stage) {
		switch stage.stage {
		case "input", "source", "native", "registry", "existing_binding", "replacement_identity", "old_lease_present", "old_lease_unknown", "old_pods_present", "old_pods_unknown", "policy", "roles", "selected_route", "authority", "source_recheck", "encode", "put_unconfirmed", "put_readback", "conflict_exhausted":
			return stage.Error()
		}
	}
	return "managed_sql_route_refused"
}

// 只允许同一数据与逻辑身份的物理替换，不改变公共Eq或Adapter唤醒的严格比较。
// 同UID的Config/凭据漂移不属于物理替换。三个新Config输入必须确实随新实例生成。
func managedSQLReplacementIdentity(old, next adapter.ManagedSQLBinding) bool {
	return old.ServiceUID == next.ServiceUID && managedSQLSuccessorIdentity(old, next)
}

func managedSQLSuccessorIdentity(old, next adapter.ManagedSQLBinding) bool {
	if old.Validate() != nil || next.Validate() != nil || old.VMUID == next.VMUID ||
		old.Incarnation == next.Incarnation || old.ConfigSecretUID == next.ConfigSecretUID ||
		old.ConfigSHA256 == next.ConfigSHA256 || old.SecretDataSHA256 == next.SecretDataSHA256 {
		return false
	}
	return old.ProtocolVersion == next.ProtocolVersion && old.Namespace == next.Namespace &&
		old.ProjectID == next.ProjectID && old.EndpointID == next.EndpointID && old.BranchID == next.BranchID &&
		old.TenantID == next.TenantID && old.TimelineID == next.TimelineID &&
		old.ComputeInstanceID == next.ComputeInstanceID &&
		old.CredentialSecretUID == next.CredentialSecretUID &&
		old.CredentialDataSHA256 == next.CredentialDataSHA256
}

// A resume may replace the VM-owned Service. The caller must match this state
// with the completed suspension and the Service bound by the current Worker.
// The public publisher cannot supply this process-local proof.
type managedSQLResumeProof struct {
	OriginalVMUID, OriginalConfigUID, OriginalServiceUID string
	Verify                                               func(context.Context, adapter.ManagedSQLBinding) error
}

func (p *managedSQLResumeProof) matches(old, next adapter.ManagedSQLBinding) bool {
	return p != nil && p.Verify != nil && managedSQLSuccessorIdentity(old, next) &&
		old.VMUID == p.OriginalVMUID && old.ConfigSecretUID == p.OriginalConfigUID && old.ServiceUID == p.OriginalServiceUID &&
		old.ServiceUID != next.ServiceUID
}

// 当前同名VM及sealed Config已由readManagedSQLRouteSource确认成新UID。
// Kubernetes同namespace/name唯一，因此旧UID的VM不能与它并存；仍必须证明旧Owner已退出。
// Lease存在（即使过期/删除中）或任何旧OwnerUID Pod存在（含终态）均拒绝，不负责清理。
func managedSQLReplacementRetired(ctx context.Context, k *kubeClient, old adapter.ManagedSQLBinding) error {
	name := kubeName(old.EndpointID)
	lease := journal.LeaseName(journal.Binding{Namespace: old.Namespace, Name: name, VMUID: types.UID(old.VMUID), Incarnation: old.Incarnation})
	path := "/apis/coordination.k8s.io/v1/namespaces/" + url.PathEscape(old.Namespace) + "/leases/" + url.PathEscape(lease)
	_, err := k.request(ctx, http.MethodGet, path, nil)
	if err == nil {
		return managedSQLRefuse("old_lease_present")
	}
	var ke kubeError
	if !errors.As(err, &ke) || ke.Status != http.StatusNotFound {
		return managedSQLRefuse("old_lease_unknown")
	}

	// 不用label过滤，防止旧Pod改标签后漏掉OwnerUID。完整分页同一快照；超界/漂移均Unknown。
	const pageLimit = 200
	token, rv := "", ""
	seen := map[string]bool{}
	for page := 0; page < 32; page++ {
		q := url.Values{"limit": {"200"}}
		if token != "" {
			q.Set("continue", token)
		}
		value, readErr := k.request(ctx, http.MethodGet, k.path("pod", "")+"?"+q.Encode(), nil)
		if readErr != nil {
			return managedSQLRefuse("old_pods_unknown")
		}
		raw, marshalErr := json.Marshal(value)
		var pods core.PodList
		if marshalErr != nil || json.Unmarshal(raw, &pods) != nil || pods.Kind != "PodList" || pods.APIVersion != "v1" ||
			pods.ResourceVersion == "" || pods.Items == nil || len(pods.Items) > pageLimit {
			return managedSQLRefuse("old_pods_unknown")
		}
		if rv == "" {
			rv = pods.ResourceVersion
		} else if rv != pods.ResourceVersion {
			return managedSQLRefuse("old_pods_unknown")
		}
		for _, pod := range pods.Items {
			if pod.Namespace != old.Namespace || pod.UID == "" {
				return managedSQLRefuse("old_pods_unknown")
			}
			for _, owner := range pod.OwnerReferences {
				if string(owner.UID) == old.VMUID {
					return managedSQLRefuse("old_pods_present")
				}
			}
		}
		token = pods.Continue
		if token == "" {
			if pods.RemainingItemCount != nil && *pods.RemainingItemCount != 0 {
				return managedSQLRefuse("old_pods_unknown")
			}
			return nil
		}
		if seen[token] {
			return managedSQLRefuse("old_pods_unknown")
		}
		seen[token] = true
	}
	return managedSQLRefuse("old_pods_unknown")
}
