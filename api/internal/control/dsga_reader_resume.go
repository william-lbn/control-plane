package control

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"

	"github.com/jackc/pgx/v5"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

type dsgaReaderResumeRole struct {
	Role                                 dsgaReaderRole
	State, SourceType, Operation, Action string
	Payload                              catalogPayload
}

// 恢复只消费当前分支已提交的原生变更证明。观察/继承/重命名来源并不含
// 当前 Timeline 的 native mutation 回执，不能把其 source_id 当作写入操作。
// 此类旧帐号保留 metadata、在本 Reader 的 Proxy 白名单中暂不授予；它们
// 不妨碍其他有完整证明的帐号恢复。已声称存在的原生证明损坏则拒绝本轮。
func readDSGAReaderResumeRoles(ctx context.Context, tx pgx.Tx, p createPayload) ([]dsgaReaderResumeRole, int, error) {
	if p.EndpointType != "read_only" || !(catalogNativeScope{p.TenantID, p.TimelineID}).valid() {
		return nil, 0, errDSGAReaderCatalog
	}
	rows, err := tx.Query(ctx, `SELECT r.name,r.state,r.version,COALESCE(r.credential_ref,''),
 COALESCE(c.id,''),COALESCE(c.identity_id,''),COALESCE(c.object_oid,0),COALESCE(c.intent_version,0),
 COALESCE(c.credential_ref,''),COALESCE(c.credential_uid,''),COALESCE(c.tenant_id,''),COALESCE(c.timeline_id,''),COALESCE(c.state,''),
 COALESCE(c.source_type,''),COALESCE(c.source_id,''),
 EXISTS(SELECT 1 FROM catalog_fork_exclusions x WHERE x.project_id=$1 AND x.branch_id=$2 AND x.name=r.name)
 OR EXISTS(SELECT 1 FROM catalog_inheritance_exclusions x WHERE x.project_id=$1 AND x.branch_id=$2 AND x.kind='role' AND x.name=r.name)
 FROM branch_roles r JOIN branches b ON b.id=r.branch_id AND b.project_id=$1
 LEFT JOIN catalog_object_bindings c ON c.project_id=$1 AND c.branch_id=r.branch_id AND c.kind='role' AND c.name=r.name AND c.intent_version=r.version
 WHERE r.branch_id=$2 ORDER BY r.name LIMIT 1025`, p.ProjectID, p.BranchID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []dsgaReaderResumeRole{}
	count, omitted := 0, 0
	for rows.Next() {
		var item dsgaReaderResumeRole
		var version int64
		var credential, tenant, timeline, state string
		var excluded bool
		r := &item.Role.Ref
		if err = rows.Scan(&item.Role.Name, &item.State, &version, &credential, &r.ID, &r.IdentityID, &r.OID, &r.Version,
			&r.CredentialRef, &r.CredentialUID, &tenant, &timeline, &state, &item.SourceType, &item.Operation, &excluded); err != nil {
			return nil, 0, err
		}
		count++
		if count > catalogInventoryLimit {
			return nil, 0, errCatalogInventoryBound
		}
		if excluded {
			omitted++
			continue
		}
		if item.State != "ready" && item.State != "deleted" {
			return nil, 0, errDSGAReaderCatalog
		}
		if item.SourceType != "create_result" && item.SourceType != "mutation_result" {
			omitted++
			continue
		}
		if r.ID == "" || r.IdentityID == "" || r.OID == 0 || r.Version != version || version < 2 || state != item.State ||
			tenant != p.TenantID || timeline != p.TimelineID || !dsgaCatalogRoleName(item.Role.Name) ||
			!functionResourceUID.MatchString(r.CredentialUID) || r.CredentialRef == "" ||
			(item.State == "ready" && credential != r.CredentialRef) || !dsgaCatalogRoleOperationID.MatchString(item.Operation) {
			return nil, 0, errDSGAReaderCatalog
		}
		out = append(out, item)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close()
	proven := make([]dsgaReaderResumeRole, 0, len(out))
	for i := range out {
		item := &out[i]
		var raw, result []byte
		var digest string
		var nativeWriter bool
		if err = tx.QueryRow(ctx, `SELECT o.action,o.payload,r.payload_sha256,r.result,
 EXISTS(SELECT 1 FROM operations boot JOIN dsga_creation_bindings b ON b.operation_id=boot.id
 WHERE boot.project_id=o.project_id AND boot.action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1','resume_endpoint_dsga_v1')
 AND boot.state='succeeded' AND boot.finished_at<=o.created_at AND b.vm_uid IS NOT NULL AND b.config_uid IS NOT NULL
 AND boot.payload->>'endpoint_id'=o.payload->>'endpoint_id' AND boot.payload->>'branch_id'=$3
 AND boot.payload->>'endpoint_type'='read_write' AND boot.payload->>'tenant_id'=$4 AND boot.payload->>'timeline_id'=$5
 AND EXISTS(SELECT 1 FROM dsga_creation_supply_facts f WHERE f.operation_id=boot.id AND f.kind='runtime_selection'))
 FROM operations o JOIN catalog_mutation_results r ON r.operation_id=o.id AND r.project_id=o.project_id AND r.branch_id=o.resource_id AND r.action=o.action
 WHERE o.id=$1 AND o.project_id=$2 AND o.resource_type='branch_catalog' AND o.resource_id=$3
 AND o.state='succeeded' AND o.finished_at IS NOT NULL AND o.lease_owner IS NULL AND o.lease_expires_at IS NULL`, item.Operation, p.ProjectID, p.BranchID, p.TenantID, p.TimelineID).
			Scan(&item.Action, &raw, &digest, &result, &nativeWriter); err != nil {
			return nil, 0, errDSGAReaderCatalog
		}
		// Legacy 的同名 result 不等于当前原生记录。没有在原 mutation 前完成
		// 资格链的 Writer 时，仅省略此帐号，不发注定无法证实的 refresh 请求。
		if !nativeWriter {
			omitted++
			continue
		}
		if json.Unmarshal(raw, &item.Payload) != nil || validateDSGAReaderResumeMutation(p, *item, digest, result) != nil {
			return nil, 0, errDSGAReaderCatalog
		}
		if item.Action == "create_role" {
			var oid uint32
			if err = tx.QueryRow(ctx, `SELECT object_oid FROM catalog_create_observations WHERE operation_id=$1 AND project_id=$2 AND branch_id=$3 AND payload_sha256=$4`, item.Operation, p.ProjectID, p.BranchID, digest).Scan(&oid); err != nil || oid != item.Role.Ref.OID {
				return nil, 0, errDSGAReaderCatalog
			}
		}
		proven = append(proven, *item)
	}
	return proven, omitted, nil
}

func validateDSGAReaderResumeMutation(p createPayload, item dsgaReaderResumeRole, digest string, rawResult []byte) error {
	v, ref := item.Payload, item.Role.Ref
	if v.NativeIdentity == nil || v.NativeIdentity.Scope != (catalogNativeScope{p.TenantID, p.TimelineID}) ||
		v.ProjectID != p.ProjectID || v.BranchID != p.BranchID || v.Name != item.Role.Name ||
		!functionWriterEndpointID.MatchString(v.EndpointID) || v.ExpectedVersion < 1 || v.ExpectedVersion == int64(^uint64(0)>>1) ||
		ref.Version != v.ExpectedVersion+1 || digest != catalogMutationDigest(item.Action, v) {
		return errDSGAReaderCatalog
	}
	var result struct {
		Name, Kind, State string
		Version           string `json:"version_token"`
	}
	if json.Unmarshal(rawResult, &result) != nil || result.Name != item.Role.Name || result.Kind != "role" || result.State != item.State || result.Version != strconv.FormatInt(ref.Version, 10) {
		return errDSGAReaderCatalog
	}
	switch item.Action {
	case "create_role":
		if item.SourceType != "create_result" || item.State != "ready" || v.NativeIdentity.Object != (catalogObjectRef{}) {
			return errDSGAReaderCatalog
		}
	case "rotate_role_password", "delete_role":
		old := v.NativeIdentity.Object
		if item.SourceType != "mutation_result" || old.OID != ref.OID || old.IdentityID != ref.IdentityID || old.Version != v.ExpectedVersion || old.ID == "" {
			return errDSGAReaderCatalog
		}
		if (item.Action == "delete_role") != (item.State == "deleted") {
			return errDSGAReaderCatalog
		}
	default:
		return errDSGAReaderCatalog
	}
	if item.State == "ready" && (v.CredentialRef != ref.CredentialRef || v.MutationCredentialUID != ref.CredentialUID) {
		return errDSGAReaderCatalog
	}
	return nil
}

// 此阶段只调用既有只读 Reader refresh：签的是原已提交操作的 generation，
// 不是恢复 Operation 的序号。每次请求和最终白名单 CAS 都重读当前 metadata、
// immutable 凭据、原 Worker lease 与新 PG boot；不向 Reader 赋予目录写权限。
func (binding *dsgaCreationBinding) publishResumedReaderCatalog(ctx context.Context, tx pgx.Tx, runtime *dsgaEndpointRuntime, in managedSQLRouteInput, expected dsgapi.Instance) error {
	p, s := runtime.Payload, binding.server
	items, omitted, err := readDSGAReaderResumeRoles(ctx, tx, p)
	if err != nil {
		return err
	}
	roles := []dsgaReaderRole{}
	for _, item := range items {
		if item.State == "ready" {
			roles = append(roles, item.Role)
		}
	}
	credentials, err := s.readDSGAReaderCredentials(ctx, p, roles)
	if err != nil {
		return err
	}
	authorizeOriginal := func(ctx context.Context) error {
		latest, e := s.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
		if e != nil || latest.action != dsgaResumeAction || latest.vmUID != binding.vmUID || latest.configUID != binding.configUID || latest.digest != binding.digest {
			return errDSGAReaderCatalog
		}
		return binding.checkLease(ctx, tx)
	}
	// branch 行锁与目录 admission/commit 共用；原绑定、Operation/result 和
	// 凭据均不可变。逐请求只复核直接相关角色，避免每角色重扫全目录造成
	// O(n²) 恢复。原 VM/PG boot 仍由签名 client 在请求前后独立复核。
	authorizeRole := func(item dsgaReaderResumeRole) func(context.Context) error {
		return func(ctx context.Context) error {
			if e := authorizeOriginal(ctx); e != nil {
				return e
			}
			var state, credential, id string
			var version int64
			if e := tx.QueryRow(ctx, `SELECT r.state,r.version,COALESCE(r.credential_ref,''),c.id
 FROM branch_roles r JOIN catalog_object_bindings c ON c.branch_id=r.branch_id AND c.kind='role' AND c.name=r.name AND c.intent_version=r.version
 WHERE r.branch_id=$1 AND r.name=$2 AND c.project_id=$3`, p.BranchID, item.Role.Name, p.ProjectID).Scan(&state, &version, &credential, &id); e != nil ||
				state != item.State || version != item.Role.Ref.Version || id != item.Role.Ref.ID || (state == "ready" && credential != item.Role.Ref.CredentialRef) {
				return errDSGAReaderCatalog
			}
			if state == "ready" {
				actual, e := s.readDSGAReaderCredentials(ctx, p, []dsgaReaderRole{item.Role})
				if e != nil || actual[item.Role.Name] != credentials[item.Role.Name] {
					return errDSGAReaderCatalog
				}
			}
			return nil
		}
	}
	authorizeAll := func(ctx context.Context) error {
		if e := authorizeOriginal(ctx); e != nil {
			return e
		}
		current, currentOmitted, e := readDSGAReaderResumeRoles(ctx, tx, p)
		if e != nil || currentOmitted != omitted || !reflect.DeepEqual(current, items) {
			return errDSGAReaderCatalog
		}
		actual, e := s.readDSGAReaderCredentials(ctx, p, roles)
		if e != nil || !reflect.DeepEqual(actual, credentials) {
			return errDSGAReaderCatalog
		}
		_, _, _, selected, e := s.readDSGABoundSelection(ctx, runtime)
		if e != nil || selected.Instance != expected {
			return errDSGAReaderCatalog
		}
		return binding.checkLease(ctx, tx)
	}
	for _, item := range items {
		input, e := readerResumeRefreshInput(p, item, credentials[item.Role.Name], expected)
		if e != nil {
			return e
		}
		if e = s.applyDSGAServiceLogin(ctx, runtime, input, authorizeRole(item)); e != nil {
			return e
		}
	}
	if err = s.observeDSGAReaderRoles(ctx, runtime, roles, expected); err != nil {
		return err
	}
	if err = publishDSGAReaderRoles(ctx, s.kube, in, credentials, authorizeAll); err != nil {
		return err
	}
	if omitted != 0 && s.logger != nil {
		s.logger.Warn("Reader resume withheld unproven catalog identities", "operation_id", binding.operationID, "count", omitted, "reason", "original_native_mutation_unconfirmed")
	}
	return nil
}

// 原操作证明不包含本次启动代次；此处只把已核验的原业务请求绑定到当前
// Reader，不将恢复 Operation、旧 Writer PG boot 或父 Timeline 充作新来源。
func readerResumeRefreshInput(p createPayload, item dsgaReaderResumeRole, verifier string, expected dsgapi.Instance) (dsgaAuthLoginRequest, error) {
	if p.EndpointType != "read_only" || expected.PGBootID == 0 || expected.VMUID == "" || expected.ComputeInstanceID == "" ||
		item.Payload.ProjectID != p.ProjectID || item.Payload.BranchID != p.BranchID || item.Payload.NativeIdentity == nil ||
		item.Payload.NativeIdentity.Scope != (catalogNativeScope{p.TenantID, p.TimelineID}) {
		return dsgaAuthLoginRequest{}, errDSGAReaderCatalog
	}
	original := item.Payload
	original.operationID = item.Operation
	input, err := dsgaCatalogRoleRequest(original, item.Action, verifier)
	if err != nil {
		return dsgaAuthLoginRequest{}, err
	}
	input.Service = dsgaCatalogRoleRefreshLogin
	input.EndpointID = p.EndpointID
	input.TenantID, input.TimelineID = p.TenantID, p.TimelineID
	input.PGBootID = expected.PGBootID
	input.CatalogRole.ExpectedOID = item.Role.Ref.OID
	if !validDSGAReaderCatalogRefresh(input) {
		return dsgaAuthLoginRequest{}, errDSGAReaderCatalog
	}
	return input, nil
}
