package control

// Prefer the last successful deployment. A first failed deployment has no
// successful source: reuse its immutable source only after the acknowledged
// stop Operation completed for the current generation and every candidate was
// retired. Neither a failed intent nor an unavailable guest permits replacement.
// The API still verifies the backing MAC/UID and blob digest before admission.
const functionPreviousSourceQuery = `SELECT d.runtime,d.bundle_digest,d.bundle_bytes,d.entry,
 b.environment_namespace,b.environment_secret_ref,b.environment_secret_uid,b.environment_mac,b.mac_key_version,d.id
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
