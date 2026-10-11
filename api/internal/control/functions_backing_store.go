package control

import (
	"context"
	"errors"
	"github.com/william-lbn/control-plane/api/internal/functions"
	"regexp"
	"time"
)

// This private receipt persists the already verified backing identities. It
// does not verify Kubernetes/S3, grant admission, provision a VM or rotate keys.
// Callers must verify actual backing outside the metadata transaction. Public
// read projections deliberately do not select this table or serialize refs.
type functionDeploymentBacking struct {
	Artifact      functionArtifactReference    `json:"-"`
	Environment   functionEnvironmentReference `json:"-"`
	MACKeyVersion string                       `json:"-"`
}

var functionMACKeyVersion = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var errFunctionBackingReceipt = errors.New("Functions backing receipt unavailable or identity rejected")

func (b functionDeploymentBacking) valid(p functionInstanceTarget) bool {
	key, err := functionArtifactKey(b.Artifact.ProjectID, b.Artifact.BranchID, b.Artifact.Digest)
	return p.valid() && err == nil && b.Artifact.Key == key && b.Artifact.ProjectID == p.ProjectID && b.Artifact.BranchID == p.BranchID &&
		b.Artifact.Size > 0 && b.Artifact.Size <= functions.MaxArchiveBytes && (b.Artifact.Entry == "index.js" || b.Artifact.Entry == "index.mjs") &&
		b.Environment.valid() && b.Environment.ProjectID == p.ProjectID && b.Environment.BranchID == p.BranchID && b.Environment.DeploymentID == p.DeploymentID &&
		b.Environment.UID != "" && functionArtifactDigest.MatchString(b.Environment.MAC) && functionMACKeyVersion.MatchString(b.MACKeyVersion)
}

// Lease/parent locking is shared with candidate reservation. A restart or lost
// commit reply must read and match the original UID/MAC/key version; neither a
// same-name replacement nor a new verification key may overwrite that receipt.
func (s *server) recordFunctionDeploymentBacking(ctx context.Context, operation, worker string, p functionInstanceTarget, backing functionDeploymentBacking) (functionDeploymentBacking, error) {
	var empty functionDeploymentBacking
	if !backing.valid(p) {
		return empty, errFunctionBackingReceipt
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, errFunctionBackingReceipt
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, operation, worker, p); err != nil {
		return empty, err
	}
	var matching bool
	err = tx.QueryRow(ctx, `SELECT bundle_digest=$2 AND bundle_bytes=$3 AND entry=$4 AND artifact_key=$5 AND environment_secret_ref=$6
 FROM function_deployments WHERE id=$1`, p.DeploymentID, backing.Artifact.Digest, backing.Artifact.Size, backing.Artifact.Entry, backing.Artifact.Key, backing.Environment.Name).Scan(&matching)
	if err != nil || !matching {
		return empty, errFunctionBackingReceipt
	}
	_, err = tx.Exec(ctx, `INSERT INTO function_deployment_backings
 (deployment_id,function_id,project_id,branch_id,environment_namespace,environment_secret_ref,environment_secret_uid,environment_mac,mac_key_version)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(deployment_id) DO NOTHING`,
		p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID, backing.Environment.Namespace, backing.Environment.Name, backing.Environment.UID, backing.Environment.MAC, backing.MACKeyVersion)
	if err != nil {
		return empty, errFunctionBackingReceipt
	}
	var namespace, name, uid, mac, version string
	err = tx.QueryRow(ctx, `SELECT environment_namespace,environment_secret_ref,environment_secret_uid,environment_mac,mac_key_version
 FROM function_deployment_backings WHERE deployment_id=$1 AND function_id=$2 AND project_id=$3 AND branch_id=$4`,
		p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID).Scan(&namespace, &name, &uid, &mac, &version)
	if err != nil || namespace != backing.Environment.Namespace || name != backing.Environment.Name || uid != backing.Environment.UID || mac != backing.Environment.MAC || version != backing.MACKeyVersion {
		return empty, errFunctionBackingReceipt
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return empty, err
	}
	return backing, nil
}

// Loading requires the same live Operation and current target. Missing receipts
// are rejected; old history is never backfilled from names or guessed UIDs.
// The future Driver must resolve the retained key version and re-read the actual
// artifact/Secret with these identities before building a private bootstrap.
func (s *server) loadFunctionDeploymentBacking(ctx context.Context, operation, worker string, p functionInstanceTarget) (functionDeploymentBacking, error) {
	var backing functionDeploymentBacking
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return backing, errFunctionBackingReceipt
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, operation, worker, p); err != nil {
		return backing, err
	}
	err = tx.QueryRow(ctx, `SELECT d.bundle_digest,d.bundle_bytes,d.entry,d.artifact_key,
 b.environment_namespace,b.environment_secret_ref,b.environment_secret_uid,b.environment_mac,b.mac_key_version
 FROM function_deployment_backings b JOIN function_deployments d ON d.id=b.deployment_id
 WHERE b.deployment_id=$1 AND b.function_id=$2 AND b.project_id=$3 AND b.branch_id=$4`,
		p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID).Scan(&backing.Artifact.Digest, &backing.Artifact.Size, &backing.Artifact.Entry, &backing.Artifact.Key,
		&backing.Environment.Namespace, &backing.Environment.Name, &backing.Environment.UID, &backing.Environment.MAC, &backing.MACKeyVersion)
	backing.Artifact.ProjectID = p.ProjectID
	backing.Artifact.BranchID = p.BranchID
	backing.Environment.ProjectID = p.ProjectID
	backing.Environment.BranchID = p.BranchID
	backing.Environment.DeploymentID = p.DeploymentID
	if err != nil || !backing.valid(p) {
		return functionDeploymentBacking{}, errFunctionBackingReceipt
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return functionDeploymentBacking{}, err
	}
	return backing, nil
}
