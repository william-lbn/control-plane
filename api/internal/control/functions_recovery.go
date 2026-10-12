package control

import (
	"context"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// Prefer the last successful deployment. A first failed deployment has no
// successful source: reuse its immutable source only after the acknowledged
// stop Operation completed for the current generation and every candidate was
// retired. Neither a failed intent nor an unavailable guest permits replacement.
// The API still verifies the backing MAC/UID and blob digest before admission.
const functionPreviousSourceQuery = `SELECT d.runtime,d.bundle_digest,d.bundle_bytes,d.entry,d.environment_names,
 b.environment_namespace,b.environment_secret_ref,b.environment_secret_uid,b.environment_mac,b.mac_key_version,d.id,f.project_id,f.branch_id
 FROM function_definitions f
 JOIN function_deployments d ON d.id=COALESCE(f.active_deployment_id,f.target_deployment_id)
 AND d.function_id=f.id AND d.project_id=f.project_id AND d.branch_id=f.branch_id
 JOIN function_deployment_backings b ON b.deployment_id=d.id AND b.function_id=f.id
 AND b.project_id=f.project_id AND b.branch_id=f.branch_id
 WHERE f.id=$1 AND (
 (d.id=f.active_deployment_id AND d.state='completed') OR
 (f.active_deployment_id IS NULL AND f.state='suspended' AND d.id=f.target_deployment_id
 AND d.state IN ('pending','building','failed')
 AND EXISTS(SELECT 1 FROM operations o WHERE o.id=d.operation_id AND o.project_id=f.project_id
 AND o.resource_type='function' AND o.resource_id=f.id AND o.action='deploy_function' AND o.state='failed')
 AND NOT EXISTS(SELECT 1 FROM function_instances i WHERE i.function_id=f.id AND i.state<>'retired')
 AND EXISTS(SELECT 1 FROM operations o WHERE o.project_id=f.project_id AND o.resource_type='function'
 AND o.resource_id=f.id AND o.action='suspend_function' AND o.state='succeeded'
 AND o.payload->>'generation'=f.generation::text AND o.payload->>'branch_id'=f.branch_id)))`

// Load the complete retained summary, including its names-only environment
// inventory. Restoration compares this inventory against the MAC-verified
// Secret; silently omitting it must never weaken that integrity check.
func (s *server) loadPreviousFunctionSource(ctx context.Context, functionID string) (functions.DeploymentSummary, functionEnvironmentReference, string, error) {
	return s.loadRetainedFunctionSource(ctx, functionID, "")
}

// An explicit historical source must belong to the exact definition and be
// completed. Secret UID/MAC and artifact bytes are verified by admission.
const functionSelectedSourceQuery = `SELECT d.runtime,d.bundle_digest,d.bundle_bytes,d.entry,d.environment_names,
 b.environment_namespace,b.environment_secret_ref,b.environment_secret_uid,b.environment_mac,b.mac_key_version,d.id,f.project_id,f.branch_id
 FROM function_definitions f JOIN function_deployments d ON d.function_id=f.id
 AND d.project_id=f.project_id AND d.branch_id=f.branch_id
 JOIN function_deployment_backings b ON b.deployment_id=d.id AND b.function_id=f.id
 AND b.project_id=f.project_id AND b.branch_id=f.branch_id
 WHERE f.id=$1 AND d.id=$2 AND d.state='completed' AND f.state<>'deleted'`

func (s *server) loadRetainedFunctionSource(ctx context.Context, functionID, source string) (functions.DeploymentSummary, functionEnvironmentReference, string, error) {
	var summary functions.DeploymentSummary
	var reference functionEnvironmentReference
	var keyVersion string
	query, args := functionPreviousSourceQuery, []any{functionID}
	if source != "" {
		query, args = functionSelectedSourceQuery, []any{functionID, source}
	}
	err := s.db.QueryRow(ctx, query, args...).Scan(
		&summary.Runtime, &summary.BundleDigest, &summary.BundleBytes, &summary.Entry, &summary.EnvironmentNames,
		&reference.Namespace, &reference.Name, &reference.UID, &reference.MAC, &keyVersion, &reference.DeploymentID,
		&reference.ProjectID, &reference.BranchID)
	return summary, reference, keyVersion, err
}
