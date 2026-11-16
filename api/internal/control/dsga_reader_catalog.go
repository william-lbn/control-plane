package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var errDSGAReaderCatalog = errors.New("original Reader catalog authorization is unconfirmed")

type dsgaReaderRole struct {
	Name string
	Ref  catalogObjectRef
}

// 首次 Reader 的授权清单只来自当前分支已持久化的同代 identity。LEFT JOIN
// 使缺失原 OID/凭据 UID 的 ready 意图显式失败；不从历史 baseline、父分支
// 或现场同名 pg_roles 自动创建 binding。明确排除项和删除 tombstone 不授予登录。
func readDSGAReaderRoles(ctx context.Context, db catalogForkReader, p createPayload) ([]dsgaReaderRole, error) {
	if p.EndpointType != "read_only" || !(catalogNativeScope{p.TenantID, p.TimelineID}).valid() {
		return nil, errDSGAReaderCatalog
	}
	rows, err := db.Query(ctx, `SELECT r.name,r.state,r.version,COALESCE(r.credential_ref,''),
 COALESCE(c.id,''),COALESCE(c.identity_id,''),COALESCE(c.object_oid,0),COALESCE(c.intent_version,0),
 COALESCE(c.credential_ref,''),COALESCE(c.credential_uid,''),COALESCE(c.tenant_id,''),COALESCE(c.timeline_id,''),COALESCE(c.state,''),
 EXISTS(SELECT 1 FROM catalog_fork_exclusions x WHERE x.project_id=$1 AND x.branch_id=$2 AND x.name=r.name)
 OR EXISTS(SELECT 1 FROM catalog_inheritance_exclusions x WHERE x.project_id=$1 AND x.branch_id=$2 AND x.kind='role' AND x.name=r.name)
 FROM branch_roles r JOIN branches b ON b.id=r.branch_id AND b.project_id=$1
 LEFT JOIN catalog_object_bindings c ON c.project_id=$1 AND c.branch_id=r.branch_id AND c.kind='role' AND c.name=r.name AND c.intent_version=r.version
 WHERE r.branch_id=$2 ORDER BY r.name LIMIT 1025`, p.ProjectID, p.BranchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dsgaReaderRole{}
	count := 0
	for rows.Next() {
		var role dsgaReaderRole
		var state, credential, tenant, timeline, boundState string
		var version int64
		var excluded bool
		ref := &role.Ref
		if err = rows.Scan(&role.Name, &state, &version, &credential, &ref.ID, &ref.IdentityID, &ref.OID, &ref.Version,
			&ref.CredentialRef, &ref.CredentialUID, &tenant, &timeline, &boundState, &excluded); err != nil {
			return nil, err
		}
		count++
		if count > catalogInventoryLimit {
			return nil, errCatalogInventoryBound
		}
		if excluded || state == "deleted" {
			continue
		}
		if state != "ready" || boundState != "ready" || version < 1 || ref.Version != version || ref.ID == "" ||
			ref.IdentityID == "" || ref.OID == 0 || !dsgaCatalogRoleName(role.Name) || credential == "" || credential != ref.CredentialRef ||
			!functionResourceUID.MatchString(ref.CredentialUID) || tenant != p.TenantID || timeline != p.TimelineID {
			return nil, errDSGAReaderCatalog
		}
		out = append(out, role)
	}
	return out, rows.Err()
}

// 现场快照仅验证既有原 OID，绝不把快照中额外角色扩充为授权清单。
// 普通登录不可包含管理属性；成员关系及实时 HBA 仍由原生资格链独立约束。
func verifyDSGAReaderRoleSnapshot(p createPayload, roles []dsgaReaderRole, snapshot catalogNativeSnapshot) error {
	scope := catalogNativeScope{p.TenantID, p.TimelineID}
	if p.EndpointType != "read_only" || validateCatalogNativeSnapshot(snapshot) != nil || snapshot.Scope != scope || len(roles) > catalogInventoryLimit {
		return errDSGAReaderCatalog
	}
	seen := map[string]bool{}
	for _, role := range roles {
		if seen[role.Name] || !dsgaCatalogRoleName(role.Name) || role.Ref.OID == 0 ||
			compareCatalogCurrentIdentity(scope, p.BranchID, "role", role.Name, "", role.Ref, snapshot).Status != "catalog_matches" {
			return errDSGAReaderCatalog
		}
		seen[role.Name] = true
		for _, actual := range snapshot.Roles {
			if actual.OID == role.Ref.OID {
				f := actual.Flags
				if !f.Login || f.Superuser || f.CreateRole || f.CreateDB || f.Replication || f.BypassRLS {
					return errDSGAReaderCatalog
				}
			}
		}
	}
	return nil
}

func (s *server) readDSGAReaderCredentials(ctx context.Context, p createPayload, roles []dsgaReaderRole) (map[string]string, error) {
	result := map[string]string{}
	for _, role := range roles {
		var secret core.Secret
		if managedSQLRead(s.kube, ctx, "secret", role.Ref.CredentialRef, &secret) != nil ||
			secret.Name != role.Ref.CredentialRef || secret.Namespace != s.kube.namespace || string(secret.UID) != role.Ref.CredentialUID ||
			secret.ResourceVersion == "" || secret.DeletionTimestamp != nil || secret.Immutable == nil || !*secret.Immutable || secret.Type != core.SecretTypeOpaque ||
			secret.Labels["neon-control/project-id"] != p.ProjectID || !functionsql.ValidVerifier(string(secret.Data["verifier"])) {
			return nil, errDSGAReaderCatalog
		}
		result[role.Name] = string(secret.Data["verifier"])
	}
	return result, nil
}

// 专用 Reader 观察连接不会扩大 dsgaBackendConnection 的 Writer 写入边界。
// 使用原随机探针凭据和已发布的原 UID 代理路由；事务强制只读，真实 recovery
// 与 Tenant/Timeline 必须匹配，VM/Pod/PG boot 在连接两侧重复核验。
func (s *server) observeDSGAReaderRoles(ctx context.Context, runtime *dsgaEndpointRuntime, roles []dsgaReaderRole, expected dsgapi.Instance) error {
	if runtime == nil || runtime.Payload.EndpointType != "read_only" {
		return errDSGAReaderCatalog
	}
	clients, vm, _, selected, err := s.readDSGABoundSelection(ctx, runtime)
	if err != nil {
		return err
	}
	if selected.Instance != expected {
		return errDSGAReaderCatalog
	}
	secret, err := clients.CoreV1().Secrets(vm.Namespace).Get(ctx, vm.Name+"-credentials", metav1.GetOptions{})
	if err != nil {
		return errDSGACredentialSource
	}
	password, err := readDSGABoundProbePassword(secret, runtime)
	if err != nil {
		return err
	}
	conn, err := connectSQL(ctx, s.proxyHost, s.proxyPort, "control_probe", password, "postgres", selector(runtime.Payload.EndpointID))
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var recovery bool
	if err = tx.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&recovery); err != nil || !recovery {
		return errDSGAReaderCatalog
	}
	snapshot, err := readCatalogNativeSnapshotTx(ctx, tx, catalogNativeScope{runtime.Payload.TenantID, runtime.Payload.TimelineID})
	if err != nil {
		return err
	}
	if err = verifyDSGAReaderRoleSnapshot(runtime.Payload, roles, snapshot); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	_, fresh, _, latest, err := s.readDSGABoundSelection(ctx, runtime)
	if err != nil || fresh.UID != vm.UID || fresh.Status.PodName != vm.Status.PodName || latest.Instance != selected.Instance {
		return errDSGAReaderCatalog
	}
	return nil
}

// Reader 初次创建及原 suspension 恢复共用入口。原 branch 行锁串行化目录准入；不持有
// Operation 行锁做外部请求，所以 heartbeat 可续租。SQL proxy/adapter 仅读 Kube
// 路由及原来源，不回取此 branch 锁。整个区段含锁等待限时 45 秒，重试不改 SQL、
// immutable Config 或角色 identity；发生不明 CAS 结果时只在下一轮重新读回确认。
func (binding *dsgaCreationBinding) publishReaderCatalog(ctx context.Context, in managedSQLRouteInput, expected dsgapi.Instance) error {
	p := binding.intent.createPayload
	if (binding.action != "create_endpoint_dsga_v1" && binding.action != dsgaResumeAction) || p.EndpointType != "read_only" {
		return errDSGAReaderCatalog
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	s := binding.server
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var scope catalogNativeScope
	if err = tx.QueryRow(ctx, `SELECT pr.tenant_id,b.timeline_id FROM branches b JOIN projects pr ON pr.id=b.project_id
 WHERE pr.id=$1 AND b.id=$2 AND pr.state='ready' AND pr.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL FOR UPDATE OF b`, p.ProjectID, p.BranchID).Scan(&scope.TenantID, &scope.TimelineID); err != nil || scope != (catalogNativeScope{p.TenantID, p.TimelineID}) {
		return errDSGAReaderCatalog
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND id<>$3
 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, p.ProjectID, p.BranchID, binding.operationID).Scan(&pending); err != nil || pending {
		return errDSGAReaderCatalog
	}
	if err = binding.checkLease(ctx, tx); err != nil {
		return err
	}
	var roles []dsgaReaderRole
	if binding.action != dsgaResumeAction {
		roles, err = readDSGAReaderRoles(ctx, tx, p)
		if err != nil {
			return err
		}
	}
	// 无普通受管角色时沿既有基础路由，不引入额外 SQL 或写入。
	if len(roles) == 0 && binding.action != dsgaResumeAction {
		return binding.checkLease(ctx, tx)
	}
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return err
	}
	runtime := &dsgaEndpointRuntime{Operation: binding.operationID, Payload: p, Plan: plan, VMUID: binding.vmUID, ConfigUID: binding.configUID}
	if binding.action == dsgaResumeAction {
		return binding.publishResumedReaderCatalog(ctx, tx, runtime, in, expected)
	}
	credentials, err := s.readDSGAReaderCredentials(ctx, p, roles)
	if err != nil {
		return err
	}
	if err = s.observeDSGAReaderRoles(ctx, runtime, roles, expected); err != nil {
		return err
	}
	authorize := func(ctx context.Context) error {
		latest, err := s.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
		if err != nil || latest.vmUID != binding.vmUID || latest.configUID != binding.configUID || latest.digest != binding.digest {
			return errDSGAReaderCatalog
		}
		current, err := readDSGAReaderRoles(ctx, tx, p)
		if err != nil || !reflect.DeepEqual(current, roles) {
			return errDSGAReaderCatalog
		}
		actual, err := s.readDSGAReaderCredentials(ctx, p, roles)
		if err != nil || !reflect.DeepEqual(actual, credentials) {
			return errDSGAReaderCatalog
		}
		_, _, _, selected, err := s.readDSGABoundSelection(ctx, runtime)
		if err != nil || selected.Instance != expected {
			return errDSGAReaderCatalog
		}
		return binding.checkLease(ctx, tx)
	}
	return publishDSGAReaderRoles(ctx, s.kube, in, credentials, authorize)
}

// 此 producer 只补齐经过原 SQL 读回验证的普通角色。基础角色、已有网络策略和
// 其他 endpoint 原文全部保留；同名不同 verifier 或清单外角色不被收养。相同
// 读回直接返回，不反复 PUT，已运行 Reader 的实时 HBA 不由此函数改写。
func publishDSGAReaderRoles(ctx context.Context, k *kubeClient, in managedSQLRouteInput, credentials map[string]string, authorize func(context.Context) error) error {
	if in.validate() != nil || authorize == nil || len(credentials) > catalogInventoryLimit {
		return errDSGAReaderCatalog
	}
	for name, verifier := range credentials {
		if !dsgaCatalogRoleName(name) || !functionsql.ValidVerifier(verifier) {
			return errDSGAReaderCatalog
		}
	}
	for attempt := 0; attempt < 6; attempt++ {
		source, err := readManagedSQLRouteSource(ctx, k, in)
		if err != nil {
			return err
		}
		registry, err := k.request(ctx, http.MethodGet, k.path("secret", routesSecret), nil)
		if err != nil || stringVal(nested(registry, "metadata", "uid")) != in.RegistryUID || stringVal(nested(registry, "metadata", "resourceVersion")) == "" || nested(registry, "metadata", "deletionTimestamp") != nil {
			return errDSGAReaderCatalog
		}
		raw, err := secretText(registry, "routes.json")
		routes, decodeErr := managedSQLRouteMap([]byte(raw))
		if err != nil || decodeErr != nil {
			return errDSGAReaderCatalog
		}
		key := selector(in.Binding.EndpointID)
		route, err := managedSQLRouteMap(routes[key])
		var original adapter.ManagedSQLBinding
		var roles map[string]string
		if err != nil || dsgapi.UnmarshalObject(route["managed_binding"], &original) != nil || original.Validate() != nil ||
			!adapter.ManagedSQLBindingEqual(original, in.Binding) || string(route["state"]) != `"active"` ||
			json.Unmarshal(route["roles"], &roles) != nil || roles == nil ||
			roles["app_owner"] != source.snapshot.credentials.ApplicationVerifier || roles["control_probe"] != source.snapshot.credentials.ProbeVerifier ||
			adapter.ValidateManagedSQLRoute(ctx, adapterKubernetes{k}, in.Binding.Namespace, key, routes[key]) != nil {
			return errDSGAReaderCatalog
		}
		for name, verifier := range roles {
			if name != "app_owner" && name != "control_probe" && credentials[name] != verifier {
				return errDSGAReaderCatalog
			}
		}
		changed := false
		for name, verifier := range credentials {
			if roles[name] != verifier {
				roles[name], changed = verifier, true
			}
		}
		if err = authorize(ctx); err != nil {
			return err
		}
		if _, err = readManagedSQLRouteSource(ctx, k, in); err != nil {
			return err
		}
		if !changed {
			return nil
		}
		route["roles"], err = json.Marshal(roles)
		if err != nil {
			return errDSGAReaderCatalog
		}
		routeRaw, err := json.Marshal(route)
		if err != nil || adapter.ValidateManagedSQLRoute(ctx, adapterKubernetes{k}, in.Binding.Namespace, key, routeRaw) != nil {
			return errDSGAReaderCatalog
		}
		routes[key] = routeRaw
		encoded, err := json.Marshal(routes)
		if err != nil || len(encoded) > 2<<20 {
			return errDSGAReaderCatalog
		}
		registry["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		updated, err := k.request(ctx, http.MethodPut, k.path("secret", routesSecret), registry)
		if kubeStatusIs(err, http.StatusConflict) {
			continue
		}
		if err != nil || stringVal(nested(updated, "metadata", "uid")) != in.RegistryUID || stringVal(nested(updated, "metadata", "resourceVersion")) == "" {
			return errDSGAReaderCatalog
		}
		actual, err := secretText(updated, "routes.json")
		observed, decodeErr := managedSQLRouteMap([]byte(actual))
		if err != nil || decodeErr != nil || !bytes.Equal(observed[key], routeRaw) {
			return errDSGAReaderCatalog
		}
		return nil
	}
	return errDSGAReaderCatalog
}
