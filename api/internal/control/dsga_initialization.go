package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
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

// 与Rust原生初始化观察合同逐字段对应。它不授予普通SQL准入或VM执行权；原生
// observed含真实boot，后续预算/准入必须再次使用该实例身份并独立对账。
type dsgaInitializationResponse struct {
	ProtocolVersion              uint32              `json:"protocol_version"`
	VMShrinkPermit               bool                `json:"vm_shrink_permit"`
	OperationUUID                string              `json:"operation_uuid"`
	TenantID                     string              `json:"tenant_id"`
	TimelineID                   string              `json:"timeline_id"`
	Startup                      dsgaNativeBootstrap `json:"startup"`
	AuthenticationPolicyVerified bool                `json:"authentication_policy_verified"`
	OrdinaryRolesVerified        bool                `json:"ordinary_roles_verified"`
	Observed                     dsgapi.Snapshot     `json:"observed"`
}

func (fact dsgaInitializationResponse) validate(p createPayload, plan dsgaCreationPlan, vmUID string) error {
	wanted, err := plan.nativeBootstrap(p, vmUID)
	if err != nil || fact.ProtocolVersion != 1 || fact.VMShrinkPermit || !fact.AuthenticationPolicyVerified || !fact.OrdinaryRolesVerified ||
		fact.OperationUUID != plan.OperationUUID || fact.TenantID != p.TenantID || fact.TimelineID != p.TimelineID || fact.Startup != wanted ||
		fact.Observed.Validate() != nil || fact.Observed.Instance.VMUID != vmUID || fact.Observed.Instance.ComputeInstanceID != plan.ComputeInstanceID ||
		uint64(fact.Observed.MaximumBuffers)*8192 != plan.Budget.MaximumBufferBytes || uint64(fact.Observed.LowBuffers)*8192 < plan.Budget.BootstrapBufferBytes {
		return errDSGACreationIdentity
	}
	return nil
}

// 此token只用于中心创建Driver的一次只读初始化GET；不复用租户/Agent Bearer，
// 不调用签发器的owner接管/resize路径。签名seed必须来自已冻结UID/完整Data的
// 同一immutable来源；调用方负责保密，既不缓存到Operation，也不记录token。
func dsgaInitializationJWT(key ed25519.PrivateKey, workload, vm, compute string, now time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": computeKeyID(key)})
	claims, _ := json.Marshal(dsgaProxyClaims{Audience: "neon-dsga", Scope: "compute_ctl:dsga_initialization", ComputeID: workload,
		VMUID: vm, ComputeInstanceID: compute, IssuerEpoch: 1, NotBefore: uint64(now.Add(-time.Second).Unix()), Expires: uint64(now.Add(time.Minute).Unix())})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
}

// observeInitialization只走当前同UID VM、原immutable config/credentials与owned
// 内部ClusterIP，不允许调用方传URL/host/端口或redirect。网络前后分别核验原
// Operation lease；不跨Kube/原生HTTP持SQL锁。身份漂移、超时、非JSON/超大/重复
// 字段或不完整证明都拒绝，不调用legacy管理密钥创建/轮转或自动补HBA。
func (binding *dsgaCreationBinding) observeInitialization(ctx context.Context) (dsgaInitializationResponse, error) {
	client := newDSGAProxy(nil).observer
	client.Timeout = 35 * time.Second
	defer client.CloseIdleConnections()
	return binding.observeInitializationWithHTTP(ctx, client)
}

// 私有传输注入仅用于有界协议测试；生产入口上面统一创建无代理/无redirect的
// 观察客户端。目标仍由同一原VM的typed Service读取，不能通过参数改URL或身份。
func (binding *dsgaCreationBinding) observeInitializationWithHTTP(ctx context.Context, client *http.Client) (dsgaInitializationResponse, error) {
	var empty dsgaInitializationResponse
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	current, err := binding.server.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
	if err != nil || current.vmUID == "" || current.configUID == "" {
		return empty, errDSGACreationIdentity
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		return empty, err
	}
	clients, vms, err := newDSGAKubeClients(current.server.kube)
	if err != nil {
		return empty, err
	}
	p, name := current.intent.createPayload, kubeName(current.intent.EndpointID)
	readVM := func() (*vmv1.VirtualMachine, error) {
		vm, err := vms.NeonvmV1().VirtualMachines(plan.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || current.validateVM(vm) != nil || string(vm.UID) != current.vmUID || vm.Status.Phase != vmv1.VmRunning ||
			!vm.Spec.DSGABootstrap.Ready || vm.Spec.DSGABootstrap.VMUID != current.vmUID || vm.Spec.DSGABootstrap.ConfigSecretUID != current.configUID {
			return nil, errDSGACreationIdentity
		}
		wanted, err := renderDSGAVM(p, plan)
		if err != nil || !sameDSGAVM(vm, wanted) {
			return nil, errDSGACreationIdentity
		}
		return vm, nil
	}
	vm, err := readVM()
	if err != nil {
		return empty, err
	}
	credentials, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, name+"-credentials", metav1.GetOptions{})
	if err != nil {
		return empty, errDSGACredentialSource
	}
	source, err := snapshotDSGACredentials(credentials, plan.Namespace, name+"-credentials", p.ProjectID, p.EndpointID)
	if err != nil || source.reference() != plan.Credentials || !dsgaCredentialProvenance(credentials, current.operationID, plan) || len(source.probeSeed) != 32 {
		return empty, errDSGACredentialSource
	}
	config, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, name+"-config", metav1.GetOptions{})
	if err != nil || string(config.UID) != current.configUID || bootstrap.CheckReady(vm, config, false) != nil {
		return empty, errDSGACreationIdentity
	}
	wanted, err := renderDSGAComputeBootstrap(p, plan, current.vmUID, source.credentials)
	if err != nil || len(config.Data) != 1 || !bytes.Equal(config.Data["config.json"], wanted) {
		return empty, errDSGACreationIdentity
	}
	service, err := clients.CoreV1().Services(plan.Namespace).Get(ctx, name+"-management", metav1.GetOptions{})
	if err != nil {
		return empty, errDSGACreationIdentity
	}
	wantedService, err := renderDSGAService(vm, p, plan, current.operationID, "management")
	if err != nil {
		return empty, err
	}
	serviceUID, err := current.serviceIdentity(ctx, "management", nil)
	if err != nil || serviceUID == "" || !sameDSGAService(service, wantedService, serviceUID) {
		return empty, errDSGACreationIdentity
	}
	key := ed25519.NewKeyFromSeed(credentials.Data["controlSigningSeed"])
	token := dsgaInitializationJWT(key, name, current.vmUID, plan.ComputeInstanceID, time.Now())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(service.Spec.ClusterIP, "3080")+"/dsg/v1/initialization", nil)
	if err != nil {
		return empty, errDSGACreationIdentity
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Cache-Control", "no-store")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	// 复用管理观察通道的无代理/无redirect/有限连接池配置；初始化核验有独立
	// 45秒总上界；只读GET的传输恢复不会产生执行意图，不在此路径发送POST。
	// HTTP内部链仍须Linux网络隔离认证。
	response, err := client.Do(request)
	if err != nil {
		return empty, errDSGACreationIdentity
	}
	defer response.Body.Close()
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	var fact dsgaInitializationResponse
	if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || len(response.Header.Values("Content-Type")) != 1 || mediaErr != nil || media != "application/json" ||
		readErr != nil || len(raw) > 64<<10 || dsgapi.UnmarshalObject(raw, &fact) != nil || fact.validate(p, plan, current.vmUID) != nil {
		return empty, errDSGACreationIdentity
	}
	latest, err := readVM()
	if err != nil {
		return empty, err
	}
	if bootstrap.CheckReady(latest, config, false) != nil {
		return empty, errDSGACreationIdentity
	}
	// 最后复核原lease和已经持久化的来源；仅同来源只读恢复，不创造新事实。
	if err := current.confirmCredentialSource(ctx, latest, source); err != nil {
		return empty, err
	}
	return fact, nil
}
