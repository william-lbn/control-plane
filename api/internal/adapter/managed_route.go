package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
	core "k8s.io/api/core/v1"
)

// ManagedSQLBinding只绑定已封存实例的SQL路由，不授予冷创建、缩容或资格许可。
type ManagedSQLBinding struct {
	ProtocolVersion            uint32 `json:"protocol_version"`
	Namespace                  string `json:"namespace"`
	ProjectID                  string `json:"project_id"`
	EndpointID                 string `json:"endpoint_id"`
	BranchID                   string `json:"branch_id"`
	TenantID                   string `json:"tenant_id"`
	TimelineID                 string `json:"timeline_id"`
	VMUID                      string `json:"vm_uid"`
	Incarnation                string `json:"incarnation"`
	ComputeInstanceID          string `json:"compute_instance_id"`
	ConfigSecretUID            string `json:"config_secret_uid"`
	ConfigSHA256               string `json:"config_sha256"`
	SecretDataSHA256           string `json:"secret_data_sha256"`
	CredentialSecretUID        string `json:"credential_secret_uid"`
	CredentialDataSHA256       string `json:"credential_data_sha256"`
	ServiceUID                 string `json:"service_uid"`
	NativeInitializationSHA256 string `json:"native_initialization_sha256"`
}

var managedSQLHash = regexp.MustCompile("^[0-9a-f]{64}$")
var managedSQLID = regexp.MustCompile("^[A-Za-z0-9_-]{1,128}$")
var managedSQLHexID = regexp.MustCompile("^[0-9a-f]{32}$")
var errManagedSQLRoute = errors.New("managed SQL route identity or source differs")

// ValidateIdentity供publisher在真实NativeProbe之前检查输入；不接受未知版本或空Branch。
func (b ManagedSQLBinding) ValidateIdentity() error {
	if b.ProtocolVersion != 1 || !dnsName.MatchString(b.Namespace) ||
		!strings.HasPrefix(b.EndpointID, "ep_") || !selectorName.MatchString("ep-"+strings.TrimPrefix(b.EndpointID, "ep_")) ||
		dsgapi.ValidateIncarnation(b.Incarnation) != nil ||
		!managedSQLHexID.MatchString(b.TenantID) || !managedSQLHexID.MatchString(b.TimelineID) ||
		!managedSQLHash.MatchString(b.ConfigSHA256) || !managedSQLHash.MatchString(b.SecretDataSHA256) || !managedSQLHash.MatchString(b.CredentialDataSHA256) {
		return errManagedSQLRoute
	}
	for _, id := range []string{b.ProjectID, b.EndpointID, b.BranchID, b.VMUID, b.ComputeInstanceID, b.ConfigSecretUID, b.CredentialSecretUID, b.ServiceUID} {
		if !managedSQLID.MatchString(id) {
			return errManagedSQLRoute
		}
	}
	return nil
}
func (b ManagedSQLBinding) Validate() error {
	if b.ValidateIdentity() != nil || !managedSQLHash.MatchString(b.NativeInitializationSHA256) {
		return errManagedSQLRoute
	}
	return nil
}

// ManagedSQLConfiguration只投影强哈希固定的公开身份、Startup及角色verifier；不得记录整个返回值。
type ManagedSQLConfiguration struct {
	Spec struct {
		ProjectID     string          `json:"project_id"`
		EndpointID    string          `json:"endpoint_id"`
		BranchID      string          `json:"branch_id"`
		TenantID      string          `json:"tenant_id"`
		TimelineID    string          `json:"timeline_id"`
		OperationUUID string          `json:"operation_uuid"`
		Dynamic       json.RawMessage `json:"dynamic_shared_buffers"`
		Cluster       struct {
			Roles []struct {
				Name     string `json:"name"`
				Verifier string `json:"encrypted_password"`
			} `json:"roles"`
		} `json:"cluster"`
	} `json:"spec"`
}

func DecodeManagedSQLConfiguration(raw []byte, b ManagedSQLBinding) (ManagedSQLConfiguration, error) {
	var c ManagedSQLConfiguration
	// 先拒绝重复键；仅对已经逐字节SHA固定的整个配置做身份投影。
	var document map[string]json.RawMessage
	if len(raw) == 0 || len(raw) > 1<<20 || dsgapi.ValidateJSON(raw, 1<<20, 65536) != nil || json.Unmarshal(raw, &document) != nil || document == nil ||
		json.Unmarshal(raw, &c) != nil || c.Spec.ProjectID != b.ProjectID || c.Spec.EndpointID != b.EndpointID ||
		c.Spec.BranchID != b.BranchID || c.Spec.TenantID != b.TenantID || c.Spec.TimelineID != b.TimelineID ||
		!managedSQLID.MatchString(c.Spec.OperationUUID) || len(c.Spec.Dynamic) == 0 {
		return c, errManagedSQLRoute
	}
	var dynamic struct {
		ProtocolVersion   uint32 `json:"protocol_version"`
		VMUID             string `json:"vm_uid"`
		ComputeInstanceID string `json:"compute_instance_id"`
	}
	if json.Unmarshal(c.Spec.Dynamic, &dynamic) != nil || !dsgapi.SupportedNativeBootstrapVersion(dynamic.ProtocolVersion) || dynamic.VMUID != b.VMUID || dynamic.ComputeInstanceID != b.ComputeInstanceID {
		return c, errManagedSQLRoute
	}
	return c, nil
}

func (s *Server) managedSQLBinding(key string, item route) (ManagedSQLBinding, error) {
	var b ManagedSQLBinding
	if len(item.ManagedBinding) == 0 || dsgapi.UnmarshalObject(item.ManagedBinding, &b) != nil || b.Validate() != nil ||
		key != "ep-"+strings.TrimPrefix(b.EndpointID, "ep_") || item.ProjectID != b.ProjectID || item.BranchID != b.BranchID ||
		item.TenantID != b.TenantID || item.Kind != "neonvm" || item.Workload != "cp-"+strings.TrimPrefix(b.EndpointID, "ep_") ||
		b.Namespace != s.config.Namespace || len(item.Template) != 0 || item.Role != "app_owner" ||
		item.Address != item.Workload+"."+s.config.Namespace+".svc.cluster.local:55432" {
		return b, errManagedSQLRoute
	}
	return b, nil
}

// ManagedSQLSecretDataSHA256沿原Helm dataHash，绑定完整Config Secret.Data三文档输入。
// 这不是bootstrap.Runtime canonical document的SHA，不能冒作旧runtime_sha256。
func ManagedSQLSecretDataSHA256(data map[string][]byte) string {
	raw, _ := json.Marshal(data)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func managedSQLDecodeMap(input map[string]any, target any) error {
	raw, err := json.Marshal(input)
	if err != nil || json.Unmarshal(raw, target) != nil {
		return errManagedSQLRoute
	}
	return nil
}

// validateManagedSQLRoute每次进入SQL访问/唤醒时读取真实对象。没有任何写API或创建降级路径。
func (s *Server) validateManagedSQLRoute(ctx context.Context, key string, item route) error {
	b, err := s.managedSQLBinding(key, item)
	if err != nil {
		return err
	}
	get := func(kind, name string, target any) error {
		m, e := s.kube.Request(ctx, http.MethodGet, s.path(kind, name), nil)
		if e != nil {
			return errManagedSQLRoute
		}
		return managedSQLDecodeMap(m, target)
	}
	vm := new(vmv1.VirtualMachine)
	config := new(core.Secret)
	service := new(core.Service)
	if get("vm", item.Workload, vm) != nil || get("secrets", item.Workload+"-config", config) != nil || get("services", item.Workload, service) != nil {
		return errManagedSQLRoute
	}
	boot := vm.Spec.DSGABootstrap
	if string(vm.UID) != b.VMUID || vm.Namespace != b.Namespace || vm.Name != item.Workload || vm.ResourceVersion == "" || vm.DeletionTimestamp != nil ||
		!dsgapi.Managed(vm.Annotations) || boot == nil || !boot.Ready || boot.Incarnation != b.Incarnation ||
		boot.VMUID != b.VMUID || boot.ComputeInstanceID != b.ComputeInstanceID || boot.ConfigSecretUID != b.ConfigSecretUID ||
		vm.Labels["neon-control/project-id"] != b.ProjectID || vm.Labels["neon-control/endpoint-id"] != b.EndpointID ||
		bootstrap.CheckReady(vm, config, false) != nil {
		return errManagedSQLRoute
	}
	raw := config.Data["config.json"]
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != b.ConfigSHA256 || ManagedSQLSecretDataSHA256(config.Data) != b.SecretDataSHA256 {
		return errManagedSQLRoute
	}
	c, err := DecodeManagedSQLConfiguration(raw, b)
	if err != nil {
		return err
	}
	// Adapter不读取原凭据Secret/签名seed。producer负责原f617来源核验；
	// 这里仅与原immutable Config的两角色verifier交叉，完整runtimeSHA已先固定。
	roleSource := map[string]string{}
	for _, name := range []string{"app_owner", "control_probe"} {
		count := 0
		for _, r := range c.Spec.Cluster.Roles {
			if r.Name == name {
				roleSource[name] = r.Verifier
				count++
			}
		}
		if count != 1 || !functionsql.ValidVerifier(roleSource[name]) || item.Roles[name] != roleSource[name] {
			return errManagedSQLRoute
		}
	}
	if item.Verifier != roleSource["app_owner"] || len(item.Roles) > 128 {
		return errManagedSQLRoute
	}
	for name, value := range item.Roles {
		if len(name) == 0 || len(name) > 63 || !functionsql.ValidVerifier(value) {
			return errManagedSQLRoute
		}
	}
	if string(service.UID) != b.ServiceUID || service.Name != item.Workload || service.Namespace != b.Namespace ||
		service.ResourceVersion == "" || service.DeletionTimestamp != nil || service.Spec.Type != core.ServiceTypeClusterIP ||
		net.ParseIP(service.Spec.ClusterIP) == nil || len(service.Spec.ExternalIPs) != 0 || service.Spec.ExternalName != "" ||
		!reflect.DeepEqual(service.Spec.Selector, map[string]string{"vm.neon.tech/name": item.Workload}) {
		return errManagedSQLRoute
	}
	postgres := 0
	for _, port := range service.Spec.Ports {
		if port.Name == "postgres" {
			if port.Port != 55432 || port.TargetPort.StrVal != "postgres" || port.TargetPort.IntVal != 0 || port.NodePort != 0 ||
				(port.Protocol != "" && port.Protocol != core.ProtocolTCP) {
				return errManagedSQLRoute
			}
			postgres++
		}
	}
	guestPostgres := 0
	for _, port := range vm.Spec.Guest.Ports {
		if port.Name == "postgres" && port.Port == 55432 {
			guestPostgres++
		}
	}
	if postgres != 1 || guestPostgres != 1 {
		return errManagedSQLRoute
	}
	// 再读原VM，避免把读取配置/Service期间的同名替换当成原实例。
	last := new(vmv1.VirtualMachine)
	if get("vm", item.Workload, last) != nil || string(last.UID) != b.VMUID || last.DeletionTimestamp != nil ||
		last.Spec.DSGABootstrap == nil || last.Spec.DSGABootstrap.Incarnation != b.Incarnation ||
		last.Spec.DSGABootstrap.ConfigSecretUID != b.ConfigSecretUID || last.Spec.DSGABootstrap.VMUID != b.VMUID || last.Spec.DSGABootstrap.ComputeInstanceID != b.ComputeInstanceID || !last.Spec.DSGABootstrap.Ready {
		return errManagedSQLRoute
	}
	return nil
}

// ValidateManagedSQLRoute复用adapter正式校验，publisher在每次CAS前也使用同一规则。
func ValidateManagedSQLRoute(ctx context.Context, kube Kubernetes, namespace, key string, raw []byte) error {
	var item route
	if dsgapi.ValidateJSON(raw, 1<<20, 65536) != nil || decodeDocument(raw, &item) != nil {
		return errManagedSQLRoute
	}
	s := &Server{config: Config{Namespace: namespace}, kube: kube}
	return s.validateManagedSQLRoute(ctx, key, item)
}

func (s *Server) wakeManagedSQLRoute(ctx context.Context, key string, item route) error {
	if s.sameRoute(ctx, key, item) != nil || s.validateManagedSQLRoute(ctx, key, item) != nil {
		return errManagedSQLRoute
	}
	vm, err := s.kube.Request(ctx, http.MethodGet, s.path("vm", item.Workload), nil)
	b, e := s.managedSQLBinding(key, item)
	if err != nil || e != nil || nested(vm, "metadata", "uid") != b.VMUID || nested(vm, "metadata", "deletionTimestamp") != nil ||
		nested(vm, "status", "phase") != "Running" {
		return errManagedSQLRoute
	}
	return s.sameRoute(ctx, key, item)
}

// managedBindingEqual忽略新一次只读NativeProbe的内容摘要，其余来源身份必须逐字段相同。
func ManagedSQLBindingEqual(a, b ManagedSQLBinding) bool {
	a.NativeInitializationSHA256 = ""
	b.NativeInitializationSHA256 = ""
	return a == b
}
