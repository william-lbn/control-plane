package control

import (
	"context"
	"time"
)

// Persist only a real, verified Kubernetes bootstrap UID. The candidate and
// backing identities must have been reserved under the same live Operation.
// Kube/TLS/artifact reads stay outside this short metadata transaction.
func (s *server) recordFunctionBootstrap(ctx context.Context, operation, worker string, p functionInstanceTarget, ref functionBootstrapReference) (functionBootstrapReference, error) {
	var empty functionBootstrapReference
	if !ref.valid() || ref.UID == "" || ref.FunctionID != p.FunctionID || ref.Scope.ProjectID != p.ProjectID || ref.Scope.BranchID != p.BranchID ||
		ref.Scope.DeploymentID != p.DeploymentID || ref.Scope.Generation != p.Generation || ref.Scope.Slug != p.Slug {
		return empty, errFunctionBootstrap
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, errFunctionBootstrap
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, operation, worker, p); err != nil {
		return empty, err
	}
	var matches bool
	err = tx.QueryRow(ctx, `SELECT i.generation=$6 AND i.bootstrap_secret_ref=$7 AND i.state IN ('provisioning','starting')
 AND d.bundle_digest=$8 AND f.endpoint_id=$9
 FROM function_instances i JOIN function_deployments d ON d.id=i.deployment_id
 JOIN function_definitions f ON f.id=i.function_id JOIN function_deployment_backings b ON b.deployment_id=d.id
 WHERE i.id=$1 AND i.function_id=$2 AND i.deployment_id=$3 AND i.project_id=$4 AND i.branch_id=$5 FOR UPDATE OF i`,
		ref.Scope.InstanceID, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID, p.Generation, ref.Name, ref.BundleDigest, ref.WriterID).Scan(&matches)
	if err != nil || !matches {
		return empty, errFunctionBootstrap
	}
	_, err = tx.Exec(ctx, `INSERT INTO function_instance_bootstraps
 (instance_id,function_id,deployment_id,project_id,branch_id,namespace,secret_name,secret_uid,content_mac,mac_key_version)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(instance_id) DO NOTHING`,
		ref.Scope.InstanceID, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID, ref.Namespace, ref.Name, ref.UID, ref.MAC, ref.MACKeyVersion)
	if err != nil {
		return empty, errFunctionBootstrap
	}
	var namespace, name, uid, mac, kid string
	err = tx.QueryRow(ctx, `SELECT namespace,secret_name,secret_uid,content_mac,mac_key_version FROM function_instance_bootstraps
 WHERE instance_id=$1 AND function_id=$2 AND deployment_id=$3 AND project_id=$4 AND branch_id=$5`,
		ref.Scope.InstanceID, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID).Scan(&namespace, &name, &uid, &mac, &kid)
	if err != nil || namespace != ref.Namespace || name != ref.Name || uid != ref.UID || mac != ref.MAC || kid != ref.MACKeyVersion {
		return empty, errFunctionBootstrap
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return empty, err
	}
	return ref, nil
}

// An absent durable receipt is an error, not permission to guess or adopt a
// same-name Secret. The Driver may explicitly resolve an unknown initial write
// by findFunctionBootstrap and then record its verified original identity.
func (s *server) loadFunctionBootstrap(ctx context.Context, operation, worker string, p functionInstanceTarget, instance string) (functionBootstrapReference, error) {
	var ref functionBootstrapReference
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ref, errFunctionBootstrap
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, operation, worker, p); err != nil {
		return ref, err
	}
	err = tx.QueryRow(ctx, `SELECT b.namespace,b.secret_name,b.secret_uid,b.content_mac,b.mac_key_version,
 d.bundle_digest,f.endpoint_id,i.generation,i.id
 FROM function_instance_bootstraps b JOIN function_instances i ON i.id=b.instance_id
 JOIN function_deployments d ON d.id=b.deployment_id JOIN function_definitions f ON f.id=b.function_id
 WHERE b.instance_id=$1 AND b.function_id=$2 AND b.deployment_id=$3 AND b.project_id=$4 AND b.branch_id=$5
 AND i.state IN ('provisioning','starting') FOR UPDATE OF i`, instance, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID).
		Scan(&ref.Namespace, &ref.Name, &ref.UID, &ref.MAC, &ref.MACKeyVersion, &ref.BundleDigest, &ref.WriterID, &ref.Scope.Generation, &ref.Scope.InstanceID)
	ref.FunctionID = p.FunctionID
	ref.Scope.DeploymentID = p.DeploymentID
	ref.Scope.ProjectID = p.ProjectID
	ref.Scope.BranchID = p.BranchID
	ref.Scope.Slug = p.Slug
	if err != nil || !ref.valid() || ref.Scope.Generation != p.Generation {
		return functionBootstrapReference{}, errFunctionBootstrap
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return functionBootstrapReference{}, err
	}
	return ref, nil
}
