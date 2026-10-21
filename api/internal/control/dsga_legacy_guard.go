package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

var errDSGALegacyDriver = errors.New("DSGA compute requires its versioned creation driver")

// requireLegacyVM保护旧PG16创建/恢复/路由入口，不允许其收养受管VM。
// bootstrap与任一journal标记的“存在”就足以拒绝，包括null/空/坏记录；
// 不能先解码失败，再当作未启用而走legacy。新DSGA路径必须使用原始创建
// 意图和UID/RV屏障，不能调用旧createOwned的仅labels冲突复用来代替它。
func requireLegacyVM(vm map[string]any) error {
	if vm == nil {
		return errDSGALegacyDriver
	}
	raw, err := json.Marshal(vm)
	if err != nil {
		return errDSGALegacyDriver
	}
	var identity struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if json.Unmarshal(raw, &identity) != nil || dsgapi.Managed(identity.Metadata.Annotations) {
		return errDSGALegacyDriver
	}
	if _, present := identity.Spec["dsgaBootstrap"]; present {
		return errDSGALegacyDriver
	}
	return nil
}

// requireLegacyConfig也拒绝仅创建了动态配置、尚未完成VM创建的中断现场。
// 这里只探测模式，不重写配置、不制造缺失日志、不轮转密钥；坏JSON和显式null
// 同样不能降级。原始JSON只用RawMessage判断字段存在，不经过float64预算中转。
func requireLegacyConfig(secret map[string]any) error {
	raw, err := secretText(secret, "config.json")
	if err != nil {
		return errDSGALegacyDriver
	}
	var config struct {
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if json.Unmarshal([]byte(raw), &config) != nil || config.Spec == nil {
		return errDSGALegacyDriver
	}
	if _, present := config.Spec["dynamic_shared_buffers"]; present {
		return errDSGALegacyDriver
	}
	return nil
}

// legacyComputePreflight在任何凭据/配置/Service写入之前，以可信客户端直接
// 检查旧操作是否误入DSGA现场。仅404表示尚未创建；超时、权限或API故障必须
// 原样中断，不能猜测为空并重建。一次检查至多10秒，不持本地锁跨网络。
// 此预检不是跨对象原子许可：createOwned冲突读回及路由入口还要再次检查，
// 新路径的不可变模式、创建屏障与最终执行fencing仍是独立必需的保护。
func (k *kubeClient) legacyComputePreflight(ctx context.Context, p createPayload) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, target := range []struct {
		kind, name string
		check      func(map[string]any) error
	}{
		{"vm", kubeName(p.EndpointID), requireLegacyVM},
		{"secret", kubeName(p.EndpointID) + "-config", requireLegacyConfig},
	} {
		item, err := k.request(ctx, http.MethodGet, k.path(target.kind, target.name), nil)
		if kubeStatusIs(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !owned(item, p.ProjectID, p.EndpointID) {
			return errors.New("legacy compute object ownership mismatch")
		}
		if err := target.check(item); err != nil {
			return err
		}
	}
	return nil
}
