package control

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func (s *server) functionBranchCopyEnabled() bool {
	return s.functions != nil && s.storage != nil && os.Getenv("NEON_FUNCTION_BRANCH_COPY_ENABLED") == "true"
}

type functionCopyInput struct {
	SourceBranch     string `json:"source_branch_id"`
	SourceSlug       string `json:"source_function_slug"`
	SourceDeployment string `json:"source_deployment_id"`
	Slug             string `json:"slug"`
	Acknowledge      bool   `json:"acknowledge_environment_copy"`
}

// Admission freezes only immutable metadata. The leased Worker must verify the
// actual original blob/Secret and create a new destination Secret/SQL identity.
// No source secrets or SQL connection strings enter this request or receipt.
func (s *server) copyFunctionBranch(w http.ResponseWriter, r *http.Request) {
	if !s.functionBranchCopyEnabled() {
		fail(w, r, 503, "function_branch_copy_disabled", "Operator must enable verified cross-branch preparation")
		return
	}
	var in functionCopyInput
	if readJSON(r, &in) != nil || !functionArtifactBranch.MatchString(in.SourceBranch) || !functions.ValidSlug(in.SourceSlug) || !functionDeploymentID.MatchString(in.SourceDeployment) || !functions.ValidSlug(in.Slug) || !in.Acknowledge || in.SourceBranch == r.PathValue("branch") {
		fail(w, r, 422, "invalid_function_copy", "Select a different branch, an exact completed deployment, a destination slug and explicit environment-copy consent")
		return
	}
	expected := r.Header.Get("If-Match")
	if expected == "" {
		fail(w, r, 428, "version_required", "Exact quoted source Function version required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	canonical, _ := json.Marshal(record{"action": "copy_function_branch", "project": r.PathValue("project"), "branch": r.PathValue("branch"), "source_version": expected, "input": in})
	fingerprint := s.functionFingerprint(canonical)
	if s.replayFunctionOperation(w, r, key, fingerprint) {
		return
	}
	functionID := s.functionRequestID("fnc_", userFrom(r).ID, key, fingerprint)
	deploymentID := s.functionRequestID("fdp_", userFrom(r).ID, key, fingerprint)
	op := s.functionRequestID("op_", userFrom(r).ID, key, fingerprint)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Copy admission unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var org string
	err = tx.QueryRow(r.Context(), `SELECT p.org_id FROM projects p JOIN organizations o ON o.id=p.org_id WHERE p.id=$1 AND p.source='managed' AND p.state='ready' AND p.deleted_at IS NULL AND o.state='active' FOR UPDATE OF p`, r.PathValue("project")).Scan(&org)
	if err != nil {
		fail(w, r, 409, "copy_project_unavailable", "Ready managed project required")
		return
	}
	rows, err := tx.Query(r.Context(), `SELECT id FROM branches WHERE project_id=$1 AND id IN ($2,$3) AND state='ready' AND deleted_at IS NULL ORDER BY id FOR UPDATE`, r.PathValue("project"), r.PathValue("branch"), in.SourceBranch)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Branch locks unavailable")
		return
	}
	count := 0
	for rows.Next() {
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != 2 {
		fail(w, r, 409, "copy_branch_unavailable", "Both original branches must be ready in the same project")
		return
	}
	var writer string
	err = tx.QueryRow(r.Context(), `SELECT id FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND endpoint_type='read_write' AND workload_kind='neonvm' AND state IN ('active','suspended') AND deleted_at IS NULL FOR UPDATE`, r.PathValue("project"), r.PathValue("branch")).Scan(&writer)
	if err != nil {
		fail(w, r, 409, "function_writer_required", "Original native destination Writer required")
		return
	}
	var sourceID, database, schema, sourceState string
	var version int64
	err = tx.QueryRow(r.Context(), `SELECT id,database_name,sql_schema,version,state FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted' FOR UPDATE`, r.PathValue("project"), in.SourceBranch, in.SourceSlug).Scan(&sourceID, &database, &schema, &version, &sourceState)
	if err != nil {
		fail(w, r, 404, "source_function_not_found", "Source Function not found")
		return
	}
	if expected != "\""+strconv.FormatInt(version, 10)+"\"" {
		fail(w, r, 412, "source_version_conflict", "Reload the original source Function version")
		return
	}
	if sourceState != "active" && sourceState != "suspended" {
		fail(w, r, 409, "source_function_not_ready", "An active or normally suspended source definition is required")
		return
	}
	var runtime, digest, entry string
	var size int64
	var names []string
	err = tx.QueryRow(r.Context(), `SELECT d.runtime,d.bundle_digest,d.bundle_bytes,d.entry,d.environment_names FROM function_deployments d JOIN function_deployment_backings b ON b.deployment_id=d.id AND b.function_id=d.function_id AND b.project_id=d.project_id AND b.branch_id=d.branch_id WHERE d.id=$1 AND d.function_id=$2 AND d.project_id=$3 AND d.branch_id=$4 AND d.state='completed'`, in.SourceDeployment, sourceID, r.PathValue("project"), in.SourceBranch).Scan(&runtime, &digest, &size, &entry, &names)
	if err != nil {
		fail(w, r, 404, "source_deployment_not_found", "Exact completed source deployment required")
		return
	}
	target := functionInstanceTarget{functionID, r.PathValue("project"), r.PathValue("branch"), deploymentID, 1, in.Slug}
	origin := functionCopySource{Actor: userFrom(r).ID, Function: sourceID, Branch: in.SourceBranch, Deployment: in.SourceDeployment, Summary: functions.DeploymentSummary{Runtime: runtime, BundleDigest: digest, BundleBytes: int(size), Entry: entry, EnvironmentNames: names}}
	err = insertFunctionCopyTx(r.Context(), tx, functionCopyAdmission{Target: target, Source: origin, SourceVersion: version, Org: org, Writer: writer, Database: database, Schema: schema, Operation: op, Request: requestID(r)})
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), fingerprint, op)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayFunctionOperation(w, r, key, fingerprint) {
			return
		}
		status, code, message := functionAdmissionError(err)
		fail(w, r, status, code, message)
		return
	}
	s.acceptFunctionOperation(w, r, op)
}
