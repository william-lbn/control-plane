package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var errDSGANativeConfigure = errors.New("original generic Compute configuration rejected")

// 复用当前原生JWT管理口及原Service通路。Mac走显式SSH SOCKS；真实目标
// 仅由原UID/owned ClusterIP产生，不能读取调用方URL或外网代理。
// 新Guest先从原ISO固定公钥；这里提交同一immutable配置，不再依赖镜像内租户数据。
func (binding *dsgaCreationBinding) configureGenericCompute(ctx context.Context) error {
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return err
	}
	if !plan.VM.generic() {
		return nil
	}
	client := newDSGAProxy(nil).observer
	client.Timeout = 35 * time.Second
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		current, vm, err := binding.originalCompute(ctx)
		if err != nil {
			return err
		}
		if vm.Status.Phase != vmv1.VmRunning {
			return errDSGACreationIdentity
		}
		clients, _, err := newDSGAKubeClients(current.server.kube)
		if err != nil {
			return err
		}
		name := vm.Name
		secret, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, name+"-config", metav1.GetOptions{})
		if err != nil || string(secret.UID) != current.configUID || bootstrap.CheckReady(vm, secret, false) != nil {
			return errDSGACreationIdentity
		}
		wanted, err := current.renderDSGABootstrap(ctx, vm)
		if err != nil || !sameDSGABootstrapData(secret.Data, wanted) {
			return errDSGACreationIdentity
		}
		credentials, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, name+"-credentials", metav1.GetOptions{})
		if err != nil {
			return errDSGACredentialSource
		}
		source, err := snapshotDSGACredentials(credentials, plan.Namespace, name+"-credentials", current.intent.ProjectID, current.intent.EndpointID)
		if err != nil || source.reference() != plan.Credentials || !current.verifyCredentialProvenance(ctx, credentials, plan) {
			return errDSGACredentialSource
		}
		service, err := clients.CoreV1().Services(plan.Namespace).Get(ctx, name+"-management", metav1.GetOptions{})
		if err != nil {
			return errDSGACreationIdentity
		}
		expected, err := renderDSGAService(vm, current.intent.createPayload, plan, current.operationID, "management")
		if err != nil {
			return err
		}
		uid, err := current.serviceIdentity(ctx, "management", nil)
		if err != nil || uid == "" || !sameDSGAService(service, expected, uid) {
			return errDSGACreationIdentity
		}
		key := ed25519.NewKeyFromSeed(credentials.Data["controlSigningSeed"])
		target := "http://" + net.JoinHostPort(service.Spec.ClusterIP, "3080")
		token := computeJWT(key, current.intent.EndpointID, time.Now())
		// POST之前重新确认当前lease与原VM；HTTP状态只是选择是否发送原配置，
		// 不是Ready/预算/normal许可。返回后原初始化和SQL链仍逐项验证。
		beforePost := func() error {
			_, fresh, err := binding.originalCompute(ctx)
			if err != nil {
				return err
			}
			if fresh.Status.Phase != vmv1.VmRunning {
				return errDSGACreationIdentity
			}
			return nil
		}
		err = configureOriginalNative(ctx, client, target, token, secret.Data["config.json"], beforePost)
		if !errors.Is(err, errDSGAInitializationPending) {
			return err
		}
		if err = waitDSGAStartupPoll(ctx); err != nil {
			return err
		}
	}
}

// Empty才提交；已进入初始化/运行时只观察，不能在Worker重试时重复reconfigure。
// POST超时/丢回复后转原初始化GET收口，不自动重放。普通SQL仍必须独立探测。
func configureOriginalNative(ctx context.Context, client *http.Client, target, token string, config []byte, beforePost func() error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/status", nil)
	if err != nil {
		return errDSGANativeConfigure
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		var network *net.OpError
		if errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) {
			return errDSGAInitializationPending
		}
		return errDSGANativeConfigure
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 8193))
	response.Body.Close()
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode == http.StatusServiceUnavailable {
		return errDSGAInitializationPending
	}
	var status struct {
		Status string `json:"status"`
	}
	if response.StatusCode != http.StatusOK || mediaErr != nil || media != "application/json" || readErr != nil || len(raw) > 8192 || dsgapi.ValidateJSON(raw, 8192, 1024) != nil || json.Unmarshal(raw, &status) != nil {
		return errDSGANativeConfigure
	}
	switch status.Status {
	case "running", "configuration_pending", "init", "configuration":
		return nil
	case "empty":
	default:
		return errDSGANativeConfigure
	}
	if err := beforePost(); err != nil {
		return err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, target+"/configure", bytes.NewReader(config))
	if err != nil {
		return errDSGANativeConfigure
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err = client.Do(req)
	if err != nil {
		// 原请求可能已被接受；只进入后继原初始化观察，不在这里再发POST。
		return nil
	}
	defer response.Body.Close()
	// 原configure可等待PG初始化；body可能带配置/SQL，永不透传其错误文本。
	if response.StatusCode == http.StatusOK {
		return nil
	}
	if response.StatusCode == http.StatusLocked || response.StatusCode == http.StatusServiceUnavailable {
		return nil
	}
	return errDSGANativeConfigure
}
