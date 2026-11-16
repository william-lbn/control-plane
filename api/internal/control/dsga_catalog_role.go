package control

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/william-lbn/control-plane/api/internal/adapter"
	"github.com/william-lbn/control-plane/api/internal/functionsql"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var dsgaCatalogRoleOperationID = regexp.MustCompile(`^op_[0-9a-f]{24}$`)

type dsgaCatalogRoleIntent struct {
	OperationID string `json:"operation_id"`
	Action      string `json:"action"`
	Name        string `json:"name"`
	ExpectedOID uint32 `json:"expected_oid"`
}

type dsgaCatalogRoleReceipt struct {
	OperationID string `json:"operation_id"`
	Action      string `json:"action"`
	OID         uint32 `json:"role_oid"`
}

func dsgaCatalogRoleName(name string) bool {
	if !validPGIdentifier(name) || protectedPGRole(name) || strings.HasPrefix(name, "/") {
		return false
	}
	lower := strings.ToLower(name)
	if lower == dsgaApplicationRole || lower == "zenith_admin" || lower == "public" || lower == "none" {
		return false
	}
	for _, prefix := range []string{"rtp_", "rtr_", "dsga_"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	return true
}

func (in dsgaCatalogRoleIntent) valid(verifier *string) bool {
	if !dsgaCatalogRoleOperationID.MatchString(in.OperationID) || !dsgaCatalogRoleName(in.Name) {
		return false
	}
	switch in.Action {
	case "create":
		return in.ExpectedOID == 0 && verifier != nil && functionsql.ValidVerifier(*verifier)
	case "rotate":
		return in.ExpectedOID != 0 && verifier != nil && functionsql.ValidVerifier(*verifier)
	case "delete":
		return in.ExpectedOID != 0 && verifier == nil
	default:
		return false
	}
}

func validDSGAReaderCatalogRefresh(input dsgaAuthLoginRequest) bool {
	if input.CatalogRole == nil || input.CatalogRole.ExpectedOID == 0 ||
		!(catalogNativeScope{TenantID: input.TenantID, TimelineID: input.TimelineID}).valid() ||
		input.Database != "postgres" || input.FunctionID != "" || input.Schema != "" ||
		input.RealtimeID != "" || input.Relations != nil {
		return false
	}
	role := *input.CatalogRole
	if role.Action == "create" {
		// Writer CREATE has no expected OID. Reader CREATE requires the OID
		// already observed from that exact committed Writer operation.
		role.ExpectedOID = 0
	}
	return role.valid(input.Verifier)
}

func dsgaCatalogRoleRequest(p catalogPayload, action, verifier string) (dsgaAuthLoginRequest, error) {
	role := dsgaCatalogRoleIntent{OperationID: p.operationID, Name: p.Name, ExpectedOID: p.NativeIdentity.Object.OID}
	var credential *string
	switch action {
	case "create_role":
		role.Action, credential = "create", &verifier
	case "rotate_role_password":
		role.Action, credential = "rotate", &verifier
	case "delete_role":
		role.Action = "delete"
	default:
		return dsgaAuthLoginRequest{}, errDSGACreationIdentity
	}
	return dsgaAuthLoginRequest{ProtocolVersion: 1, Service: dsgaCatalogRoleServiceLogin, ProjectID: p.ProjectID,
		BranchID: p.BranchID, EndpointID: p.EndpointID, Database: "postgres", Generation: dsgapi.Uint64(p.ExpectedVersion),
		CatalogRole: &role, Verifier: credential}, nil
}

func (s *server) authorizeDSGACatalogRole(ctx context.Context, original *dsgaEndpointRuntime, p catalogPayload, action string) error {
	latest, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.EndpointID)
	if err != nil {
		return err
	}
	if latest == nil || latest.Operation != original.Operation || latest.VMUID != original.VMUID ||
		latest.ConfigUID != original.ConfigUID || latest.Payload.BranchID != p.BranchID {
		return errDSGACreationIdentity
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = verifyCatalogMutationOperationTx(ctx, tx, p.operationID, p.workerID, action, p); err != nil {
		return err
	}
	if err = lockCatalogMutationIntentTx(ctx, tx, p.operationID, action, p); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DSGA 的启动凭据、Compute 配置和管理 Service 固定到原 Worker 创建绑定。
// 角色写入走当前 Writer 上签名、短期、逐请求认证的已有限定入口；原 PostgreSQL
// 在同一事务中持久化角色 OID 和 Operation，续跑时不按角色名收养外部对象。
func (s *server) applyDSGACatalogRole(ctx context.Context, runtime *dsgaEndpointRuntime, p catalogPayload, action, verifier string, knownOID uint32) error {
	if p.NativeIdentity == nil || p.ExpectedVersion < 1 || runtime.Payload.BranchID != p.BranchID ||
		runtime.Payload.TenantID != p.NativeIdentity.Scope.TenantID || runtime.Payload.TimelineID != p.NativeIdentity.Scope.TimelineID {
		return errDSGACreationIdentity
	}
	input, err := dsgaCatalogRoleRequest(p, action, verifier)
	if err != nil {
		return err
	}
	receipt, err := s.applyDSGAServiceLoginResult(ctx, runtime, input, func(ctx context.Context) error {
		return s.authorizeDSGACatalogRole(ctx, runtime, p, action)
	})
	if err != nil {
		return err
	}
	if input.CatalogRole.Action == "create" {
		oid := receipt.CatalogRole.OID
		if knownOID != 0 && knownOID != oid {
			return errDSGACreationIdentity
		}
		if err = s.recordCatalogCreateObservation(ctx, p, action, oid); err != nil {
			return err
		}
		knownOID = oid
	}
	conn, err := s.dsgaBackendConnection(ctx, runtime, p.BranchID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return verifyCatalogNativeBinding(ctx, conn, p, action, knownOID, true)
}

// 在变更代理凭据之前，要求仍运行于原分支的 Reader 观察到 Writer
// 事务与原 OID，并确认本地 HBA 已加载。管理请求只绑定该 Reader 启动代次。
func (s *server) refreshDSGAReaderCatalogRole(ctx context.Context, reader *dsgaEndpointRuntime, p catalogPayload, action, verifier string) error {
	writer, err := s.verifyDSGACatalogEndpoint(ctx, p, p.EndpointID)
	if err != nil {
		return err
	}
	if writer == nil || writer.Payload.EndpointType != "read_write" {
		return errDSGACreationIdentity
	}
	input, err := dsgaCatalogRoleRequest(p, action, verifier)
	if err != nil {
		return err
	}
	if input.CatalogRole.Action == "create" {
		input.CatalogRole.ExpectedOID, err = s.catalogCreateObservation(ctx, p, action)
		if err != nil {
			return err
		}
	}
	input.Service = dsgaCatalogRoleRefreshLogin
	input.EndpointID = reader.Payload.EndpointID
	input.TenantID, input.TimelineID = p.NativeIdentity.Scope.TenantID, p.NativeIdentity.Scope.TimelineID
	return s.applyDSGAServiceLogin(ctx, reader, input, func(ctx context.Context) error {
		latest, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, input.EndpointID)
		if err != nil {
			return err
		}
		if latest == nil || latest.Operation != reader.Operation || latest.VMUID != reader.VMUID ||
			latest.ConfigUID != reader.ConfigUID || latest.Payload.BranchID != p.BranchID ||
			latest.Payload.TenantID != input.TenantID || latest.Payload.TimelineID != input.TimelineID {
			return errDSGACreationIdentity
		}
		return s.authorizeDSGACatalogRole(ctx, writer, p, action)
	})
}

func (s *server) verifyDSGACatalogEndpoint(ctx context.Context, p catalogPayload, id string) (*dsgaEndpointRuntime, error) {
	runtime, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, id)
	if err != nil || runtime == nil {
		return runtime, err
	}
	if p.NativeIdentity == nil || runtime.Payload.BranchID != p.BranchID ||
		runtime.Payload.TenantID != p.NativeIdentity.Scope.TenantID || runtime.Payload.TimelineID != p.NativeIdentity.Scope.TimelineID {
		return nil, errDSGACreationIdentity
	}
	client, vm, _, _, err := s.readDSGABoundSelection(ctx, runtime)
	if err != nil {
		return nil, err
	}
	config, err := client.CoreV1().Secrets(runtime.Plan.Namespace).Get(ctx, vm.Name+"-config", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if string(config.UID) != runtime.ConfigUID || bootstrap.CheckReady(vm, config, false) != nil {
		return nil, errDSGACreationIdentity
	}
	return runtime, nil
}

func (s *server) verifyDSGACatalogRoute(ctx context.Context, p catalogPayload, endpoint string, route map[string]any) error {
	runtime, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, endpoint)
	if err != nil {
		return err
	}
	rawBinding, hasBinding := route["managed_binding"]
	if runtime == nil && !hasBinding {
		return nil
	}
	if runtime == nil || !hasBinding || p.NativeIdentity == nil || route["state"] != "active" {
		return errDSGACreationIdentity
	}
	raw, err := json.Marshal(rawBinding)
	if err != nil {
		return err
	}
	var bound adapter.ManagedSQLBinding
	if dsgapi.UnmarshalObject(raw, &bound) != nil || bound.Validate() != nil ||
		bound.ProjectID != p.ProjectID || bound.BranchID != p.BranchID || bound.EndpointID != endpoint ||
		bound.TenantID != p.NativeIdentity.Scope.TenantID || bound.TimelineID != p.NativeIdentity.Scope.TimelineID ||
		bound.VMUID != runtime.VMUID || bound.ConfigSecretUID != runtime.ConfigUID ||
		bound.ComputeInstanceID != runtime.Plan.ComputeInstanceID || bound.Incarnation != runtime.Plan.Incarnation {
		return errDSGACreationIdentity
	}
	raw, err = json.Marshal(route)
	if err != nil {
		return err
	}
	return adapter.ValidateManagedSQLRoute(ctx, adapterKubernetes{s.kube}, runtime.Plan.Namespace, selector(endpoint), raw)
}
