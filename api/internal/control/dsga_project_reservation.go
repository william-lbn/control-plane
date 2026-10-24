package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

var errDSGACreationConflict = errors.New("original DSGA creation key or inputs differ")
var errDSGACreationAuthority = errors.New("current organization creation authority unavailable")
var errDSGAProjectBounds = errors.New("requested PG17 resources are outside the trusted class")

// 可信进程配置，不是API body。Bounds同时给默认范围和本规格允许的外边界，
// 公开请求只能收窄；真正Guest/PG预算仍由原计划和后续领域执行器约束。
type dsgaProjectPolicy struct {
	ProtocolVersion uint32             `json:"protocol_version"`
	Region          string             `json:"region"`
	Bounds          createBounds       `json:"bounds"`
	Creation        dsgaCreationPolicy `json:"creation"`
}

// 非敏感投影：类型中没有Password字段，避免将HTTP原body写入元数据库。
// 明文只交给prepareDSGACredentials；原请求关联使用已有带密钥HMAC摘要。
type dsgaProjectDescription struct {
	Name, DefaultBranch, Region string
	Bounds                      *createBounds
}

type dsgaCreationDraft struct {
	ProtocolVersion uint32           `json:"protocol_version"`
	ResourceName    string           `json:"resource_name"`
	DefaultBranch   string           `json:"default_branch"`
	Region          string           `json:"region"`
	Payload         createPayload    `json:"payload"`
	Plan            dsgaCreationPlan `json:"plan"`
}

type dsgaProjectReservation struct {
	Actor, KeyHash, RequestHash, Scope, Org, Operation string
	Draft                                              dsgaCreationDraft
	Credentials                                        dsgaCredentialReference
	Accepted                                           bool
}

func decodeDSGAObject(raw []byte, into any) error {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return errDSGACreationPlan
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(into) != nil || decoder.Decode(new(any)) != io.EOF {
		return errDSGACreationPlan
	}
	return nil
}

func readDSGAProjectPolicy(namespace string) (dsgaProjectPolicy, error) {
	var policy dsgaProjectPolicy
	if decodeDSGAObject([]byte(os.Getenv("NEON_DSGA_CREATION_POLICY")), &policy) != nil || policy.ProtocolVersion != 1 ||
		policy.Creation.Namespace != namespace || policy.Region == "" || len(policy.Region) > 128 || strings.TrimSpace(policy.Region) != policy.Region ||
		policy.Creation.VM.validateResourceBudget(policy.Creation.Budget) != nil || policy.Creation.Storage.validate(namespace) != nil || !dsgaPinnedImage.MatchString(policy.Creation.Image) || len(namespace) > 63 || !dsgaPlanNamespace.MatchString(namespace) || policy.Bounds.MinCPU < 250 || policy.Bounds.MinCPU%250 != 0 || policy.Bounds.MaxCPU < policy.Bounds.MinCPU || policy.Bounds.MaxCPU%250 != 0 || policy.Bounds.MaxCPU > 1<<31-1 ||
		policy.Bounds.MinMem <= 0 || policy.Bounds.MaxMem < policy.Bounds.MinMem || policy.Bounds.MaxMem > 1<<31-1 {
		return policy, errDSGACreationPlan
	}
	// 先验证平台默认规格本身，区分服务配置错误与客户端非法规格。只有
	// 合法class内的请求才会保留随机计划，不能让调用者偶然绕开坏默认值。
	slot, budget := policy.Creation.MemorySlotBytes, policy.Creation.Budget
	minBytes, maxBytes := uint64(policy.Bounds.MinMem)*(1<<20), uint64(policy.Bounds.MaxMem)*(1<<20)
	if slot == 0 || slot%(8<<20) != 0 || minBytes%slot != 0 || maxBytes%slot != 0 || maxBytes/slot > 512 || budget.validate() != nil ||
		budget.MinimumGuestMemoryBytes < slot || budget.MinimumGuestMemoryBytes%slot != 0 || budget.MinimumGuestMemoryBytes > minBytes {
		return policy, errDSGACreationPlan
	}
	return policy, nil
}

// 固定锁序：org→user→membership→quota→reservation→新Operation。
// 组织管理操作先锁org，因此此处NO KEY UPDATE串行权限变更又兼容正常FK读取。
// 每次外部写前准备及最终接受均重核当前权限，不仅依赖已通过的HTTP middleware。
func lockDSGAProjectAuthority(ctx context.Context, tx pgx.Tx, org, actor string) error {
	var state, role string
	var disabled bool
	if err := tx.QueryRow(ctx, `SELECT state FROM organizations WHERE id=$1 FOR NO KEY UPDATE`, org).Scan(&state); err != nil {
		return dsgaAuthorityReadError(err)
	}
	if state != "active" {
		return errDSGACreationAuthority
	}
	if err := tx.QueryRow(ctx, `SELECT disabled FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&disabled); err != nil {
		return dsgaAuthorityReadError(err)
	}
	if disabled {
		return errDSGACreationAuthority
	}
	if err := tx.QueryRow(ctx, `SELECT role FROM organization_members WHERE org_id=$1 AND user_id=$2 FOR SHARE`, org, actor).Scan(&role); err != nil {
		return dsgaAuthorityReadError(err)
	}
	if roleLevel(role) < 2 {
		return errDSGACreationAuthority
	}
	return lockDSGARequestCredential(ctx, tx, org, actor)
}

// 预留占用也计入配额：不能在Kube反复生成孤立Secret而绕过资源数量限制。
// quota行先锁住，随后才锁reservation；接受事务在同一锁序内将pending转换为
// 实际资源，二者不会同时对其他事务可见而重复计数。历史accepted恢复不新增占用。
func lockDSGAReservationQuota(ctx context.Context, tx pgx.Tx, org string) (int, int, error) {
	var projects, endpoints int
	err := tx.QueryRow(ctx, `SELECT max_projects,max_endpoints FROM organization_quotas WHERE org_id=$1 FOR UPDATE`, org).Scan(&projects, &endpoints)
	return projects, endpoints, err
}

func checkDSGAReservationQuota(ctx context.Context, tx pgx.Tx, org string, projectLimit, endpointLimit int, existing bool) error {
	var projects, endpoints int
	err := tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM projects WHERE org_id=$1 AND deleted_at IS NULL)+
 (SELECT count(*) FROM dsga_creation_reservations WHERE org_id=$1 AND base_action='create_project' AND accepted_id IS NULL),
 (SELECT count(*) FROM endpoints e JOIN projects p ON p.id=e.project_id WHERE p.org_id=$1 AND e.deleted_at IS NULL)+
 (SELECT count(*) FROM dsga_creation_reservations WHERE org_id=$1 AND accepted_id IS NULL AND COALESCE(draft#>>'{payload,endpoint_id}','')<>'')`, org).Scan(&projects, &endpoints)
	if err != nil {
		return err
	}
	if existing {
		if projects > projectLimit || endpoints > endpointLimit {
			return errOrganizationQuota
		}
	} else if projects >= projectLimit || endpoints >= endpointLimit {
		return errOrganizationQuota
	}
	return nil
}

func readDSGAReservation(ctx context.Context, tx pgx.Tx, actor, keyDigest string) (dsgaProjectReservation, error) {
	r := dsgaProjectReservation{Actor: actor, KeyHash: keyDigest}
	var raw, reference []byte
	var accepted *string
	var base string
	err := tx.QueryRow(ctx, `SELECT request_hash,scope,org_id,base_action,operation_id,draft,credential_reference,accepted_id
 FROM dsga_creation_reservations WHERE actor_id=$1 AND key_hash=$2 FOR UPDATE`, actor, keyDigest).
		Scan(&r.RequestHash, &r.Scope, &r.Org, &base, &r.Operation, &raw, &reference, &accepted)
	if err != nil {
		return r, err
	}
	if base != "create_project" || decodeDSGAObject(raw, &r.Draft) != nil || r.Draft.ProtocolVersion != 1 || r.Draft.Plan.validate(r.Draft.Payload) != nil ||
		r.Draft.Plan.VM.validateResourceBudget(r.Draft.Plan.Budget) != nil || r.Draft.Plan.Credentials != (dsgaCredentialReference{}) || dsgapi.ValidateID(r.Operation, "reserved Operation") != nil {
		return r, errDSGACreationPlan
	}
	if len(reference) > 0 && (decodeDSGAObject(reference, &r.Credentials) != nil || r.Credentials.validate() != nil) {
		return r, errDSGACreationPlan
	}
	r.Accepted = accepted != nil
	if r.Accepted && (*accepted != r.Operation || r.Credentials.validate() != nil) {
		return r, errDSGACreationPlan
	}
	return r, nil
}

func (r dsgaProjectReservation) matches(org, scope, requestHash, namespace string, body dsgaProjectDescription) bool {
	p := r.Draft.Payload
	return r.Org == org && r.Scope == scope && r.RequestHash == requestHash && r.Draft.Plan.Namespace == namespace &&
		r.Draft.ResourceName == body.Name && r.Draft.DefaultBranch == body.DefaultBranch && (body.Region == "" || body.Region == r.Draft.Region) &&
		(body.Bounds == nil || *body.Bounds == (createBounds{p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem}))
}

// reserveDSGAProject先保留同一原随机计划/Operation ID，事务提交后才允许Kube
// 写凭据。恢复先读原行，不重读policy/env、不生成新随机值；同actor/key在通用
// 幂等表和本表间也经数据库锁/守卫互斥。所有网络动作位于本函数返回之后。
func (s *server) reserveDSGAProject(ctx context.Context, actor, org, scope, key, requestHash string, body dsgaProjectDescription) (dsgaProjectReservation, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var empty dsgaProjectReservation
	if s == nil || s.db == nil || s.kube == nil || len(s.idempotencyKey) < 32 || len(key) < 8 || len(key) > 128 ||
		!strings.HasPrefix(requestHash, "h1:") || len(requestHash) != 67 || scope != "POST:/organizations/"+org+"/projects" ||
		!validName(body.Name, 64) || !validName(body.DefaultBranch, 63) {
		return empty, errDSGACreationPlan
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockDSGAProjectAuthority(ctx, tx, org, actor); err != nil {
		return empty, err
	}
	pl, el, err := lockDSGAReservationQuota(ctx, tx, org)
	if err != nil {
		return empty, err
	}
	r, err := readDSGAReservation(ctx, tx, actor, keyHash(key))
	if err == nil {
		if !r.matches(org, scope, requestHash, s.kube.namespace, body) {
			return empty, errDSGACreationConflict
		}
		if !r.Accepted {
			if err := checkDSGAReservationQuota(ctx, tx, org, pl, el, true); err != nil {
				return empty, err
			}
		}
		if err := lockDSGARequestCredential(ctx, tx, org, actor); err != nil {
			return empty, err
		}
		if err := tx.Commit(ctx); err != nil {
			return empty, err
		}
		return r, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if err := checkDSGAReservationQuota(ctx, tx, org, pl, el, false); err != nil {
		return empty, err
	}
	policy, err := readDSGAProjectPolicy(s.kube.namespace)
	if err != nil {
		return empty, err
	}
	region := body.Region
	if region == "" {
		region = policy.Region
	}
	if region != policy.Region {
		return empty, errDSGACreationPlan
	}
	b := policy.Bounds
	if body.Bounds != nil {
		b = *body.Bounds
	}
	if b.MinCPU < policy.Bounds.MinCPU || b.MaxCPU > policy.Bounds.MaxCPU || b.MinMem < policy.Bounds.MinMem || b.MaxMem > policy.Bounds.MaxMem {
		return empty, errDSGAProjectBounds
	}
	if b.MaxCPU < b.MinCPU || b.MinCPU%250 != 0 || b.MaxCPU%250 != 0 || b.MaxMem < b.MinMem ||
		(uint64(b.MinMem)*(1<<20))%policy.Creation.MemorySlotBytes != 0 || (uint64(b.MaxMem)*(1<<20))%policy.Creation.MemorySlotBytes != 0 {
		return empty, errDSGAProjectBounds
	}
	suffix := stableSuffix(actor, scope, key)
	p := creationPayload("prj_"+suffix, "br_"+suffix, "ep_"+suffix, stableHex(suffix, "tenant"), stableHex(suffix, "timeline"), b)
	p.EndpointType = "read_write"
	plan, err := newDSGACreationPlan(p, policy.Creation, time.Now())
	if err != nil {
		return empty, err
	}
	if plan.VM.validate() != nil {
		return empty, errDSGACreationPlan
	}
	var collision bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE org_id=$1 AND lower(name)=lower($2) AND deleted_at IS NULL)`, org, body.Name).Scan(&collision); err != nil {
		return empty, err
	}
	if collision {
		return empty, errDSGACreationConflict
	}
	r = dsgaProjectReservation{Actor: actor, KeyHash: keyHash(key), RequestHash: requestHash, Scope: scope, Org: org, Operation: newID("op_"), Draft: dsgaCreationDraft{1, body.Name, body.DefaultBranch, region, p, plan}}
	raw, err := json.Marshal(r.Draft)
	if err != nil {
		return empty, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO dsga_creation_reservations(actor_id,key_hash,request_hash,scope,org_id,base_action,operation_id,draft)
 VALUES($1,$2,$3,$4,$5,'create_project',$6,$7)`, actor, r.KeyHash, requestHash, scope, org, r.Operation, raw)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return empty, errDSGACreationConflict
		}
		return empty, err
	}
	if err := lockDSGARequestCredential(ctx, tx, org, actor); err != nil {
		return empty, err
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}
	return r, nil
}

// 来源UID/完整Data摘要只允许第一次固定，恢复完全只读；即使后面资源事务
// 失败，也保留该事实。再次调用凭据生产者时携带此ref，原Secret丢失不重建。
func (s *server) pinDSGAReservationCredentials(ctx context.Context, r dsgaProjectReservation, ref dsgaCredentialReference) (dsgaProjectReservation, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if ref.validate() != nil {
		return r, errDSGACredentialSource
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return r, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockDSGAProjectAuthority(ctx, tx, r.Org, r.Actor); err != nil {
		return r, err
	}
	current, err := readDSGAReservation(ctx, tx, r.Actor, r.KeyHash)
	if err != nil {
		return r, err
	}
	if current.Operation != r.Operation || current.RequestHash != r.RequestHash {
		return r, errDSGACreationConflict
	}
	if current.Credentials != (dsgaCredentialReference{}) {
		if current.Credentials != ref {
			return r, errDSGACredentialSource
		}
	} else {
		raw, _ := json.Marshal(ref)
		if _, err := tx.Exec(ctx, `UPDATE dsga_creation_reservations SET credential_reference=$3 WHERE actor_id=$1 AND key_hash=$2`, r.Actor, r.KeyHash, raw); err != nil {
			return r, err
		}
		current.Credentials = ref
	}
	if err := lockDSGARequestCredential(ctx, tx, r.Org, r.Actor); err != nil {
		return r, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, err
	}
	return current, nil
}

// 接受的线性化点是这一事务COMMIT：资源、原Operation、步骤、通用幂等键和
// reservation接受见证一起出现。此前Kube来源已单独固定；任何配额/名称/权限
// 或COMMIT未知错误都不删除原来源/换Operation。恢复先读已接受状态，不UPDATE
// 原行或重插资源。当前事务不做Kube/网络调用。
func (s *server) acceptDSGAProjectReservation(ctx context.Context, r dsgaProjectReservation, key, requestID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockDSGAProjectAuthority(ctx, tx, r.Org, r.Actor); err != nil {
		return false, err
	}
	pl, el, err := lockDSGAReservationQuota(ctx, tx, r.Org)
	if err != nil {
		return false, err
	}
	current, err := readDSGAReservation(ctx, tx, r.Actor, r.KeyHash)
	if err != nil {
		return false, err
	}
	if keyHash(key) != current.KeyHash || current.Operation != r.Operation || current.RequestHash != r.RequestHash || current.Scope != r.Scope || current.Org != r.Org || current.Credentials != r.Credentials {
		return false, errDSGACreationConflict
	}
	if current.Accepted {
		if err := lockDSGARequestCredential(ctx, tx, r.Org, r.Actor); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	if current.Credentials.validate() != nil {
		return false, errDSGACredentialSource
	}
	if err := checkDSGAReservationQuota(ctx, tx, r.Org, pl, el, true); err != nil {
		return false, err
	}
	d := current.Draft
	p := d.Payload
	plan := d.Plan
	plan.Credentials = current.Credentials
	if plan.validate(p) != nil || plan.VM.validate() != nil || plan.Namespace != s.kube.namespace {
		return false, errDSGACreationPlan
	}
	_, err = tx.Exec(ctx, `INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,default_branch_id,created_at,updated_at)
 VALUES($1,$2,$3,$4,$5,17,'provisioning','managed',$6,clock_timestamp(),clock_timestamp())`, p.ProjectID, r.Org, d.ResourceName, p.TenantID, d.Region, p.BranchID)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO project_grants(project_id,user_id,role) VALUES($1,$2,'admin')`, p.ProjectID, r.Actor); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO branches(id,project_id,name,timeline_id,is_default,protected,state,created_at)
 VALUES($1,$2,$3,$4,true,true,'creating',clock_timestamp())`, p.BranchID, p.ProjectID, d.DefaultBranch, p.TimelineID); err != nil {
		return false, err
	}
	// 不调用旧insertEndpoint：它固定cloud_admin/1024MiB槽，既不能暴露内部
	// SU，也不能丢失本计划的实际slot粒度。quota已在同一父锁下计入保留占用。
	name := kubeName(p.EndpointID)
	if _, err := tx.Exec(ctx, `INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,
 role_name,database_name,min_cpu_milli,max_cpu_milli,min_memory_mib,max_memory_mib,slot_size_mib,scale_to_zero,endpoint_type,created_at,updated_at)
 VALUES($1,$2,$3,$4,'neonvm',$5,$5,'provisioning',$6,$7,$8,$9,$10,$11,$12,false,'read_write',clock_timestamp(),clock_timestamp())`,
		p.EndpointID, p.ProjectID, p.BranchID, selector(p.EndpointID), name, dsgaApplicationRole, dsgaApplicationDatabase, p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem, int64(plan.MemorySlotBytes/(1<<20))); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version)
 VALUES($1,'postgres','provisioning','provisioning','dsga-v1')`, p.BranchID); err != nil {
		return false, err
	}
	steps := []string{"create_managed_tenant", "create_timeline_pg17", "prepare_neonvm_dsga", "wait_neonvm_running", "confirm_native_initialization", "publish_proxy_route", "sql_probe_via_proxy", "persist_ready_state"}
	if err := addDSGACreateOperation(ctx, tx, "create_project", "project", p.ProjectID, p.ProjectID, current.Operation, r.Actor, requestID, p, plan, key, current.RequestHash, steps); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE dsga_creation_reservations SET accepted_id=operation_id,accepted_at=clock_timestamp() WHERE actor_id=$1 AND key_hash=$2`, r.Actor, r.KeyHash); err != nil {
		return false, err
	}
	if err := lockDSGARequestCredential(ctx, tx, r.Org, r.Actor); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
