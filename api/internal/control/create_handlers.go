package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type createBounds struct {
	MinCPU int `json:"min_cpu_milli"`
	MaxCPU int `json:"max_cpu_milli"`
	MinMem int `json:"min_memory_mib"`
	MaxMem int `json:"max_memory_mib"`
}

func defaults(bounds *createBounds) createBounds {
	if bounds == nil {
		return createBounds{1000, 2000, 1024, 3072}
	}
	return *bounds
}

// Preserve historical integer profiles. Fractional preview profiles follow the
// verified agent compute unit (250m / 1Gi) and this cluster's 3Gi ceiling.
func (b createBounds) valid() bool {
	if b.MinCPU < 250 || b.MaxCPU > 2000 || b.MinCPU > b.MaxCPU || b.MinMem < 1024 || b.MaxMem > 3072 || b.MinMem > b.MaxMem || b.MinCPU%250 != 0 || b.MaxCPU%250 != 0 || b.MinMem%1024 != 0 || b.MaxMem%1024 != 0 {
		return false
	}
	if b.MinCPU%1000 == 0 && b.MaxCPU%1000 == 0 {
		return true
	}
	return b.MaxCPU <= 750 && b.MinMem == b.MinCPU/250*1024 && b.MaxMem == b.MaxCPU/250*1024
}
func cpuQuantityText(milli int) string {
	if milli%1000 == 0 {
		return fmt.Sprint(milli / 1000)
	}
	return fmt.Sprintf("%dm", milli)
}
func vmCPUValue(milli int) any {
	if milli%1000 == 0 {
		return milli / 1000
	}
	return cpuQuantityText(milli)
}

var resourceName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _.-]{0,63}$`)

func validName(name string, max int) bool {
	return len(name) <= max && resourceName.MatchString(name) && strings.TrimSpace(name) == name
}

func creationEnabled() bool { return os.Getenv("NEON_V2_CREATE_ENABLED") == "true" }

// Creation bodies contain a database password. A keyed digest prevents an
// offline dictionary attack against the request hash stored in PostgreSQL.
func (s *server) createRequestHash(scope string, value any) string {
	b, _ := json.Marshal(value)
	mac := hmac.New(sha256.New, s.idempotencyKey)
	_, _ = mac.Write([]byte(scope + ":"))
	_, _ = mac.Write(b)
	return "h1:" + hex.EncodeToString(mac.Sum(nil))
}

func creationKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		fail(w, r, 422, "invalid_idempotency_key", "Idempotency-Key must contain 8-128 characters")
		return "", false
	}
	return key, true
}

func conflictCode(err error) (int, string, string) {
	if errors.Is(err, errOrganizationQuota) {
		return 429, "organization_quota_exceeded", "Organization resource quota exceeded"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 409, "name_or_resource_conflict", "A resource with this name or identifier already exists"
	}
	return 503, "metadata_unavailable", "Could not commit resource operation"
}

func (s *server) finishCreateError(w http.ResponseWriter, r *http.Request, tx pgx.Tx,
	key, hash, legacyHash, kind string, err error) {
	_ = tx.Rollback(r.Context())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && s.replayCreate(w, r, key, hash, legacyHash, kind) {
		return
	}
	var fault *catalogRenameFault
	if errors.As(err, &fault) {
		fail(w, r, fault.Status, fault.Code, fault.Message)
		return
	}
	status, code, message := conflictCode(err)
	fail(w, r, status, code, message)
}

func (s *server) replayCreate(w http.ResponseWriter, r *http.Request, key, hash, legacyHash, kind string) bool {
	var storedHash, operationID string
	err := s.db.QueryRow(r.Context(), `SELECT request_hash,operation_id FROM idempotency_keys
		WHERE actor_id=$1 AND key_hash=$2`, userFrom(r).ID, keyHash(key)).Scan(&storedHash, &operationID)
	if isNoRows(err) {
		return false
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check idempotency")
		return true
	}
	if storedHash != hash && storedHash != legacyHash {
		fail(w, r, 409, "idempotency_conflict", "Key used with different input")
		return true
	}
	var projectID, resourceID string
	err = s.db.QueryRow(r.Context(), "SELECT project_id,resource_id FROM operations WHERE id=$1", operationID).Scan(&projectID, &resourceID)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not replay operation")
		return true
	}
	s.acceptCreate(w, r, projectID, resourceID, operationID, kind)
	return true
}

func (s *server) acceptCreate(w http.ResponseWriter, r *http.Request, projectID, resourceID, operationID, kind string) {
	table := map[string]string{"project": "projects", "branch": "branches", "endpoint": "endpoints"}[kind]
	if table == "" {
		fail(w, r, 500, "internal_error", "Unsupported resource kind")
		return
	}
	resource, err := s.one(r.Context(), "SELECT * FROM "+table+" WHERE id=$1", resourceID)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load accepted resource")
		return
	}
	op, err := s.operationRecord(r.Context(), operationID, projectID)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load accepted operation")
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+projectID+"/operations/"+operationID)
	jsonResponse(w, 202, map[string]any{"resource": resource, "operation": op})
}

func addCreateOperation(ctx context.Context, tx pgx.Tx, action, resourceType, resourceID, projectID, operationID, actor, requestID string, payload createPayload, key, hash string, steps []string) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return addCreateOperationJSON(ctx, tx, action, resourceType, resourceID, projectID, operationID, actor, requestID, b, key, hash, steps)
}

// 原PG16与版本化DSGA生产者共享写入/步骤/idempotency事务；只接受各自已经
// 校验并编码的完整payload，不在此解释模式或把未知字段解码成float64后重写。
func addCreateOperationJSON(ctx context.Context, tx pgx.Tx, action, resourceType, resourceID, projectID, operationID, actor, requestID string, b []byte, key, hash string, steps []string) error {
	_, err := tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
		VALUES($1,$2,$3,$4,$5,'queued',$6,$7,$8)`, operationID, projectID, resourceType, resourceID, action, actor, requestID, b)
	if err != nil {
		return err
	}
	for ordinal, name := range steps {
		if _, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state)
			VALUES($1,$2,$3,'queued')`, operationID, ordinal, name); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id)
		VALUES($1,$2,$3,$4)`, actor, keyHash(key), hash, operationID)
	return err
}

func insertEndpoint(ctx context.Context, tx pgx.Tx, p createPayload) error {
	if err := checkDefaultOwnerReservation(ctx, tx, p.ProjectID, p.BranchID); err != nil {
		return err
	}
	if err := endpointQuota(ctx, tx, p.ProjectID); err != nil {
		return err
	}
	name := kubeName(p.EndpointID)
	endpointType := p.EndpointType
	if endpointType == "" {
		endpointType = "read_write"
	}
	var database string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT database_name FROM default_database_owners WHERE project_id=$1 AND branch_id=$2 AND state='ready'),
 (SELECT database_name FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND endpoint_type='read_write' AND deleted_at IS NULL LIMIT 1),'postgres')`, p.ProjectID, p.BranchID).Scan(&database); err != nil {
		return err
	}
	if !validPGIdentifier(database) || protectedPGDatabase(database) && database != "postgres" {
		return errors.New("original endpoint default database rejected")
	}
	_, err := tx.Exec(ctx, `INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,
		service_name,state,role_name,database_name,min_cpu_milli,max_cpu_milli,min_memory_mib,max_memory_mib,
		slot_size_mib,scale_to_zero,endpoint_type,created_at,updated_at)
		VALUES($1,$2,$3,$4,'neonvm',$5,$5,'provisioning','cloud_admin',$11,$6,$7,$8,$9,1024,false,$10,now(),now())`,
		p.EndpointID, p.ProjectID, p.BranchID, selector(p.EndpointID), name, p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem, endpointType, database)
	return err
}

func createSteps(action string, withEndpoint bool) []string {
	steps := []string{}
	if action == "create_project" {
		steps = append(steps, "create_managed_tenant")
	}
	if action == "create_project" || action == "create_branch" {
		steps = append(steps, "create_timeline")
	}
	if withEndpoint {
		steps = append(steps, "create_neonvm", "wait_neonvm_running", "publish_proxy_route", "sql_probe_via_proxy")
	}
	return append(steps, "persist_ready_state")
}

func creationPayload(projectID, branchID, endpointID, tenantID, timelineID string, b createBounds) createPayload {
	return createPayload{ProjectID: projectID, BranchID: branchID, EndpointID: endpointID,
		TenantID: tenantID, TimelineID: timelineID, MinCPU: b.MinCPU, MaxCPU: b.MaxCPU, MinMem: b.MinMem, MaxMem: b.MaxMem}
}

type projectCreateRequest struct {
	Name              string        `json:"name"`
	Password          string        `json:"password"`
	DefaultBranchName string        `json:"default_branch_name"`
	RegionID          string        `json:"region_id"`
	PostgresVersion   int           `json:"postgres_version"`
	Autoscaling       *createBounds `json:"autoscaling"`
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	if !creationEnabled() {
		fail(w, r, 503, "creation_disabled", "Creation driver is not enabled")
		return
	}
	var body projectCreateRequest
	if err := readJSON(r, &body); err != nil {
		fail(w, r, 422, "invalid_request", "Invalid project request")
		return
	}
	if body.DefaultBranchName == "" {
		body.DefaultBranchName = "main"
	}
	if body.PostgresVersion == 0 {
		body.PostgresVersion = 16
	}
	// PG17显式进入版本化DSGA生产者；默认仍关闭，不借旧PG16凭据/Driver
	// 忽略新增计划。原Region默认只在legacy分支应用，新路径先读原保留计划。
	if body.PostgresVersion == 17 {
		s.createDSGAProject(w, r, body)
		return
	}
	if body.RegionID == "" {
		body.RegionID = "rke2-lab"
	}
	bounds := defaults(body.Autoscaling)
	if !validName(body.Name, 64) || !validName(body.DefaultBranchName, 63) || body.RegionID != "rke2-lab" ||
		body.PostgresVersion != 16 || !bounds.valid() || len(body.Password) < 12 || len(body.Password) > 256 {
		fail(w, r, 422, "invalid_project", "Check name, PostgreSQL 16, region, password and VM bounds")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	scope := "POST:/organizations/" + r.PathValue("org") + "/projects"
	hash, legacyHash := s.createRequestHash(scope, body), requestHash(scope, body)
	if s.replayCreate(w, r, key, hash, legacyHash, "project") {
		return
	}
	suffix := stableSuffix(userFrom(r).ID, scope, key)
	p := creationPayload("prj_"+suffix, "br_"+suffix, "ep_"+suffix,
		stableHex(suffix, "tenant"), stableHex(suffix, "timeline"), bounds)
	if err := s.kube.reserveCredentials(r.Context(), p.ProjectID, p.EndpointID, body.Password); err != nil {
		fail(w, r, 503, "credential_unavailable", "Could not reserve endpoint credentials")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	err = projectQuota(r.Context(), tx, r.PathValue("org"))
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,
		default_branch_id,created_at,updated_at) VALUES($1,$5,$2,$3,'rke2-lab',16,'provisioning','managed',$4,now(),now())`,
			p.ProjectID, body.Name, p.TenantID, p.BranchID, r.PathValue("org"))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO project_grants(project_id,user_id,role) VALUES($1,$2,'admin')`, p.ProjectID, userFrom(r).ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO branches(id,project_id,name,timeline_id,is_default,protected,state,created_at)
			VALUES($1,$2,$3,$4,true,true,'creating',now())`, p.BranchID, p.ProjectID, body.DefaultBranchName, p.TimelineID)
	}
	if err == nil {
		err = insertEndpoint(r.Context(), tx, p)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version)
			VALUES($1,'postgres','provisioning','provisioning','open-source-neon')`, p.BranchID)
	}
	opID := newID("op_")
	if err == nil {
		err = addCreateOperation(r.Context(), tx, "create_project", "project", p.ProjectID, p.ProjectID,
			opID, userFrom(r).ID, requestID(r), p, key, hash, createSteps("create_project", true))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.finishCreateError(w, r, tx, key, hash, legacyHash, "project", err)
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "create_project", "project", p.ProjectID, "accepted", requestID(r))
	s.acceptCreate(w, r, p.ProjectID, p.ProjectID, opID, "project")
}

func (s *server) createBranch(w http.ResponseWriter, r *http.Request) {
	if !creationEnabled() {
		fail(w, r, 503, "creation_disabled", "Creation driver is not enabled")
		return
	}
	project, ok := s.requireProject(w, r)
	if !ok {
		return
	}
	if project["source"] != "managed" || project["state"] != "ready" {
		fail(w, r, 422, "managed_project_required", "Ready managed project required")
		return
	}
	var body struct {
		Name                   string        `json:"name"`
		ParentBranchID         string        `json:"parent_branch_id"`
		CreateEndpoint         *bool         `json:"create_endpoint"`
		Password               string        `json:"password"`
		Autoscaling            *createBounds `json:"autoscaling"`
		ParentTimestamp        string        `json:"parent_timestamp,omitempty"`
		ParentLSN              string        `json:"parent_lsn,omitempty"`
		FunctionCopyPolicy     string        `json:"function_copy_policy,omitempty"`
		AcknowledgeFunctions   bool          `json:"acknowledge_function_environment_copy,omitempty"`
		AcknowledgeCurrentCode bool          `json:"acknowledge_current_functions_for_historical_data,omitempty"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, r, 422, "invalid_request", "Invalid branch request")
		return
	}
	withEndpoint := body.CreateEndpoint == nil || *body.CreateEndpoint
	historical := body.ParentTimestamp != "" || body.ParentLSN != ""
	if err := validateRestoreInput(body.ParentTimestamp, body.ParentLSN, time.Now()); err != nil {
		e := err.(restoreError)
		fail(w, r, e.status, e.code, "Specify one valid retained timestamp or LSN")
		return
	}
	if historical && !pitrEnabled() {
		fail(w, r, 503, "pitr_disabled", "Historical branch restore is not enabled")
		return
	}
	bounds := defaults(body.Autoscaling)
	if !validName(body.Name, 63) || !bounds.valid() || (withEndpoint && (len(body.Password) < 12 || len(body.Password) > 256)) ||
		(!withEndpoint && body.Password != "") {
		fail(w, r, 422, "invalid_branch", "Check branch name, password and VM bounds")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	scope := "POST:/projects/" + r.PathValue("project") + "/branches"
	hash, legacyHash := s.createRequestHash(scope, body), requestHash(scope, body)
	if s.replayCreate(w, r, key, hash, legacyHash, "branch") {
		return
	}
	copyPolicy := body.FunctionCopyPolicy
	if copyPolicy == "" {
		copyPolicy = "omit"
		if s.functionBranchCopyEnabled() {
			copyPolicy = "inherit_paused"
		}
	}
	if copyPolicy != "omit" && copyPolicy != "inherit_paused" {
		fail(w, r, 422, "invalid_function_copy_policy", "Choose inherit_paused or omit for branch Functions")
		return
	}
	if copyPolicy == "inherit_paused" && !s.functionBranchCopyEnabled() {
		fail(w, r, 503, "function_branch_copy_disabled", "Operator must enable verified branch Function preparation")
		return
	}
	parent, err := s.one(r.Context(), `SELECT id,timeline_id,state,expires_at FROM branches
		WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, body.ParentBranchID, r.PathValue("project"))
	if err != nil {
		fail(w, r, 404, "parent_not_found", "Parent branch not found")
		return
	}
	if parent["state"] != "ready" {
		fail(w, r, 423, "parent_not_ready", "Parent branch is not ready")
		return
	}
	if parent["expires_at"] != nil {
		fail(w, r, 409, "parent_expiring", "An expiring branch cannot acquire child branches")
		return
	}
	suffix := stableSuffix(userFrom(r).ID, scope, key)
	endpointID := ""
	if withEndpoint {
		endpointID = "ep_" + suffix
	}
	p := creationPayload(r.PathValue("project"), "br_"+suffix, endpointID,
		stringVal(project["tenant_id"]), stableHex(suffix, "timeline"), bounds)
	p.ParentTimelineID = stringVal(parent["timeline_id"])
	p.CatalogPolicy = catalogPreserveInheritedSQL
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var parentState string
	err = tx.QueryRow(r.Context(), "SELECT state FROM projects WHERE id=$1 AND source='managed' AND deleted_at IS NULL FOR UPDATE", p.ProjectID).Scan(&parentState)
	if err != nil || parentState != "ready" {
		fail(w, r, 409, "project_not_ready", "Ready managed parent project required")
		return
	}
	var expiring bool
	err = tx.QueryRow(r.Context(), "SELECT state,expires_at IS NOT NULL FROM branches WHERE id=$1 AND project_id=$2 FOR UPDATE", body.ParentBranchID, p.ProjectID).Scan(&parentState, &expiring)
	if err != nil || parentState != "ready" {
		fail(w, r, 409, "parent_not_ready", "Parent branch is not ready")
		return
	}
	if expiring {
		fail(w, r, 409, "parent_expiring", "An expiring branch cannot acquire child branches")
		return
	}
	if err = checkDefaultOwnerReservation(r.Context(), tx, p.ProjectID, body.ParentBranchID); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	var catalogPending bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE resource_type='branch_catalog'
		AND resource_id=$1 AND state IN ('queued','running','retry_wait'))`, body.ParentBranchID).Scan(&catalogPending)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect parent catalog")
		return
	}
	if catalogPending {
		fail(w, r, 409, "branch_operation_in_progress", "Parent catalog operation is active")
		return
	}
	if copyPolicy == "inherit_paused" {
		var functionsExist bool
		if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND state NOT IN ('deleting','deleted'))`, p.ProjectID, body.ParentBranchID).Scan(&functionsExist) != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not inspect original Function policy")
			return
		}
		if functionsExist && (!body.AcknowledgeFunctions || (historical && !body.AcknowledgeCurrentCode)) {
			fail(w, r, 422, "function_copy_acknowledgment_required", "Confirm the branch environment copy; historical SQL restore additionally requires acknowledging current accepted Function code rather than historical code")
			return
		}
	}
	if historical {
		p.ParentLSN, err = s.kube.resolveRestorePoint(r.Context(), p.TenantID, p.ParentTimelineID, body.ParentTimestamp, body.ParentLSN)
		p.RestoreSource = "lsn"
		if body.ParentTimestamp != "" {
			p.RestoreSource = "timestamp"
		}
	} else {
		p.ParentLSN, err = s.kube.parentLSN(r.Context(), p.TenantID, p.ParentTimelineID)
	}
	if err != nil {
		var restoreErr restoreError
		if errors.As(err, &restoreErr) {
			fail(w, r, restoreErr.status, restoreErr.code, "The requested restore point is not available")
			return
		}
		fail(w, r, 503, "parent_lsn_unavailable", "Could not read parent fork point")
		return
	}
	if withEndpoint {
		if err = s.kube.reserveCredentials(r.Context(), p.ProjectID, p.EndpointID, body.Password); err != nil {
			fail(w, r, 503, "credential_unavailable", "Could not reserve endpoint credentials")
			return
		}
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,parent_lsn,
		is_default,protected,state,created_at) VALUES($1,$2,$3,$4,$5,$6,false,false,'creating',now())`,
		p.BranchID, p.ProjectID, body.Name, p.TimelineID, body.ParentBranchID, p.ParentLSN)
	if err == nil && historical {
		_, err = tx.Exec(r.Context(), `UPDATE branches SET restore_source=$2,parent_timestamp=NULLIF($3,'')::timestamptz WHERE id=$1`, p.BranchID, p.RestoreSource, body.ParentTimestamp)
	}
	// Historical data must not receive today's catalog DDL or role credentials.
	// Its physical SQL inventory is observed, rather than copied from metadata.
	if err == nil && !historical {
		_, err = tx.Exec(r.Context(), `INSERT INTO branch_roles(branch_id,name,state,credential_ref)
		SELECT $1,name,'ready',credential_ref FROM branch_roles WHERE branch_id=$2 AND state='ready'`, p.BranchID, body.ParentBranchID)
	}
	if err == nil && !historical {
		_, err = tx.Exec(r.Context(), `INSERT INTO branch_databases(branch_id,name,owner_name,state)
		SELECT $1,name,owner_name,'ready' FROM branch_databases WHERE branch_id=$2 AND state='ready'`, p.BranchID, body.ParentBranchID)
	}
	if err == nil && !historical {
		// The live fork copies PostgreSQL OIDs and current catalog intent. A
		// historical restore never receives today's connection identity defaults.
		_, err = tx.Exec(r.Context(), `INSERT INTO branch_connection_defaults(branch_id,mode,role_name,database_name,role_oid,database_oid,tenant_id,timeline_id,version,updated_by)
		SELECT $1,mode,role_name,database_name,role_oid,database_oid,
		CASE WHEN mode='managed' THEN $3 END,CASE WHEN mode='managed' THEN $4 END,2,$5
		FROM branch_connection_defaults WHERE branch_id=$2`, p.BranchID, body.ParentBranchID, p.TenantID, p.TimelineID, userFrom(r).ID)
	}
	if err == nil && !historical {
		err = copyDefaultOwnerBindingTx(r.Context(), tx, p.ProjectID, body.ParentBranchID, p.BranchID, p.TenantID, p.TimelineID, userFrom(r).ID)
	}
	if err == nil && withEndpoint {
		err = insertEndpoint(r.Context(), tx, p)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version)
			VALUES($1,'postgres','provisioning','provisioning','open-source-neon')`, p.BranchID)
	}
	opID := newID("op_")
	if err == nil {
		err = addCreateOperation(r.Context(), tx, "create_branch", "branch", p.BranchID, p.ProjectID,
			opID, userFrom(r).ID, requestID(r), p, key, hash, branchCreationSteps(withEndpoint, historical))
	}
	if err == nil {
		err = s.captureCatalogInheritanceTx(r.Context(), tx, opID, p, body.ParentBranchID, historical)
	}
	if err == nil && copyPolicy == "inherit_paused" {
		err = s.snapshotFunctionBranchTx(r.Context(), tx, p.ProjectID, body.ParentBranchID, p.BranchID, opID, userFrom(r).ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.finishCreateError(w, r, tx, key, hash, legacyHash, "branch", err)
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "create_branch", "branch", p.BranchID, "accepted", requestID(r))
	s.acceptCreate(w, r, p.ProjectID, p.BranchID, opID, "branch")
}

func (s *server) createEndpoint(w http.ResponseWriter, r *http.Request) {
	if !creationEnabled() {
		fail(w, r, 503, "creation_disabled", "Creation driver is not enabled")
		return
	}
	project, ok := s.requireProject(w, r)
	if !ok {
		return
	}
	if project["source"] != "managed" || project["state"] != "ready" {
		fail(w, r, 422, "managed_project_required", "Ready managed project required")
		return
	}
	var body struct {
		BranchID    string        `json:"branch_id"`
		Password    string        `json:"password"`
		Type        string        `json:"type,omitempty"`
		Autoscaling *createBounds `json:"autoscaling"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, r, 422, "invalid_request", "Invalid endpoint request")
		return
	}
	bounds := defaults(body.Autoscaling)
	endpointType := body.Type
	if endpointType == "" {
		endpointType = "read_write"
	}
	if !bounds.valid() || (endpointType != "read_write" && endpointType != "read_only") ||
		(endpointType == "read_write" && (len(body.Password) < 12 || len(body.Password) > 256)) ||
		(endpointType == "read_only" && body.Password != "") {
		fail(w, r, 422, "invalid_endpoint", "Check type, password and VM bounds")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	scope := "POST:/projects/" + r.PathValue("project") + "/endpoints"
	hash, legacyHash := s.createRequestHash(scope, body), requestHash(scope, body)
	if s.replayCreate(w, r, key, hash, legacyHash, "endpoint") {
		return
	}
	branch, err := s.one(r.Context(), `SELECT id,timeline_id,state,expires_at FROM branches
		WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, body.BranchID, r.PathValue("project"))
	if err != nil {
		fail(w, r, 404, "branch_not_found", "Branch not found")
		return
	}
	if branch["state"] != "ready" {
		fail(w, r, 423, "branch_not_ready", "Branch is not ready")
		return
	}
	if branch["expires_at"] != nil {
		fail(w, r, 409, "branch_expiring", "Remove the future expiration before adding Compute")
		return
	}
	if err = checkDefaultOwnerReservation(r.Context(), s.db, r.PathValue("project"), body.BranchID); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	suffix := stableSuffix(userFrom(r).ID, scope, key)
	p := creationPayload(r.PathValue("project"), body.BranchID, "ep_"+suffix,
		stringVal(project["tenant_id"]), stringVal(branch["timeline_id"]), bounds)
	p.EndpointType = endpointType
	p.CatalogPolicy = catalogPreserveInheritedSQL
	var credentialErr error
	if endpointType == "read_only" {
		var writerID string
		credentialErr = s.db.QueryRow(r.Context(), `SELECT id FROM endpoints WHERE branch_id=$1 AND project_id=$2
			AND endpoint_type='read_write' AND deleted_at IS NULL AND state='active'`,
			p.BranchID, p.ProjectID).Scan(&writerID)
		if isNoRows(credentialErr) {
			fail(w, r, 422, "writer_required", "Create a ready read-write endpoint before a read replica")
			return
		}
		if credentialErr == nil {
			credentialErr = s.kube.reserveReplicaCredentials(r.Context(), p.ProjectID, p.EndpointID, writerID)
		}
	} else {
		credentialErr = s.reserveEndpointCredentials(r.Context(), p, body.Password)
	}
	if credentialErr != nil {
		if errors.Is(credentialErr, errBranchPasswordMismatch) {
			fail(w, r, 422, "branch_password_mismatch", "Use the existing branch SQL password; creating a replacement compute does not rotate database roles")
			return
		}
		fail(w, r, 503, "credential_unavailable", "Could not reserve endpoint credentials")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var ready string
	err = tx.QueryRow(r.Context(), "SELECT state FROM projects WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", p.ProjectID).Scan(&ready)
	if err == nil && ready != "ready" {
		fail(w, r, 409, "project_not_ready", "Project is not ready")
		return
	}
	if err == nil {
		var expiring bool
		err = tx.QueryRow(r.Context(), "SELECT state,expires_at IS NOT NULL FROM branches WHERE id=$1 AND project_id=$2 FOR UPDATE", p.BranchID, p.ProjectID).Scan(&ready, &expiring)
		if err == nil && expiring {
			fail(w, r, 409, "branch_expiring", "Remove the future expiration before adding Compute")
			return
		}
	}
	if err == nil {
		var pending bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE resource_type='branch_catalog'
			AND resource_id=$1 AND state IN ('queued','running','retry_wait'))`, p.BranchID).Scan(&pending)
		if pending {
			fail(w, r, 409, "branch_operation_in_progress", "A catalog operation is active")
			return
		}
	}
	if err == nil && ready != "ready" {
		fail(w, r, 409, "branch_not_ready", "Branch is not ready")
		return
	}
	if err == nil {
		err = checkDefaultOwnerReservation(r.Context(), tx, p.ProjectID, p.BranchID)
	}
	if err == nil {
		err = insertEndpoint(r.Context(), tx, p)
	}
	opID := newID("op_")
	if err == nil {
		err = addCreateOperation(r.Context(), tx, "create_endpoint", "endpoint", p.EndpointID, p.ProjectID,
			opID, userFrom(r).ID, requestID(r), p, key, hash, createSteps("create_endpoint", true))
	}
	if err == nil {
		err = s.captureCatalogInheritanceTx(r.Context(), tx, opID, p, p.BranchID, false)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.finishCreateError(w, r, tx, key, hash, legacyHash, "endpoint", err)
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "create_endpoint", "endpoint", p.EndpointID, "accepted", requestID(r))
	s.acceptCreate(w, r, p.ProjectID, p.EndpointID, opID, "endpoint")
}

func (s *server) retryOperation(w http.ResponseWriter, r *http.Request) {
	// Authorization already resolves the parent and limits tombstone access
	// to Admin. Recovery may fail while deleted_at is retained; requiring a
	// live project here would make that durable recovery Operation impossible
	// to retry. PostgreSQL's lifecycle guard rejects unrelated work on tombstones.
	if err := s.retryFailed(r.Context(), r.PathValue("operation"), r.PathValue("project")); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, r, 409, "operation_not_retryable", "Operation is not failed and retryable")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not retry operation")
		}
		return
	}
	op, err := s.operationRecord(r.Context(), r.PathValue("operation"), r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load operation")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "retry_operation", "operation", r.PathValue("operation"), "accepted", requestID(r))
	jsonResponse(w, 202, op)
}
