package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/owner"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// dsgaProxy只位于中心独占TLS监听器。入站目标只能从已验证SNI确定，内部
// 地址只能从owned ClusterIP Service读取；不接受URL、Host、转发头或redirect。
// 内部原生3080保留Rust专用JWT验证；网络隔离/Service路由属于Linux后验门槛。
type dsgaProxy struct {
	broker    *dsgaCredentialBroker
	execution *http.Client
	observer  *http.Client
}

func newDSGAProxy(broker *dsgaCredentialBroker) *dsgaProxy {
	client := func(timeout time.Duration) *http.Client {
		return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil,
			MaxConnsPerHost: 16, MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
			IdleConnTimeout: 30 * time.Second, DisableCompression: true,
			DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	// 执行与观察各自拥有有限连接池，长执行不会占满观察/取消通道。
	// 这里只发送一次POST；连接断开、超时、响应丢失均不重发或创造成功回执。
	return &dsgaProxy{broker: broker, execution: client(70 * time.Second), observer: client(15 * time.Second)}
}

type dsgaProxyClaims struct {
	Audience          string        `json:"aud"`
	Scope             string        `json:"scope"`
	ComputeID         string        `json:"compute_id"`
	VMUID             string        `json:"vm_uid"`
	ComputeInstanceID string        `json:"compute_instance_id"`
	IssuerEpoch       dsgapi.Uint64 `json:"issuer_epoch"`
	Expires           uint64        `json:"exp"`
	NotBefore         uint64        `json:"nbf"`
}

// 与Rust deny_unknown_fields一致，还拒绝重复JSON成员，避免不同解析器对
// 同一个签名解释成不同主体。业务body保持原字节交给原生有类型解析器。
func dsgaTokenObject(raw []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errors.New("invalid token object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return errors.New("duplicate token member")
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return errors.New("invalid token member")
		}
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid token tail")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

func validDSGAProxyJWT(token, workload string, config dsgaCredentialConfig, now time.Time) (dsgaProxyClaims, bool) {
	var claims dsgaProxyClaims
	parts := strings.Split(token, ".")
	if len(token) > 8192 || len(parts) != 3 || !computeWorkloadPattern.MatchString(workload) || config.Spec.Dynamic == nil {
		return claims, false
	}
	headerBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err != nil || dsgaTokenObject(headerBytes, &header) != nil || header.Alg != "EdDSA" || header.Typ != "JWT" || header.Kid == "" {
		return claims, false
	}
	var public []byte
	matches := 0
	for _, jwk := range config.Ctl.JWKS.Keys {
		if jwk.Kid == header.Kid {
			matches++
			if jwk.Kty != "OKP" || jwk.Crv != "Ed25519" || jwk.Alg != "EdDSA" || jwk.Use != "sig" || len(jwk.Ops) != 1 || jwk.Ops[0] != "verify" {
				return claims, false
			}
			public, err = base64.RawURLEncoding.Strict().DecodeString(jwk.X)
			if err != nil {
				return claims, false
			}
		}
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || matches != 1 || len(public) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(public), []byte(parts[0]+"."+parts[1]), signature) {
		return claims, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || dsgaTokenObject(raw, &claims) != nil || claims.Audience != "neon-dsga" || claims.Scope != "compute_ctl:dsga" ||
		claims.ComputeID != workload || config.Spec.Dynamic.ProtocolVersion != 2 || claims.VMUID != config.Spec.Dynamic.VMUID || claims.ComputeInstanceID != config.Spec.Dynamic.ComputeInstanceID ||
		dsgapi.ValidateID(claims.VMUID, "VM") != nil || dsgapi.ValidateID(claims.ComputeInstanceID, "Compute") != nil || claims.IssuerEpoch == 0 ||
		claims.Expires <= uint64(now.Unix()) || claims.NotBefore > uint64(now.Unix()) || claims.Expires <= claims.NotBefore || claims.Expires-claims.NotBefore > 300 {
		return claims, false
	}
	return claims, true
}

func (proxy *dsgaProxy) current(ctx context.Context, store *journal.Store, claims dsgaProxyClaims) (*journal.Snapshot, error) {
	snapshot, err := store.Load(ctx)
	if err != nil || !snapshot.Dispatchable() || snapshot.State().Owner.Epoch != claims.IssuerEpoch {
		return nil, owner.ErrLeadershipLost
	}
	binding := store.Binding()
	lease, err := proxy.broker.coord.Leases(binding.Namespace).Get(ctx, owner.LeaseName(binding), metav1.GetOptions{})
	if err != nil {
		return nil, owner.ErrLeadershipLost
	}
	proof := owner.Ownership{Namespace: binding.Namespace, VMName: binding.Name, VMUID: binding.VMUID, Incarnation: binding.Incarnation,
		LeaseUID: lease.UID, HolderID: snapshot.State().Owner.HolderID, IssuerEpoch: claims.IssuerEpoch}
	snapshot, lease, err = owner.VerifyCurrent(ctx, proxy.broker.coord, store, proof)
	if err != nil || !snapshot.Dispatchable() || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 2 {
		return nil, owner.ErrLeadershipLost
	}
	// 不将短JWT当永续Lease。保守扣除2秒时钟/传播余量，未来续约时间亦
	// 有界；最终检查后仍可换主，PG的单调issuer floor仍是必需的最后栅栏。
	now := time.Now()
	if lease.Spec.RenewTime.After(now.Add(2*time.Second)) || !now.Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds-2)*time.Second)) || ctx.Err() != nil {
		return nil, owner.ErrLeadershipLost
	}
	return snapshot, nil
}

func (proxy *dsgaProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	deny := func(code int) { http.Error(w, "DSGA management request rejected", code) }
	execution, known := false, true
	switch r.URL.Path {
	case "/dsg/v1/capabilities", "/dsg/v1/memory", "/dsg/v1/initialization", "/dsg/v1/budget/shared", "/dsg/v1/work/state", "/dsg/v1/connections/state", "/dsg/v1/work/prepared", "/dsg/v1/work/dsm", "/dsg/v1/resource/identity", "/dsg/v1/resource/process", "/dsg/v1/resource/state":
		known = r.Method == http.MethodGet
	case "/dsg/v1/resize", "/dsg/v1/issuer/fence", "/dsg/v1/resource/fence", "/dsg/v1/resource/apply", "/dsg/v1/resource/replay-original":
		execution, known = true, r.Method == http.MethodPost
	case "/dsg/v1/resize/status", "/dsg/v1/resize/cancel",
		"/dsg/v1/admission", "/dsg/v1/admission/status",
		"/dsg/v1/admission/close-original", "/dsg/v1/admission/close-original/status":
		// 准入写入也走独立观察连接池：关闭必须能在resize阻塞时到达PG。
		// 这里只扩展固定路由名单，仍逐次核验原VM/Service/current owner；
		// 原生Rust按完整请求再核对执行epoch，网关不代造预算或执行回执。
		known = r.Method == http.MethodPost
	case "/dsg/v1/resource/observe":
		// Guest资源对账独立于PG boot，仍按正式TokenReview/Lease/原Service链
		// 校验；只是固定路径转发，不能在网关代造Root限额或VM许可。
		known = r.Method == http.MethodPost
	default:
		known = false
	}
	if !known || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || r.TLS == nil || len(r.Header.Values("Authorization")) != 1 {
		deny(http.StatusBadRequest)
		return
	}
	suffix := "-management." + proxy.broker.kube.namespace + ".svc"
	workload := strings.TrimSuffix(r.TLS.ServerName, suffix)
	bearer, valid := owner.BearerToken(r.Header.Get("Authorization"))
	if !strings.HasSuffix(r.TLS.ServerName, suffix) || !computeWorkloadPattern.MatchString(workload) || !valid {
		deny(http.StatusUnauthorized)
		return
	}
	// 入站body必须在10秒内读完；POST最多16KiB，GET不接受body。令牌/
	// 内部错误不进入响应/日志，不转发任何调用方的连接、重试或目标头。
	if http.NewResponseController(w).SetReadDeadline(time.Now().Add(10*time.Second)) != nil {
		deny(http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16*1024))
	if err != nil {
		deny(http.StatusRequestEntityTooLarge)
		return
	}
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Header.Get("Content-Encoding") != "" || (r.Method == http.MethodGet && len(body) != 0) ||
		(r.Method == http.MethodPost && (len(r.Header.Values("Content-Type")) != 1 || mediaErr != nil || media != "application/json" || len(body) == 0)) {
		deny(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	preflight, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	secret, err := proxy.broker.core.Secrets(proxy.broker.kube.namespace).Get(preflight, workload+"-config", metav1.GetOptions{})
	endpoint := "ep_" + strings.TrimPrefix(workload, "cp-")
	if err != nil || secret == nil || !dsgaOwnedSecret(secret, proxy.broker.kube.namespace, workload+"-config", secret.Labels["neon-control/project-id"], endpoint) || secret.Labels["neon-control/project-id"] == "" {
		deny(http.StatusServiceUnavailable)
		return
	}
	var config dsgaCredentialConfig
	if json.Unmarshal(secret.Data["config.json"], &config) != nil {
		deny(http.StatusServiceUnavailable)
		return
	}
	claims, valid := validDSGAProxyJWT(bearer, workload, config, time.Now())
	if !valid {
		deny(http.StatusUnauthorized)
		return
	}
	vm, err := proxy.broker.vms.NeonvmV1().VirtualMachines(proxy.broker.kube.namespace).Get(preflight, workload, metav1.GetOptions{})
	if err != nil || vm == nil || vm.Namespace != proxy.broker.kube.namespace || vm.Name != workload || string(vm.UID) != claims.VMUID || !dsgapi.Managed(vm.Annotations) || dsgapi.ValidateManagedTransition(vm.Annotations, vm.Annotations, false) != nil ||
		vm.Labels["neon-control/project-id"] != secret.Labels["neon-control/project-id"] || vm.Labels["neon-control/endpoint-id"] != endpoint {
		deny(http.StatusForbidden)
		return
	}
	store, err := journal.NewStore(proxy.broker.vms.NeonvmV1().VirtualMachines(vm.Namespace), journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: vm.Annotations[dsgapi.ModeAnnotation][2:]})
	if err != nil {
		deny(http.StatusForbidden)
		return
	}
	service, err := proxy.broker.core.Services(vm.Namespace).Get(preflight, workload+"-management", metav1.GetOptions{})
	if err != nil || service == nil || service.Namespace != vm.Namespace || service.Name != workload+"-management" || service.UID == "" || service.ResourceVersion == "" || service.DeletionTimestamp != nil ||
		service.Labels["neon-control/project-id"] != secret.Labels["neon-control/project-id"] || service.Labels["neon-control/endpoint-id"] != endpoint || service.Spec.Type != core.ServiceTypeClusterIP ||
		service.Spec.ExternalName != "" || len(service.Spec.ExternalIPs) != 0 || net.ParseIP(service.Spec.ClusterIP) == nil || len(service.Spec.Selector) != 1 || service.Spec.Selector["vm.neon.tech/name"] != workload ||
		len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Protocol != core.ProtocolTCP || service.Spec.Ports[0].Port != 3080 || service.Spec.Ports[0].TargetPort.IntVal != 3080 || service.Spec.Ports[0].TargetPort.Type != 0 {
		deny(http.StatusServiceUnavailable)
		return
	}
	snapshot, err := proxy.current(preflight, store, claims)
	if err != nil || snapshot.VirtualMachine().Labels["neon-control/project-id"] != secret.Labels["neon-control/project-id"] || snapshot.VirtualMachine().Labels["neon-control/endpoint-id"] != endpoint || bootstrap.CheckReady(snapshot.VirtualMachine(), secret, false) != nil {
		deny(http.StatusForbidden)
		return
	}
	stop()
	request, err := http.NewRequestWithContext(ctx, r.Method, "http://"+net.JoinHostPort(service.Spec.ClusterIP, "3080")+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		deny(http.StatusServiceUnavailable)
		return
	}
	request.GetBody = nil
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	client := proxy.observer
	if execution {
		client = proxy.execution
	}
	response, err := client.Do(request)
	if err != nil {
		deny(http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	media, _, err = mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || response.Header.Get("Content-Encoding") != "" || response.StatusCode < 200 || response.StatusCode >= 300 && response.StatusCode < 400 {
		deny(http.StatusBadGateway)
		return
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(result) > 1<<20 || !json.Valid(result) {
		deny(http.StatusBadGateway)
		return
	}
	// 只透传有界JSON事实与状态码，不复制上游Set-Cookie/Location/缓存头。
	// 502/取消只能表示未知；Agent须保留完整原意图并查询，不因网关失败重试POST。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(result)
}
