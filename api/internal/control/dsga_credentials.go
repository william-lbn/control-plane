package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/owner"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

// dsgaCredentialBroker只驻留可信控制面。复用自有fork的完整journal/主体核验，
// 不复制压缩日志解码、不从独立owner镜像或请求epoch推导权限。签名私钥只读
// owned Secret；Agent只能得到同一VM/Compute/epoch的短令牌，永远不取得seed。
type dsgaCredentialBroker struct {
	kube     *kubeClient
	vms      vmclient.Interface
	core     coreclient.CoreV1Interface
	coord    coordclient.CoordinationV1Interface
	verifier *owner.PodVerifier
}

func newDSGACredentialBroker(kube *kubeClient, agentNamespace, account string) (*dsgaCredentialBroker, error) {
	clients, vms, err := newDSGAKubeClients(kube)
	if err != nil {
		return nil, err
	}
	verifier, err := owner.NewPodVerifier(clients.AuthenticationV1(), clients.CoreV1(), clients.CoordinationV1(), agentNamespace, account, 5*time.Second, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return &dsgaCredentialBroker{kube: kube, vms: vms, core: clients.CoreV1(), coord: clients.CoordinationV1(), verifier: verifier}, nil
}

// 只读取本入口的身份字段；完整启动预算由生产Compute的同一Rust/PG契约校验。
// 不通过map[string]any/float64中转原始数值，也不在签发器另造预算成功回执。
type dsgaCredentialConfig struct {
	Spec struct {
		Dynamic *struct {
			ProtocolVersion   uint32 `json:"protocol_version"`
			VMUID             string `json:"vm_uid"`
			ComputeInstanceID string `json:"compute_instance_id"`
		} `json:"dynamic_shared_buffers"`
	} `json:"spec"`
	Ctl struct {
		JWKS struct {
			Keys []struct {
				Kty string   `json:"kty"`
				Crv string   `json:"crv"`
				Alg string   `json:"alg"`
				Kid string   `json:"kid"`
				X   string   `json:"x"`
				Use string   `json:"use"`
				Ops []string `json:"key_ops"`
			} `json:"keys"`
		} `json:"jwks"`
	} `json:"compute_ctl_config"`
}

func dsgaOwnedSecret(secret *core.Secret, namespace, name, project, endpoint string) bool {
	return secret != nil && secret.Labels["neon-control/project-id"] == project && secret.Labels["neon-control/endpoint-id"] == endpoint &&
		secret.Namespace == namespace && secret.Name == name && secret.UID != "" && secret.ResourceVersion != "" && secret.DeletionTimestamp == nil
}

// signingIdentity不调用会创建/轮转私钥的computeControlKey兜底。缺失/坏seed、
// 目标配置未启用、VM/实例绑定不同或JWK不匹配均拒绝，不能先发令牌再改Compute。
func (broker *dsgaCredentialBroker) signingIdentity(ctx context.Context, workload, project, endpoint string, claim owner.Ownership, snapshot *journal.Snapshot) (ed25519.PrivateKey, string, error) {
	configSecret, err := broker.core.Secrets(broker.kube.namespace).Get(ctx, workload+"-config", metav1.GetOptions{})
	if err != nil || !dsgaOwnedSecret(configSecret, broker.kube.namespace, workload+"-config", project, endpoint) {
		return nil, "", owner.ErrCredentialsDenied
	}
	// 启动记录已经固定真实VM/Config Secret UID与Compute身份。复用同一
	// 完整日志/owned配置核验，不能只看同名Secret或一份可改变的config.json。
	// 这仍不是预算/VM许可，记录必须由受管创建流程先行准备并解除屏障。
	if snapshot == nil || bootstrap.CheckReady(snapshot.VirtualMachine(), configSecret, false) != nil {
		return nil, "", owner.ErrCredentialsDenied
	}
	raw := configSecret.Data["config.json"]
	var config dsgaCredentialConfig
	if json.Unmarshal(raw, &config) != nil || config.Spec.Dynamic == nil || config.Spec.Dynamic.ProtocolVersion != 2 ||
		config.Spec.Dynamic.VMUID != string(claim.VMUID) || dsgapi.ValidateID(config.Spec.Dynamic.ComputeInstanceID, "Compute") != nil {
		return nil, "", owner.ErrCredentialsDenied
	}
	credentials, err := broker.core.Secrets(broker.kube.namespace).Get(ctx, workload+"-credentials", metav1.GetOptions{})
	if err != nil || !dsgaOwnedSecret(credentials, broker.kube.namespace, workload+"-credentials", project, endpoint) {
		return nil, "", owner.ErrCredentialsDenied
	}
	seed := credentials.Data["controlSigningSeed"]
	if len(seed) != ed25519.SeedSize {
		return nil, "", owner.ErrCredentialsDenied
	}
	key := ed25519.NewKeyFromSeed(seed)
	public := key.Public().(ed25519.PublicKey)
	matches := 0
	for _, jwk := range config.Ctl.JWKS.Keys {
		if jwk.Kid == computeKeyID(key) {
			x, err := base64.RawURLEncoding.DecodeString(jwk.X)
			if err != nil || jwk.Kty != "OKP" || jwk.Crv != "Ed25519" || jwk.Alg != "EdDSA" ||
				jwk.Use != "sig" || len(jwk.Ops) != 1 || jwk.Ops[0] != "verify" || !bytes.Equal(x, public) {
				return nil, "", owner.ErrCredentialsDenied
			}
			matches++
		}
	}
	if matches != 1 {
		return nil, "", owner.ErrCredentialsDenied
	}
	return key, config.Spec.Dynamic.ComputeInstanceID, nil
}

func dsgaSubjectProject(snapshot *journal.Snapshot, endpoint string) (string, bool) {
	vm := snapshot.VirtualMachine()
	project := vm.Labels["neon-control/project-id"]
	return project, project != "" && vm.Labels["neon-control/endpoint-id"] == endpoint
}

// ServeHTTP只在显式配置允许SA后由现有TLS gateway挂载。TLS/SNI、固定路径、
// 单一Bearer、有限body/总期限先行；完成真实主体与owner核验后才能读取私钥，
// 读取配置/私钥后再核验同一owner。仍无跨对象原子许可，最终PG/Runner须fence。
func (broker *dsgaCredentialBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	deny := func(code int) { http.Error(w, "DSGA credential request rejected", code) }
	workload := r.PathValue("compute")
	if r.Method != http.MethodPost || !computeWorkloadPattern.MatchString(workload) || r.TLS == nil ||
		r.TLS.ServerName != workload+"-management."+broker.kube.namespace+".svc" || len(r.Header.Values("Authorization")) != 1 {
		deny(http.StatusForbidden)
		return
	}
	bearer, valid := owner.BearerToken(r.Header.Get("Authorization"))
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !valid || len(r.Header.Values("Content-Type")) != 1 || err != nil || media != "application/json" || r.Header.Get("Content-Encoding") != "" {
		deny(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// context期限不会自行打断恶意慢速入站body；真实HTTP连接还必须限制
	// 读截止时间。网关没有隐藏的ResponseWriter包装，不支持期限时失败关闭。
	if http.NewResponseController(w).SetReadDeadline(time.Now().Add(10*time.Second)) != nil {
		deny(http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var claim owner.Ownership
	if decoder.Decode(&claim) != nil || decoder.Decode(new(any)) != io.EOF || claim.Namespace != broker.kube.namespace || claim.VMName != workload {
		deny(http.StatusBadRequest)
		return
	}
	store, err := journal.NewStore(broker.vms.NeonvmV1().VirtualMachines(broker.kube.namespace), journal.Binding{
		Namespace: claim.Namespace, Name: claim.VMName, VMUID: claim.VMUID, Incarnation: claim.Incarnation,
	})
	if err != nil {
		deny(http.StatusForbidden)
		return
	}
	snapshot, err := broker.verifier.Verify(ctx, bearer, store, claim)
	if err != nil {
		deny(http.StatusForbidden)
		return
	}
	endpoint := "ep_" + strings.TrimPrefix(workload, "cp-")
	project, valid := dsgaSubjectProject(snapshot, endpoint)
	if !valid {
		deny(http.StatusForbidden)
		return
	}
	expectedBootstrap := snapshot.VirtualMachine().Spec.DSGABootstrap
	key, instance, err := broker.signingIdentity(ctx, workload, project, endpoint, claim, snapshot)
	if err != nil {
		deny(http.StatusForbidden)
		return
	}
	snapshot, err = broker.verifier.Verify(ctx, bearer, store, claim)
	if err != nil {
		deny(http.StatusForbidden)
		return
	}
	confirmedProject, valid := dsgaSubjectProject(snapshot, endpoint)
	currentBootstrap := snapshot.VirtualMachine().Spec.DSGABootstrap
	if !valid || confirmedProject != project || expectedBootstrap == nil || currentBootstrap == nil || *expectedBootstrap != *currentBootstrap || ctx.Err() != nil {
		deny(http.StatusForbidden)
		return
	}
	now := time.Now()
	expiry := now.Add(time.Minute).Unix()
	header, _ := json.Marshal(map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": computeKeyID(key)})
	// 专用scope/audience及严格字符串epoch，与正式Compute DSGA校验一致；
	// 不复用general computeJWT、admin或Hadron的其他权限。
	// 此令牌不是预算、物理回收或VM许可，也不推进PG issuer floor；原生
	// 端点仍核对boot/完整意图/floor，最终资源域仍须验证预算并独立fence。
	claims, _ := json.Marshal(map[string]any{"aud": "neon-dsga", "scope": "compute_ctl:dsga", "compute_id": workload,
		"vm_uid": string(claim.VMUID), "compute_instance_id": instance, "issuer_epoch": claim.IssuerEpoch,
		"nbf": now.Add(-2 * time.Second).Unix(), "exp": expiry})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	token := input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
	jsonResponse(w, http.StatusOK, map[string]any{"token_type": "Bearer", "access_token": token, "expires_at": expiry,
		"compute_id": workload, "vm_uid": string(claim.VMUID), "compute_instance_id": instance, "issuer_epoch": claim.IssuerEpoch})
}
