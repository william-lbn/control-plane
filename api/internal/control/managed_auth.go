package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type ManagedAuthSpec struct {
	Database       string   `json:"database"`
	AllowedOrigins []string `json:"allowed_origins"`
}
type managedAuthPayload struct {
	ProjectID  string          `json:"project_id"`
	BranchID   string          `json:"branch_id"`
	EndpointID string          `json:"endpoint_id"`
	Generation int64           `json:"generation"`
	SecretRef  string          `json:"secret_ref"`
	Spec       ManagedAuthSpec `json:"spec"`
}

func managedAuthName(branch string) string { return "neon-auth-" + strings.TrimPrefix(branch, "br_") }
func managedAuthLogin(branch string) string {
	return "control_auth_" + strings.TrimPrefix(branch, "br_")
}
func managedAuthBase(branch string) string {
	return os.Getenv("NEON_AUTH_PUBLIC_ORIGIN") + "/auth/v1/" + branch
}
func managedAuthEnabled() bool {
	return os.Getenv("NEON_AUTH_ENABLED") == "true" && os.Getenv("NEON_AUTH_LAB_HTTP") == "true" && digestImage.MatchString(os.Getenv("NEON_AUTH_RUNTIME_IMAGE")) && validAuthOrigin(os.Getenv("NEON_AUTH_PUBLIC_ORIGIN"))
}
func validAuthOrigin(origin string) bool {
	u, e := url.Parse(origin)
	return e == nil && u.Host != "" && !strings.Contains(u.Host, "*") && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && (u.Scheme == "https" || (os.Getenv("NEON_AUTH_LAB_HTTP") == "true" && u.Scheme == "http"))
}
func validateManagedAuth(p managedAuthPayload) error {
	if !dataBranchID.MatchString(p.BranchID) || !validPGIdentifier(p.Spec.Database) || p.Spec.AllowedOrigins == nil || len(p.Spec.AllowedOrigins) > 8 {
		return errors.New("invalid Managed Auth configuration; database and allowed_origins array are required")
	}
	for _, v := range p.Spec.AllowedOrigins {
		if !validAuthOrigin(v) {
			return errors.New("trusted origins must be exact HTTPS origins, or explicit lab HTTP origins")
		}
	}
	return nil
}
func managedAuthLabels(p managedAuthPayload) map[string]string {
	l := credentialLabels(p.ProjectID, p.EndpointID)
	l["neon-control/branch-id"] = p.BranchID
	l["neon-control/managed-auth"] = p.BranchID
	return l
}
func ownedManagedAuth(v map[string]any, p managedAuthPayload) bool {
	return owned(v, p.ProjectID, p.EndpointID) && nested(v, "metadata", "labels", "neon-control/branch-id") == p.BranchID
}
func (s *server) managedAuthRead(w http.ResponseWriter, r *http.Request) {
	branch, err := s.one(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, r.PathValue("branch"), r.PathValue("project"))
	if err != nil {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	item, err := s.one(r.Context(), `SELECT branch_id,endpoint_id,generation,state,spec,updated_at FROM managed_auth_instances WHERE branch_id=$1 AND project_id=$2`, branch["id"], r.PathValue("project"))
	if isNoRows(err) {
		item = record{"branch_id": branch["id"], "generation": int64(0), "state": "disabled", "spec": nil}
	} else if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read Managed Auth")
		return
	}
	item["driver_enabled"] = managedAuthEnabled()
	item["issuer"] = managedAuthBase(stringVal(branch["id"]))
	item["audience"] = branch["id"]
	item["public_endpoint"] = managedAuthBase(stringVal(branch["id"]))
	// Kubernetes readiness is observed without SQL; visiting this page must not
	// cold-wake a Compute. Active metadata alone is not a runtime health claim.
	if item["state"] == "active" {
		item["runtime"] = s.kube.runtime(r.Context(), "deployment", managedAuthName(stringVal(branch["id"])))
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, int64(number(item["generation"]))))
	jsonResponse(w, 200, item)
}
func (s *server) acceptManagedAuth(w http.ResponseWriter, r *http.Request, id string) {
	op, err := s.operationRecord(r.Context(), id, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load Operation")
		return
	}
	if op["resource_id"] != r.PathValue("branch") || op["resource_type"] != "managed_auth" {
		fail(w, r, 409, "idempotency_conflict", "Key belongs to another resource")
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+r.PathValue("project")+"/operations/"+id)
	jsonResponse(w, 202, map[string]any{"branch_id": r.PathValue("branch"), "operation": op})
}
func (s *server) replayManagedAuth(w http.ResponseWriter, r *http.Request, key, hash string) bool {
	var stored, id string
	err := s.db.QueryRow(r.Context(), `SELECT request_hash,operation_id FROM idempotency_keys WHERE actor_id=$1 AND key_hash=$2`, userFrom(r).ID, keyHash(key)).Scan(&stored, &id)
	if isNoRows(err) {
		return false
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check idempotency")
		return true
	}
	if stored != hash {
		fail(w, r, 409, "idempotency_conflict", "Key used with different input")
		return true
	}
	s.acceptManagedAuth(w, r, id)
	return true
}
func (s *server) managedAuthMutate(w http.ResponseWriter, r *http.Request) {
	if !managedAuthEnabled() || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "managed_auth_driver_disabled", "Managed Auth native Driver is not configured")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	if err != nil || version < 0 {
		fail(w, r, 428, "version_required", "If-Match with the numeric generation is required")
		return
	}
	p := managedAuthPayload{ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch")}
	action := "enable_managed_auth"
	if r.Method == http.MethodDelete {
		action = "disable_managed_auth"
	} else {
		if err = readJSON(r, &p.Spec); err != nil {
			fail(w, r, 422, "invalid_json", "Invalid Managed Auth configuration JSON")
			return
		}
		if err = validateManagedAuth(p); err != nil {
			fail(w, r, 422, "invalid_managed_auth_config", err.Error())
			return
		}
	}
	hash := s.createRequestHash(action+":"+p.ProjectID+":"+p.BranchID, map[string]any{"spec": p.Spec, "generation": version})
	if s.replayManagedAuth(w, r, key, hash) {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not admit Operation")
		return
	}
	defer tx.Rollback(r.Context())
	var branchState string
	err = tx.QueryRow(r.Context(), `SELECT state FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, p.BranchID, p.ProjectID).Scan(&branchState)
	if err != nil || branchState != "ready" {
		fail(w, r, 409, "branch_not_ready", "Ready branch required")
		return
	}
	var previous int64
	var state string
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT generation,state,spec,endpoint_id,secret_ref FROM managed_auth_instances WHERE branch_id=$1`, p.BranchID).Scan(&previous, &state, &raw, &p.EndpointID, &p.SecretRef)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read service intent")
		return
	}
	if previous != version {
		fail(w, r, 412, "version_conflict", "Managed Auth generation changed; refresh before retrying")
		return
	}
	if action == "enable_managed_auth" && previous > 0 {
		var original ManagedAuthSpec
		if json.Unmarshal(raw, &original) != nil || original.Database != p.Spec.Database {
			fail(w, r, 409, "auth_database_immutable", "Auth database changes require a separate identity migration; re-enable the original database")
			return
		}
	}
	if action == "disable_managed_auth" {
		var dependent bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM data_api_instances WHERE branch_id=$1 AND state<>'disabled' AND spec->>'issuer'=$2 AND spec->>'audience'=$1)`, p.BranchID, managedAuthBase(p.BranchID)).Scan(&dependent); err != nil || dependent {
			fail(w, r, 409, "auth_data_api_dependency", "Disable this Auth-backed Data API before changing the identity service")
			return
		}
	}
	var pending bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, p.ProjectID, p.BranchID).Scan(&pending)
	if err != nil || pending {
		fail(w, r, 409, "branch_operation_in_progress", "Another branch Operation is active")
		return
	}
	if action == "enable_managed_auth" {
		var count int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM managed_auth_instances WHERE state<>'disabled'`).Scan(&count); err != nil || count >= 8 {
			fail(w, r, 409, "auth_capacity_exhausted", "Managed Auth instance budget exhausted; disable unused instances first")
			return
		}
	}
	p.Generation = previous + 1
	if action == "enable_managed_auth" {
		if state != "" && state != "disabled" {
			fail(w, r, 409, "disable_before_reconfigure", "Disable the existing instance before changing configuration")
			return
		}
		err = tx.QueryRow(r.Context(), `SELECT id FROM endpoints WHERE branch_id=$1 AND project_id=$2 AND endpoint_type='read_write' AND state='active' AND deleted_at IS NULL`, p.BranchID, p.ProjectID).Scan(&p.EndpointID)
		if err != nil {
			fail(w, r, 409, "writer_required", "A ready branch writer is required")
			return
		}
		p.SecretRef = fmt.Sprintf("%s-g%d", managedAuthName(p.BranchID), p.Generation)
		state = "provisioning"
	} else {
		if previous == 0 || state == "disabled" {
			fail(w, r, 409, "managed_auth_not_enabled", "Managed Auth is already disabled")
			return
		}
		if err = json.Unmarshal(raw, &p.Spec); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Service intent is invalid")
			return
		}
		state = "disabling"
	}
	spec, _ := json.Marshal(p.Spec)
	_, err = tx.Exec(r.Context(), `INSERT INTO managed_auth_instances(branch_id,project_id,endpoint_id,generation,state,spec,secret_ref) VALUES($1,$2,$3,$4,$5,$6,$7)
        ON CONFLICT(branch_id) DO UPDATE SET generation=$4,state=$5,spec=$6,secret_ref=$7,endpoint_id=$3,updated_at=now()`, p.BranchID, p.ProjectID, p.EndpointID, p.Generation, state, spec, p.SecretRef)
	id := newID("op_")
	payload, _ := json.Marshal(p)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'managed_auth',$3,$4,'queued',$5,$6,$7)`, id, p.ProjectID, p.BranchID, action, userFrom(r).ID, requestID(r), payload)
	}
	for i, step := range []string{"reserve_service_credentials", "apply_restricted_database_identity", "publish_service_proxy_identity", "reconcile_service_workload", "verify_and_commit_service"} {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, id, i, step)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayManagedAuth(w, r, key, hash) {
			return
		}
		fail(w, r, 409, "operation_conflict", "Could not admit service Operation")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, action, "managed_auth", p.BranchID, "accepted", requestID(r))
	s.acceptManagedAuth(w, r, id)
}
