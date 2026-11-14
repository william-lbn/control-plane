package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"reflect"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// dsgaPostgresPort沿现有Helm受管数据面；Legacy端口仍由旧路径保留。
const dsgaPostgresPort = 55432

var errManagedSQLPublish = errors.New("managed SQL route publication refused")

// managedSQLRouteInput不包含口令、签名seed、verifier或调用者自报的Native事实。
// Branch是原已冻结业务Spec的输入，必须与本次原immutable Config逐项相同。
type managedSQLRouteInput struct {
	ProtocolVersion      uint32                    `json:"protocol_version"`
	Binding              adapter.ManagedSQLBinding `json:"binding"`
	RegistryUID          string                    `json:"registry_uid"`
	ManagementServiceUID string                    `json:"management_service_uid"`
	resume               *managedSQLResumeProof
}

// ManagedSQLRouteReceipt只回传来源元数据；路由正文与凭据不得进入日志。
type ManagedSQLRouteReceipt struct {
	ProtocolVersion            uint32 `json:"protocol_version"`
	Selector                   string `json:"selector"`
	VMUID                      string `json:"vm_uid"`
	Incarnation                string `json:"incarnation"`
	ConfigSecretUID            string `json:"config_secret_uid"`
	ConfigSHA256               string `json:"config_sha256"`
	SecretDataSHA256           string `json:"secret_data_sha256"`
	RegistryUID                string `json:"registry_uid"`
	RegistryResourceVersion    string `json:"registry_resource_version"`
	NativeInitializationSHA256 string `json:"native_initialization_sha256"`
}

type managedSQLRouteSource struct {
	vm                  *vmv1.VirtualMachine
	config, credentials *core.Secret
	management          *core.Service
	cfg                 adapter.ManagedSQLConfiguration
	startup             dsgaNativeBootstrap
	snapshot            dsgaCredentialSnapshot
}

func (in managedSQLRouteInput) validate() error {
	if in.ProtocolVersion != 1 || in.Binding.ValidateIdentity() != nil || in.Binding.NativeInitializationSHA256 != "" ||
		dsgapi.ValidateID(in.RegistryUID, "route registry") != nil || dsgapi.ValidateID(in.ManagementServiceUID, "management Service") != nil {
		return errManagedSQLPublish
	}
	return nil
}

func managedSQLDigest(v []byte) string { h := sha256.Sum256(v); return hex.EncodeToString(h[:]) }
func managedSQLDataDigest(v map[string][]byte) string {
	raw, _ := json.Marshal(v)
	return managedSQLDigest(raw)
}

func managedSQLRead(k *kubeClient, ctx context.Context, kind, name string, target any) error {
	value, err := k.request(ctx, http.MethodGet, k.path(kind, name), nil)
	if err != nil {
		return errManagedSQLPublish
	}
	raw, err := json.Marshal(value)
	if err != nil || json.Unmarshal(raw, target) != nil {
		return errManagedSQLPublish
	}
	return nil
}

// startup的wire合同仍是原七字段；显式alias供strict decoder校验，随后还原原类型。
func decodeManagedSQLStartup(raw []byte) (dsgaNativeBootstrap, error) {
	var wire struct {
		ProtocolVersion         uint32 `json:"protocol_version"`
		VMUID                   string `json:"vm_uid"`
		ComputeInstanceID       string `json:"compute_instance_id"`
		BootstrapBufferBytes    uint64 `json:"bootstrap_buffer_bytes"`
		MaximumBufferBytes      uint64 `json:"maximum_buffer_bytes"`
		MinimumGuestMemoryBytes uint64 `json:"minimum_guest_memory_bytes"`
		NonBufferReserveBytes   uint64 `json:"non_buffer_reserve_bytes"`
	}
	if dsgapi.UnmarshalObject(raw, &wire) != nil {
		return dsgaNativeBootstrap{}, errManagedSQLPublish
	}
	return dsgaNativeBootstrap{ProtocolVersion: wire.ProtocolVersion, VMUID: wire.VMUID, ComputeInstanceID: wire.ComputeInstanceID,
		dsgaBootstrapBudget: dsgaBootstrapBudget{BootstrapBufferBytes: wire.BootstrapBufferBytes, MaximumBufferBytes: wire.MaximumBufferBytes,
			MinimumGuestMemoryBytes: wire.MinimumGuestMemoryBytes, NonBufferReserveBytes: wire.NonBufferReserveBytes}}, nil
}

func managedSQLRouteMap(raw []byte) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	// 原registry是2MiB有界只读文档；解析不赋予任何Native或写许可。
	if dsgapi.ValidateObservationJSON(raw, 2<<20, 65536) != nil || json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, errManagedSQLPublish
	}
	return out, nil
}

// policy原字节/语义保留：空列表表示deny-all，不能在refresh时改成默认allow-all。
func validManagedSQLPolicy(route map[string]json.RawMessage) bool {
	if raw, ok := route["allowed_ips"]; ok {
		var ips []string
		if json.Unmarshal(raw, &ips) != nil {
			return false
		}
		for _, ip := range ips {
			if net.ParseIP(ip) == nil {
				if _, _, err := net.ParseCIDR(ip); err != nil {
					return false
				}
			}
		}
	}
	if raw, ok := route["pageserver_node_id"]; ok {
		var id uint64
		if json.Unmarshal(raw, &id) != nil {
			return false
		}
	}
	if raw, ok := route["state"]; ok {
		var state string
		if json.Unmarshal(raw, &state) != nil {
			return false
		}
	}
	return true
}

func readManagedSQLRouteSource(ctx context.Context, k *kubeClient, in managedSQLRouteInput) (managedSQLRouteSource, error) {
	var out managedSQLRouteSource
	b := in.Binding
	name := kubeName(b.EndpointID)
	if k.namespace != b.Namespace {
		return out, errManagedSQLPublish
	}
	vm := new(vmv1.VirtualMachine)
	cfg := new(core.Secret)
	creds := new(core.Secret)
	management := new(core.Service)
	if managedSQLRead(k, ctx, "vm", name, vm) != nil || managedSQLRead(k, ctx, "secret", name+"-config", cfg) != nil ||
		managedSQLRead(k, ctx, "secret", name+"-credentials", creds) != nil || managedSQLRead(k, ctx, "service", name+"-management", management) != nil {
		return out, errManagedSQLPublish
	}
	boot := vm.Spec.DSGABootstrap
	if vm.Name != name || vm.Namespace != b.Namespace || string(vm.UID) != b.VMUID || vm.ResourceVersion == "" || vm.DeletionTimestamp != nil ||
		!dsgapi.Managed(vm.Annotations) || boot == nil || !boot.Ready || boot.VMUID != b.VMUID ||
		boot.Incarnation != b.Incarnation || boot.ComputeInstanceID != b.ComputeInstanceID || boot.ConfigSecretUID != b.ConfigSecretUID ||
		vm.Labels["neon-control/project-id"] != b.ProjectID || vm.Labels["neon-control/endpoint-id"] != b.EndpointID ||
		bootstrap.CheckReady(vm, cfg, false) != nil || managedSQLDigest(cfg.Data["config.json"]) != b.ConfigSHA256 || managedSQLDataDigest(cfg.Data) != b.SecretDataSHA256 {
		return out, errManagedSQLPublish
	}
	projection, err := adapter.DecodeManagedSQLConfiguration(cfg.Data["config.json"], b)
	if err != nil {
		return out, errManagedSQLPublish
	}
	startup, startupErr := decodeManagedSQLStartup(projection.Spec.Dynamic)
	// 路由发布与 Agent、Gateway 共用启动协议集合；后续仍须实际 PG 回执与封存配置逐字段相符。
	if startupErr != nil || !dsgapi.SupportedNativeBootstrapVersion(startup.ProtocolVersion) || startup.VMUID != b.VMUID ||
		startup.ComputeInstanceID != b.ComputeInstanceID || startup.BootstrapBufferBytes == 0 || startup.BootstrapBufferBytes%8192 != 0 ||
		startup.MaximumBufferBytes < startup.BootstrapBufferBytes || startup.MaximumBufferBytes%8192 != 0 {
		return out, errManagedSQLPublish
	}
	snap, err := snapshotDSGACredentials(creds, b.Namespace, name+"-credentials", b.ProjectID, b.EndpointID)
	if err != nil || len(creds.Data) != 4 || string(creds.UID) != b.CredentialSecretUID || managedSQLDataDigest(creds.Data) != b.CredentialDataSHA256 {
		return out, errManagedSQLPublish
	}
	if string(management.UID) != in.ManagementServiceUID || management.ResourceVersion == "" || management.DeletionTimestamp != nil ||
		management.Name != name+"-management" || management.Namespace != b.Namespace || management.Spec.Type != core.ServiceTypeClusterIP ||
		net.ParseIP(management.Spec.ClusterIP) == nil || len(management.Spec.ExternalIPs) != 0 || management.Spec.ExternalName != "" ||
		!reflect.DeepEqual(management.Spec.Selector, map[string]string{"vm.neon.tech/name": name}) ||
		management.Labels["neon-control/project-id"] != b.ProjectID || management.Labels["neon-control/endpoint-id"] != b.EndpointID {
		return out, errManagedSQLPublish
	}
	portCount := 0
	for _, port := range management.Spec.Ports {
		if port.Name == "compute-control" && port.Port == 3080 && port.NodePort == 0 &&
			(port.TargetPort.IntVal == 3080 || port.TargetPort.StrVal == "compute-control") {
			portCount++
		}
	}
	if portCount != 1 {
		return out, errManagedSQLPublish
	}
	out = managedSQLRouteSource{vm: vm, config: cfg, credentials: creds, management: management, cfg: projection, startup: startup, snapshot: snap}
	return out, nil
}

// observeManagedSQLNative复用原初始化JWT与无代理/无redirect传输。只GET，不发送configure或lease请求。
func observeManagedSQLNative(ctx context.Context, client *http.Client, in managedSQLRouteInput, source managedSQLRouteSource) (dsgaInitializationResponse, error) {
	var fact dsgaInitializationResponse
	key := ed25519.NewKeyFromSeed(source.credentials.Data["controlSigningSeed"])
	token := dsgaInitializationJWT(key, source.vm.Name, in.Binding.VMUID, in.Binding.ComputeInstanceID, time.Now())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(source.management.Spec.ClusterIP, "3080")+"/dsg/v1/initialization", nil)
	if err != nil {
		return fact, errManagedSQLPublish
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Cache-Control", "no-store")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		return fact, errManagedSQLPublish
	}
	defer response.Body.Close()
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" ||
		len(response.Header.Values("Content-Type")) != 1 || mediaErr != nil || media != "application/json" || readErr != nil ||
		len(raw) > 64<<10 || dsgapi.UnmarshalObject(raw, &fact) != nil {
		return fact, errManagedSQLPublish
	}
	if validateManagedSQLNative(fact, source) != nil {
		return fact, errManagedSQLPublish
	}
	return fact, nil
}

func validateManagedSQLNative(fact dsgaInitializationResponse, source managedSQLRouteSource) error {
	p := createPayload{ProjectID: source.cfg.Spec.ProjectID, EndpointID: source.cfg.Spec.EndpointID, BranchID: source.cfg.Spec.BranchID, TenantID: source.cfg.Spec.TenantID, TimelineID: source.cfg.Spec.TimelineID}
	return fact.validateStartup(p, source.cfg.Spec.OperationUUID, source.startup)
}

// publishManagedSQLRoute是CLI和Driver共同的唯一写入口。仅409重读；未知PUT结果不自动重发。
func publishManagedSQLRoute(ctx context.Context, k *kubeClient, in managedSQLRouteInput,
	observe func(context.Context, managedSQLRouteInput, managedSQLRouteSource) (dsgaInitializationResponse, error),
	authorize func(context.Context) error) (ManagedSQLRouteReceipt, error) {
	var receipt ManagedSQLRouteReceipt
	if in.validate() != nil || k == nil || k.namespace != in.Binding.Namespace || observe == nil || in.resume != nil && in.resume.Verify == nil {
		return receipt, managedSQLRefuse("input")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for attempt := 0; attempt < 6; attempt++ {
		source, err := readManagedSQLRouteSource(ctx, k, in)
		if err != nil {
			return receipt, managedSQLRefuse("source")
		}
		fact, err := observe(ctx, in, source)
		if err != nil || validateManagedSQLNative(fact, source) != nil {
			return receipt, managedSQLRefuse("native")
		}
		nativeRaw, err := json.Marshal(fact)
		if err != nil {
			return receipt, managedSQLRefuse("encode")
		}
		b := in.Binding
		b.NativeInitializationSHA256 = managedSQLDigest(nativeRaw)
		roles := map[string]string{"app_owner": source.snapshot.credentials.ApplicationVerifier, "control_probe": source.snapshot.credentials.ProbeVerifier}
		registry, err := k.request(ctx, http.MethodGet, k.path("secret", routesSecret), nil)
		if err != nil || stringVal(nested(registry, "metadata", "uid")) != in.RegistryUID || stringVal(nested(registry, "metadata", "resourceVersion")) == "" ||
			nested(registry, "metadata", "deletionTimestamp") != nil {
			return receipt, managedSQLRefuse("registry")
		}
		raw, err := secretText(registry, "routes.json")
		routes, mapErr := managedSQLRouteMap([]byte(raw))
		if err != nil || mapErr != nil {
			return receipt, managedSQLRefuse("registry")
		}
		key := selector(b.EndpointID)
		route := map[string]json.RawMessage{}
		hadPrevious := false
		var replacement *adapter.ManagedSQLBinding
		if previous, ok := routes[key]; ok {
			var old struct {
				ProjectID string            `json:"project_id"`
				BranchID  string            `json:"branch_id"`
				Roles     map[string]string `json:"roles"`
				Binding   json.RawMessage   `json:"managed_binding"`
			}
			var oldBinding adapter.ManagedSQLBinding
			if json.Unmarshal(previous, &old) != nil || old.ProjectID != b.ProjectID || old.BranchID != b.BranchID ||
				dsgapi.UnmarshalObject(old.Binding, &oldBinding) != nil || oldBinding.Validate() != nil {
				return receipt, managedSQLRefuse("existing_binding")
			}
			if !adapter.ManagedSQLBindingEqual(oldBinding, b) {
				if in.resume != nil && !in.resume.matches(oldBinding, b) ||
					in.resume == nil && !managedSQLReplacementIdentity(oldBinding, b) {
					return receipt, managedSQLRefuse("replacement_identity")
				}
				if err := managedSQLReplacementRetired(ctx, k, oldBinding); err != nil {
					return receipt, err
				}
				replacement = &oldBinding
			}
			if old.Roles["app_owner"] != roles["app_owner"] || old.Roles["control_probe"] != roles["control_probe"] {
				return receipt, managedSQLRefuse("roles")
			}
			previousMap, policyErr := managedSQLRouteMap(previous)
			if policyErr != nil || !validManagedSQLPolicy(previousMap) {
				return receipt, managedSQLRefuse("policy")
			}
			// 包括未知VPC/network字段在内的整份原策略保留；只替换本producer负责的字段。
			route = previousMap
			hadPrevious = true

		}
		if in.resume != nil && !hadPrevious {
			return receipt, managedSQLRefuse("existing_binding")
		}
		owned := map[string]any{"project_id": b.ProjectID, "branch_id": b.BranchID, "tenant_id": b.TenantID,
			"kind": "neonvm", "workload": source.vm.Name, "address": source.vm.Name + "." + b.Namespace + ".svc.cluster.local:55432",
			"role": "app_owner", "verifier": roles["app_owner"], "roles": roles, "managed_binding": b}
		// 既有route完整保留Role/策略/未知字段；受控替换仅更新managed_binding。
		if hadPrevious {
			owned = map[string]any{"managed_binding": b}
		}
		for name, value := range owned {
			encoded, encodeErr := json.Marshal(value)
			if encodeErr != nil {
				return receipt, managedSQLRefuse("encode")
			}
			route[name] = encoded
		}
		if !hadPrevious {
			stateRaw, _ := json.Marshal("active")
			route["state"] = stateRaw
		}
		// 初次不注入allowed_ips/NodeID：沿adapter既有nil-IP默认和实际configured PS节点。

		routeRaw, err := json.Marshal(route)
		if err != nil || adapter.ValidateManagedSQLRoute(ctx, adapterKubernetes{k}, b.Namespace, key, routeRaw) != nil {
			return receipt, managedSQLRefuse("selected_route")
		}
		if authorize != nil && authorize(ctx) != nil {
			return receipt, managedSQLRefuse("authority")
		}
		if in.resume != nil && in.resume.Verify(ctx, b) != nil {
			return receipt, managedSQLRefuse("authority")
		}
		// Native读取之后、CAS之前再权威核同一新VM/immutable Config，不能复用旧事实。
		if _, err := readManagedSQLRouteSource(ctx, k, in); err != nil {
			return receipt, managedSQLRefuse("source_recheck")
		}
		if replacement != nil {
			if err := managedSQLReplacementRetired(ctx, k, *replacement); err != nil {
				return receipt, err
			}
		}
		routes[key] = routeRaw
		encoded, err := json.Marshal(routes)
		if err != nil || len(encoded) > 2<<20 {
			return receipt, managedSQLRefuse("encode")
		}
		data, ok := registry["data"].(map[string]any)
		if !ok {
			return receipt, managedSQLRefuse("registry")
		}
		data["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		updated, err := k.request(ctx, http.MethodPut, k.path("secret", routesSecret), registry)
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == http.StatusConflict {
			continue
		}
		if err != nil || stringVal(nested(updated, "metadata", "uid")) != in.RegistryUID || stringVal(nested(updated, "metadata", "resourceVersion")) == "" {
			return receipt, managedSQLRefuse("put_unconfirmed")
		}
		actual, err := secretText(updated, "routes.json")
		var actualRoutes map[string]json.RawMessage
		if err != nil || json.Unmarshal([]byte(actual), &actualRoutes) != nil || !bytes.Equal(actualRoutes[key], routeRaw) {
			return receipt, managedSQLRefuse("put_readback")
		}
		return ManagedSQLRouteReceipt{ProtocolVersion: 1, Selector: key, VMUID: b.VMUID, Incarnation: b.Incarnation, ConfigSecretUID: b.ConfigSecretUID, ConfigSHA256: b.ConfigSHA256, SecretDataSHA256: b.SecretDataSHA256, RegistryUID: in.RegistryUID, RegistryResourceVersion: stringVal(nested(updated, "metadata", "resourceVersion")), NativeInitializationSHA256: b.NativeInitializationSHA256}, nil
	}
	return receipt, managedSQLRefuse("conflict_exhausted")
}

// PublishManagedSQLRoute是正式orchestrator阶段：先读真实NativeProbe，再调用上述同一个publisher。
// 空Branch等静态错误先于Kube初始化拒绝，不接受外部URL、Native事实或路由正文。
func PublishManagedSQLRoute(ctx context.Context, kubeconfig string, input []byte) (ManagedSQLRouteReceipt, error) {
	var receipt ManagedSQLRouteReceipt
	var in managedSQLRouteInput
	if len(input) == 0 || len(input) > 64<<10 || dsgapi.UnmarshalObject(input, &in) != nil || in.validate() != nil {
		return receipt, errManagedSQLPublish
	}
	var k *kubeClient
	var err error
	if kubeconfig == "" {
		k, err = newKubeClient()
	} else {
		config, e := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if e != nil {
			return receipt, errManagedSQLPublish
		}
		client, e := rest.HTTPClientFor(config)
		if e != nil {
			return receipt, errManagedSQLPublish
		}
		k = &kubeClient{base: config.Host, namespace: in.Binding.Namespace, token: config.BearerToken, tokenFile: config.BearerTokenFile, http: client}
	}
	if err != nil || k == nil || k.namespace != in.Binding.Namespace {
		return receipt, errManagedSQLPublish
	}
	client := newDSGAProxy(nil).observer
	client.Timeout = 35 * time.Second
	defer client.CloseIdleConnections()
	return publishManagedSQLRoute(ctx, k, in, func(ctx context.Context, in managedSQLRouteInput, source managedSQLRouteSource) (dsgaInitializationResponse, error) {
		return observeManagedSQLNative(ctx, client, in, source)
	}, nil)
}

// publishInitializedSQLRoute从原Operation/lease重载，不把prepare/seal当初始化，也不走Legacy publisher。
func (binding *dsgaCreationBinding) publishInitializedSQLRoute(ctx context.Context, fresh dsgaInitializationResponse) error {
	current, err := binding.server.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
	if err != nil {
		return errManagedSQLPublish
	}
	plan, err := current.creationPlan(ctx)
	if err != nil || current.vmUID == "" || current.configUID == "" || current.intent.BranchID == "" || fresh.configSHA256 == "" || fresh.secretDataSHA256 == "" {
		return errManagedSQLPublish
	}
	k := current.server.kube
	p := current.intent.createPayload
	name := kubeName(p.EndpointID)
	config := new(core.Secret)
	credentials := new(core.Secret)
	sqlService := new(core.Service)
	management := new(core.Service)
	if managedSQLRead(k, ctx, "secret", name+"-config", config) != nil || managedSQLRead(k, ctx, "secret", name+"-credentials", credentials) != nil ||
		managedSQLRead(k, ctx, "service", name, sqlService) != nil || managedSQLRead(k, ctx, "service", name+"-management", management) != nil {
		return errManagedSQLPublish
	}
	snap, err := snapshotDSGACredentials(credentials, plan.Namespace, name+"-credentials", p.ProjectID, p.EndpointID)
	if err != nil || snap.reference() != plan.Credentials {
		return errManagedSQLPublish
	}
	registry, err := k.request(ctx, http.MethodGet, k.path("secret", routesSecret), nil)
	if err != nil {
		return errManagedSQLPublish
	}
	in := managedSQLRouteInput{ProtocolVersion: 1, RegistryUID: stringVal(nested(registry, "metadata", "uid")), ManagementServiceUID: string(management.UID),
		Binding: adapter.ManagedSQLBinding{ProtocolVersion: 1, Namespace: plan.Namespace, ProjectID: p.ProjectID, EndpointID: p.EndpointID, BranchID: p.BranchID, TenantID: p.TenantID, TimelineID: p.TimelineID,
			VMUID: current.vmUID, Incarnation: plan.Incarnation, ComputeInstanceID: plan.ComputeInstanceID, ConfigSecretUID: current.configUID,
			ConfigSHA256: fresh.configSHA256, SecretDataSHA256: fresh.secretDataSHA256, CredentialSecretUID: string(credentials.UID), CredentialDataSHA256: managedSQLDataDigest(credentials.Data), ServiceUID: string(sqlService.UID)}}
	if current.action == dsgaResumeAction {
		source, e := readDSGAResumeSource(ctx, current.server.db, p, current.intent.ResumeFrom)
		if e != nil || source.PGBootID == fresh.Observed.Instance.PGBootID || source.VMUID == current.vmUID || source.ConfigUID == current.configUID {
			return errManagedSQLPublish
		}
		in.resume = source.routeProof(func(ctx context.Context, selected adapter.ManagedSQLBinding) error {
			latest, e := readDSGAResumeSource(ctx, current.server.db, p, current.intent.ResumeFrom)
			if e != nil || latest != source || !adapter.ManagedSQLBindingEqual(selected, in.Binding) {
				return errManagedSQLPublish
			}
			uid, e := current.serviceIdentity(ctx, "postgres", nil)
			if e != nil || uid != selected.ServiceUID || uid == source.ServiceUID {
				return errManagedSQLPublish
			}
			return nil
		})
	}
	_, err = publishManagedSQLRoute(ctx, k, in, func(ctx context.Context, in managedSQLRouteInput, source managedSQLRouteSource) (dsgaInitializationResponse, error) {
		// 仅复用本次Driver刚读取、严格验证且已记录的响应；不读取旧持久fact。
		// publisher仍在CAS前fresh读取VM/Config及原UID/hash，CLI不能传入该事实。
		if validateManagedSQLNative(fresh, source) != nil {
			return dsgaInitializationResponse{}, errManagedSQLPublish
		}
		return fresh, nil
	}, func(ctx context.Context) error {
		latest, e := current.server.loadDSGACreationBinding(ctx, current.operationID, current.workerID)
		if e != nil || latest.vmUID != current.vmUID || latest.configUID != current.configUID || latest.digest != current.digest {
			return errManagedSQLPublish
		}
		return nil
	})
	return err
}
