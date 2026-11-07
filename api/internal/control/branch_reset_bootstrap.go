package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

const branchResetBootstrapWitness = "branch_reset_sql_probe_v1"

// A current restore epoch supplements the original immutable bootstrap. A
// prepared but unpublished epoch cannot supersede it, and another timeline's
// publication cannot be used as the current branch's backing witness.
func readBranchResetBootstrap(ctx context.Context, q connectionDefaultReader, p createPayload) (computeCatalogBootstrap, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT r.proof FROM compute_catalog_reset_receipts r
 JOIN compute_catalog_reset_publications pub USING(operation_id,endpoint_id)
 JOIN branch_reset_checkpoints c ON c.operation_id=r.operation_id
 JOIN branches b ON b.id=c.branch_id AND b.project_id=c.project_id
   AND b.timeline_id=c.target_timeline_id AND b.deleted_at IS NULL
 JOIN endpoints e ON e.id=r.endpoint_id AND e.project_id=b.project_id AND e.branch_id=b.id AND e.deleted_at IS NULL
 JOIN projects pr ON pr.id=b.project_id AND pr.tenant_id=c.tenant_id AND pr.deleted_at IS NULL
 WHERE r.endpoint_id=$1`, p.EndpointID).Scan(&raw)
	var value computeCatalogBootstrap
	if err != nil {
		return value, err
	}
	if json.Unmarshal(raw, &value) != nil || value.Witness != branchResetBootstrapWitness || !sameBootstrapScope(value.payload(), p) || value.ConfigUID == "" || value.CredentialUID == "" || value.Operation == "" {
		return value, errBranchResetConfig
	}
	return value, nil
}

func storeBranchResetBootstrap(ctx context.Context, tx pgx.Tx, operation, worker string, p createPayload, value computeCatalogBootstrap) error {
	if !sameBootstrapScope(value.payload(), p) || value.ConfigUID == "" || value.CredentialUID == "" {
		return errBranchResetConfig
	}
	if err := assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	var authorized bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations o
 JOIN projects p ON p.id=o.project_id AND p.id=$2 AND p.state='ready' AND p.deleted_at IS NULL
 JOIN organizations org ON org.id=p.org_id AND org.state='active'
 JOIN users u ON u.id=o.actor_id AND NOT u.disabled
 JOIN organization_members m ON m.org_id=org.id AND m.user_id=u.id
 LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id
 WHERE o.id=$1 AND o.action='reset_branch' AND (m.role='admin' OR g.role='admin'))`, operation, p.ProjectID).Scan(&authorized); err != nil {
		return err
	}
	if !authorized {
		return errBranchResetConfig
	}
	value.Operation, value.Witness = operation, branchResetBootstrapWitness
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT proof FROM compute_catalog_reset_receipts WHERE operation_id=$1 AND endpoint_id=$2`, operation, p.EndpointID).Scan(&raw)
	if err == nil {
		var original computeCatalogBootstrap
		if json.Unmarshal(raw, &original) != nil || original != value {
			return errBranchResetConfig
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	var project, branch, tenant, timeline, endpointType, configUID, credentialUID string
	var eligible bool
	err = tx.QueryRow(ctx, `SELECT c.project_id,c.branch_id,c.tenant_id,c.target_timeline_id,
 k.endpoint_type,k.original_config_uid,k.original_credential_uid,
 b.state='restoring' AND b.timeline_id=c.original_timeline_id AND b.deleted_at IS NULL
 AND pr.state='ready' AND pr.source='managed' AND pr.deleted_at IS NULL
 AND e.state='suspended' AND e.deleted_at IS NULL AND e.workload_kind='neonvm'
 AND e.endpoint_type=k.endpoint_type AND org.state='active'
 AND EXISTS(SELECT 1 FROM users u JOIN organization_members m ON m.user_id=u.id AND m.org_id=org.id
   LEFT JOIN project_grants g ON g.user_id=u.id AND g.project_id=pr.id
   WHERE u.id=o.actor_id AND NOT u.disabled AND (m.role='admin' OR g.role='admin'))
 AND EXISTS(SELECT 1 FROM operation_steps step WHERE step.operation_id=o.id
   AND step.name='probe_restored_native_catalog' AND step.state='running')
 FROM branch_reset_checkpoints c JOIN operations o ON o.id=c.operation_id
   AND o.project_id=c.project_id AND o.resource_id=c.branch_id AND o.action='reset_branch'
 JOIN branch_reset_endpoint_backings k ON k.operation_id=c.operation_id AND k.endpoint_id=$2
 JOIN endpoints e ON e.id=k.endpoint_id AND e.branch_id=c.branch_id AND e.project_id=c.project_id
 JOIN branches b ON b.id=c.branch_id AND b.project_id=c.project_id
 JOIN projects pr ON pr.id=c.project_id JOIN organizations org ON org.id=pr.org_id
 WHERE c.operation_id=$1 FOR UPDATE OF c,k,b,e,pr`, operation, p.EndpointID).Scan(&project, &branch, &tenant, &timeline, &endpointType, &configUID, &credentialUID, &eligible)
	if err != nil {
		return err
	}
	if !eligible || project != p.ProjectID || branch != p.BranchID || tenant != p.TenantID || timeline != p.TimelineID || bootstrapEndpointType(p.EndpointType) != endpointType || configUID != value.ConfigUID || credentialUID != value.CredentialUID {
		return errBranchResetConfig
	}
	raw, err = json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO compute_catalog_reset_receipts(operation_id,endpoint_id,proof) VALUES($1,$2,$3::jsonb)`, operation, p.EndpointID, raw)
	return err
}

func currentComputeBootstrap(ctx context.Context, q connectionDefaultReader, p createPayload) (computeCatalogBootstrap, error) {
	value, err := readBranchResetBootstrap(ctx, q, p)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return value, err
	}
	var raw []byte
	err = q.QueryRow(ctx, `SELECT proof FROM compute_catalog_bootstrap_receipts WHERE endpoint_id=$1`, p.EndpointID).Scan(&raw)
	if err != nil {
		return value, err
	}
	if json.Unmarshal(raw, &value) != nil || !sameBootstrapScope(value.payload(), p) {
		return value, errBranchResetConfig
	}
	return value, nil
}
