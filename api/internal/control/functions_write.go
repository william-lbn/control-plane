package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/william-lbn/control-plane/api/internal/functions"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

func (s *server) functionsConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.functions == nil || s.storage == nil {
		fail(w, r, 503, "functions_driver_disabled", "Functions preview Driver is not configured")
		return false
	}
	return true
}

func functionAdmissionError(err error) (int, string, string) {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23514" && pgError.Message == "function definition budget exhausted" {
		return 429, "function_quota_exceeded", "Function definition preview budget exhausted"
	}
	return conflictCode(err)
}

func (s *server) functionRequestID(prefix, actor, key, fingerprint string) string {
	mac := hmac.New(sha256.New, s.functions.key)
	_, _ = mac.Write([]byte("neon.functions.intent-id.v1\x00" + prefix + "\x00" + actor + "\x00" + key + "\x00" + fingerprint))
	return prefix + hex.EncodeToString(mac.Sum(nil))[:16]
}

func (s *server) functionFingerprint(input []byte) string {
	mac := hmac.New(sha256.New, s.functions.key)
	_, _ = mac.Write([]byte("neon.functions.metadata-request.v1\x00"))
	_, _ = mac.Write(input)
	return "fn1:" + hex.EncodeToString(mac.Sum(nil))
}

func (s *server) acceptFunctionOperation(w http.ResponseWriter, r *http.Request, id string) {
	op, err := s.operationRecord(r.Context(), id, r.PathValue("project"))
	if err != nil || op["resource_type"] != "function" {
		fail(w, r, 503, "metadata_unavailable", "Could not load Function Operation")
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+r.PathValue("project")+"/operations/"+id)
	jsonResponse(w, 202, record{"function_id": op["resource_id"], "operation": op})
}

func (s *server) replayFunctionOperation(w http.ResponseWriter, r *http.Request, key, fingerprint string) bool {
	var prior, id string
	err := s.db.QueryRow(r.Context(), `SELECT request_hash,operation_id FROM idempotency_keys WHERE actor_id=$1 AND key_hash=$2`, userFrom(r).ID, keyHash(key)).Scan(&prior, &id)
	if isNoRows(err) {
		return false
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect Function request")
		return true
	}
	if prior != fingerprint {
		fail(w, r, 409, "idempotency_conflict", "Key was used for different input")
		return true
	}
	s.acceptFunctionOperation(w, r, id)
	return true
}

// Definition admission performs metadata only. It creates no customer process,
// role or VM; deployment must subsequently admit an immutable code/env intent.
func (s *server) functionCreate(w http.ResponseWriter, r *http.Request) {
	if !s.functionsConfigured(w, r) {
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	var input struct {
		Slug     string `json:"slug"`
		Database string `json:"database_name"`
		Schema   string `json:"sql_schema"`
	}
	if readJSON(r, &input) != nil || !functions.ValidSlug(input.Slug) {
		fail(w, r, 422, "invalid_function", "Valid immutable slug/database/schema required")
		return
	}
	scope := functionsql.Scope{FunctionID: "fnc_0000000000000000", ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch"), Database: input.Database, Schema: input.Schema, ProvisioningRole: "control_probe"}
	if scope.Validate() != nil {
		fail(w, r, 422, "invalid_function", "Native project/branch and application database/schema required")
		return
	}
	canonical, _ := json.Marshal(record{"action": "create_function", "project_id": scope.ProjectID, "branch_id": scope.BranchID, "input": input})
	fingerprint := s.functionFingerprint(canonical)
	if s.replayFunctionOperation(w, r, key, fingerprint) {
		return
	}
	scope.FunctionID = s.functionRequestID("fnc_", userFrom(r).ID, key, fingerprint)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not admit Function")
		return
	}
	defer tx.Rollback(r.Context())
	var org, writer string
	err = tx.QueryRow(r.Context(), `SELECT p.org_id,e.id FROM projects p JOIN organizations o ON o.id=p.org_id
 JOIN branches b ON b.project_id=p.id JOIN endpoints e ON e.branch_id=b.id AND e.project_id=p.id
 WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL
 AND o.state='active' AND b.state='ready' AND b.deleted_at IS NULL AND e.endpoint_type='read_write'
 AND e.workload_kind='neonvm' AND e.state IN ('active','suspended') AND e.deleted_at IS NULL FOR UPDATE OF p,b,e`, scope.ProjectID, scope.BranchID).Scan(&org, &writer)
	if err != nil {
		fail(w, r, 409, "function_writer_required", "Ready native branch and original Writer required")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'provisioning')`, scope.FunctionID, org, scope.ProjectID, scope.BranchID, writer, input.Slug, input.Database, input.Schema, scope.Role())
	op := s.functionRequestID("op_", userFrom(r).ID, key, fingerprint)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,started_at,finished_at)
 VALUES($1,$2,'function',$3,'create_function','succeeded',$4,$5,'{}',now(),now())`, op, scope.ProjectID, scope.FunctionID, userFrom(r).ID, requestID(r))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), fingerprint, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'create_function','function',$2,$3,'accepted')`, userFrom(r).ID, scope.FunctionID, requestID(r))
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

// Source/env bytes are verified and written outside the metadata transaction.
// The transaction later rechecks authoritative parents/version and binds the
// exact actual env UID/MAC in the same commit as its queued Operation. Failed
// admission retains orphan backing for reviewed GC; no request retries DELETE.
func (s *server) functionDeploy(w http.ResponseWriter, r *http.Request) {
	if !s.functionsConfigured(w, r) {
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	if err != nil || version < 1 {
		fail(w, r, 428, "version_required", "Function numeric If-Match version required")
		return
	}
	input, err := functions.ParseDeploymentRequest(r)
	if err != nil {
		fail(w, r, 422, "invalid_deployment", err.Error())
		return
	}
	fingerprint, err := input.RequestFingerprint(s.functions.key, functions.DeploymentRequestScope{ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch"), Slug: r.PathValue("slug"), ExpectedVersion: version})
	if err != nil {
		fail(w, r, 422, "invalid_deployment", "Deployment scope or environment rejected")
		return
	}
	fingerprint = "fn1:" + fingerprint
	if s.replayFunctionOperation(w, r, key, fingerprint) {
		return
	}
	var functionID, state string
	var actualVersion, generation int64
	err = s.db.QueryRow(r.Context(), `SELECT id,state,version,generation FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted'`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug")).Scan(&functionID, &state, &actualVersion, &generation)
	if err != nil {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	if actualVersion != version {
		fail(w, r, 412, "version_conflict", "Function changed; refresh before retrying")
		return
	}
	if state == "deleting" {
		fail(w, r, 409, "function_deleting", "Function is being deleted")
		return
	}
	if state == "active" {
		fail(w, r, 409, "stop_before_redeploy", "Explicitly stop the original Function before replacing code or environment")
		return
	}
	// Hydrate previous immutable source/env for config-only deployments. No
	// customer code is executed in API and secret values never enter a response.
	var previous *functions.DeploymentSnapshot
	var summary functions.DeploymentSummary
	var envRef functionEnvironmentReference
	var priorKeyVersion string
	err = s.db.QueryRow(r.Context(), `SELECT d.runtime,d.bundle_digest,d.bundle_bytes,d.entry,b.environment_namespace,b.environment_secret_ref,b.environment_secret_uid,b.environment_mac,b.mac_key_version,d.id
 FROM function_definitions f JOIN function_deployments d ON d.id=f.active_deployment_id
 JOIN function_deployment_backings b ON b.deployment_id=d.id WHERE f.id=$1 AND d.state='completed'`, functionID).
		Scan(&summary.Runtime, &summary.BundleDigest, &summary.BundleBytes, &summary.Entry, &envRef.Namespace, &envRef.Name, &envRef.UID, &envRef.MAC, &priorKeyVersion, &envRef.DeploymentID)
	if err == nil {
		envRef.ProjectID, envRef.BranchID = r.PathValue("project"), r.PathValue("branch")
		if priorKeyVersion != s.functions.input.KeyVersion {
			fail(w, r, 409, "function_key_version_unavailable", "Original backing verification key is required")
			return
		}
		environment, e := s.kube.readFunctionEnvironment(r.Context(), s.functions.key, envRef)
		if e != nil {
			fail(w, r, 503, "function_backing_unavailable", "Original environment unavailable")
			return
		}
		snapshot, e := functions.RestoreDeploymentSnapshot(summary, environment)
		if e != nil {
			fail(w, r, 503, "function_backing_unavailable", "Original deployment unavailable")
			return
		}
		previous = &snapshot
	} else if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not load deployment history")
		return
	}
	snapshot, err := functions.ResolveDeployment(previous, input)
	if err != nil {
		fail(w, r, 422, "invalid_deployment", err.Error())
		return
	}
	summary = snapshot.Summary()
	var artifact functionArtifactReference
	if archive := snapshot.ArtifactBytes(); len(archive) > 0 {
		artifact, err = s.storage.reserveFunctionArtifact(r.Context(), r.PathValue("project"), r.PathValue("branch"), archive)
	} else {
		artifact.Key, err = functionArtifactKey(r.PathValue("project"), r.PathValue("branch"), summary.BundleDigest)
		artifact.ProjectID, artifact.BranchID, artifact.Digest, artifact.Size, artifact.Entry = r.PathValue("project"), r.PathValue("branch"), summary.BundleDigest, int64(summary.BundleBytes), summary.Entry
		if err == nil {
			_, err = s.storage.readFunctionArtifact(r.Context(), artifact)
		}
	}
	if err != nil {
		fail(w, r, 503, "function_backing_unavailable", "Immutable code artifact unavailable")
		return
	}
	deployment := s.functionRequestID("fdp_", userFrom(r).ID, key, fingerprint)
	environment, err := s.kube.reserveFunctionEnvironment(r.Context(), s.functions.key, r.PathValue("project"), r.PathValue("branch"), deployment, snapshot.SecretEnvironment())
	if err != nil {
		fail(w, r, 503, "function_backing_unavailable", "Immutable deployment environment unavailable")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not admit deployment")
		return
	}
	defer tx.Rollback(r.Context())
	var eligible bool
	err = tx.QueryRow(r.Context(), `SELECT f.version=$4 AND f.state NOT IN ('active','deleting','deleted') AND p.state='ready' AND p.source='managed'
 AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active'
 AND e.endpoint_type='read_write' AND e.state IN ('active','suspended') AND e.deleted_at IS NULL,f.generation
 FROM function_definitions f JOIN projects p ON p.id=f.project_id JOIN branches b ON b.id=f.branch_id
 JOIN organizations o ON o.id=f.org_id JOIN endpoints e ON e.id=f.endpoint_id
 WHERE f.id=$1 AND f.project_id=$2 AND f.branch_id=$3 FOR UPDATE OF p,b,e,f`, functionID, r.PathValue("project"), r.PathValue("branch"), version).Scan(&eligible, &generation)
	if err != nil || !eligible {
		fail(w, r, 412, "function_intent_changed", "Function or parents changed before admission")
		return
	}
	var pending bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$3))`, r.PathValue("project"), functionID, r.PathValue("branch")).Scan(&pending)
	if err != nil || pending {
		fail(w, r, 409, "function_operation_in_progress", "Another branch or Function Operation is active")
		return
	}
	p := functionInstanceTarget{FunctionID: functionID, ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch"), DeploymentID: deployment, Generation: generation + 1, Slug: r.PathValue("slug")}
	op := s.functionRequestID("op_", userFrom(r).ID, key, fingerprint)
	payload, _ := json.Marshal(p)
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
 VALUES($1,$2,'function',$3,'deploy_function','queued',$4,$5,$6)`, op, p.ProjectID, p.FunctionID, userFrom(r).ID, requestID(r), payload)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,environment_names,actor_id,operation_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, deployment, functionID, p.ProjectID, p.BranchID, summary.Runtime, summary.BundleDigest, summary.BundleBytes, summary.Entry, artifact.Key, environment.Name, summary.EnvironmentNames, userFrom(r).ID, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO function_deployment_backings(deployment_id,function_id,project_id,branch_id,environment_namespace,environment_secret_ref,environment_secret_uid,environment_mac,mac_key_version)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, deployment, functionID, p.ProjectID, p.BranchID, environment.Namespace, environment.Name, environment.UID, environment.MAC, s.functions.input.KeyVersion)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE function_definitions SET target_deployment_id=$2,generation=$3,version=version+1,state='provisioning',updated_at=now() WHERE id=$1`, functionID, deployment, p.Generation)
	}
	for ordinal, name := range []string{"verify_immutable_backing", "reconcile_restricted_sql_identity", "reserve_candidate", "publish_immutable_bootstrap", "reconcile_microvm", "verify_guest_and_publish"} {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, op, ordinal, name)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), fingerprint, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'deploy_function','function',$2,$3,'accepted')`, userFrom(r).ID, functionID, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayFunctionOperation(w, r, key, fingerprint) {
			return
		}
		status, code, message := conflictCode(err)
		fail(w, r, status, code, message)
		return
	}
	s.acceptFunctionOperation(w, r, op)
}
