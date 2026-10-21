package control

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

var errFunctionCopy = errors.New("original Function copy source, authority or paused publication rejected")

type functionCopySource struct {
	Actor       string
	Function    string
	Branch      string
	Deployment  string
	Summary     functions.DeploymentSummary
	Environment functionEnvironmentReference
	KeyVersion  string
}

func (s *server) loadFunctionCopySource(ctx context.Context, p functionInstanceTarget) (functionCopySource, error) {
	var source functionCopySource
	err := s.db.QueryRow(ctx, `SELECT c.admitted_actor_id,c.source_function_id,c.source_branch_id,c.source_deployment_id,
 d.runtime,d.bundle_digest,d.bundle_bytes,d.entry,d.environment_names,
 b.environment_namespace,b.environment_secret_ref,b.environment_secret_uid,b.environment_mac,b.mac_key_version
 FROM function_copy_sources c JOIN function_deployments d ON d.id=c.source_deployment_id
 AND d.function_id=c.source_function_id AND d.project_id=c.project_id AND d.branch_id=c.source_branch_id
 JOIN function_deployment_backings b ON b.deployment_id=d.id AND b.function_id=d.function_id
 AND b.project_id=d.project_id AND b.branch_id=d.branch_id
 WHERE c.deployment_id=$1 AND c.function_id=$2 AND c.project_id=$3 AND c.branch_id=$4 AND d.state='completed'`, p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID).Scan(&source.Actor, &source.Function, &source.Branch, &source.Deployment, &source.Summary.Runtime, &source.Summary.BundleDigest, &source.Summary.BundleBytes, &source.Summary.Entry, &source.Summary.EnvironmentNames, &source.Environment.Namespace, &source.Environment.Name, &source.Environment.UID, &source.Environment.MAC, &source.KeyVersion)
	source.Environment.ProjectID, source.Environment.BranchID, source.Environment.DeploymentID = p.ProjectID, source.Branch, source.Deployment
	if err != nil || source.KeyVersion != s.functions.input.KeyVersion || !source.Environment.valid() {
		return functionCopySource{}, errFunctionCopy
	}
	return source, nil
}

func (s *server) authorizeFunctionCopyActor(ctx context.Context, project, actor string) error {
	access, err := s.projectAccess(ctx, project, actor)
	var enabled bool
	if err != nil || access.Level < 2 || s.db.QueryRow(ctx, `SELECT NOT disabled FROM users WHERE id=$1`, actor).Scan(&enabled) != nil || !enabled {
		return errFunctionCopy
	}
	return nil
}

// A completed paused copy has no Guest observation. Its replay is confirmed
// separately from the existing active-instance publication contract.
func lockFunctionCopyLease(ctx context.Context, tx pgx.Tx, op, worker string, p functionInstanceTarget) (bool, error) {
	if !p.valid() {
		return false, errFunctionCopy
	}
	if err := assertCreateLeaseTx(ctx, tx, op, worker); err != nil {
		return false, err
	}
	var committed, valid bool
	err := tx.QueryRow(ctx, `SELECT
 f.state='suspended' AND f.active_deployment_id=d.id AND f.target_deployment_id IS NULL AND d.state='completed',
 f.generation=$5 AND f.slug=$6 AND f.state NOT IN ('deleting','deleted')
 AND o.project_id=f.project_id AND o.resource_id=f.id AND o.resource_type='function' AND o.action='deploy_function'
 AND d.operation_id=o.id AND c.admitted_operation_id=o.id
 AND ((f.target_deployment_id=d.id AND d.state IN ('pending','building')) OR
  (f.active_deployment_id=d.id AND f.target_deployment_id IS NULL AND f.state='suspended' AND d.state='completed'))
 AND project.state='ready' AND project.source='managed' AND project.deleted_at IS NULL AND org.state='active'
 AND branch.state='ready' AND branch.deleted_at IS NULL
 AND e.endpoint_type='read_write' AND e.workload_kind='neonvm' AND e.state IN ('active','suspended') AND e.deleted_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM function_instances WHERE function_id=f.id)
 FROM function_definitions f JOIN function_deployments d ON d.id=$4 AND d.function_id=f.id
 JOIN function_copy_sources c ON c.deployment_id=d.id AND c.function_id=f.id AND c.project_id=f.project_id AND c.branch_id=f.branch_id
 JOIN operations o ON o.id=$7
 JOIN projects project ON project.id=f.project_id JOIN organizations org ON org.id=project.org_id
 JOIN branches branch ON branch.id=f.branch_id AND branch.project_id=f.project_id
 JOIN endpoints e ON e.id=f.endpoint_id AND e.project_id=f.project_id AND e.branch_id=f.branch_id
 WHERE f.id=$1 AND f.project_id=$2 AND f.branch_id=$3 FOR UPDATE OF f,d,project,branch,e`, p.FunctionID, p.ProjectID, p.BranchID, p.DeploymentID, p.Generation, p.Slug, op).Scan(&committed, &valid)
	if err != nil || !valid {
		return false, errFunctionCopy
	}
	return committed, nil
}

func (s *server) prepareFunctionCopyBacking(ctx context.Context, op, worker string, p functionInstanceTarget, source functionCopySource) error {
	if err := s.authorizeFunctionCopyActor(ctx, p.ProjectID, source.Actor); err != nil {
		return err
	}
	if err := s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
		return err
	}
	sourceKey, err := functionArtifactKey(p.ProjectID, source.Branch, source.Summary.BundleDigest)
	if err != nil {
		return err
	}
	original := functionArtifactReference{ProjectID: p.ProjectID, BranchID: source.Branch, Digest: source.Summary.BundleDigest, Size: int64(source.Summary.BundleBytes), Entry: source.Summary.Entry, Key: sourceKey}
	archive, err := s.storage.readFunctionArtifact(ctx, original)
	if err != nil {
		return err
	}
	environment, err := s.kube.readFunctionEnvironment(ctx, s.functions.key, source.Environment)
	if err != nil {
		return err
	}
	snapshot, err := functions.RestoreDeploymentSnapshot(source.Summary, environment)
	if err != nil {
		return errFunctionCopy
	}
	if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
		return err
	}
	artifact, err := s.storage.reserveFunctionArtifact(ctx, p.ProjectID, p.BranchID, archive)
	if err != nil {
		return err
	}
	if err = s.authorizeFunctionCopyActor(ctx, p.ProjectID, source.Actor); err != nil {
		return err
	}
	if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
		return err
	}
	verified, err := s.kube.reserveFunctionEnvironment(ctx, s.functions.key, p.ProjectID, p.BranchID, p.DeploymentID, snapshot.SecretEnvironment())
	if err != nil {
		return err
	}
	_, err = s.recordFunctionDeploymentBacking(ctx, op, worker, p, functionDeploymentBacking{Artifact: artifact, Environment: verified, MACKeyVersion: s.functions.input.KeyVersion})
	return err
}

func (s *server) reconcileFunctionBranchCopy(ctx context.Context, op, worker string, p functionInstanceTarget) error {
	if !s.functionBranchCopyEnabled() || s.processRole != roleWorker {
		return errFunctionsPlatform
	}
	source, err := s.loadFunctionCopySource(ctx, p)
	if err != nil {
		return err
	}
	if err = s.authorizeFunctionCopyActor(ctx, p.ProjectID, source.Actor); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	committed, err := lockFunctionCopyLease(ctx, tx, op, worker, p)
	if err != nil {
		return err
	}
	if !committed {
		_, err = tx.Exec(ctx, `UPDATE function_definitions SET state='provisioning',updated_at=now() WHERE id=$1 AND state='degraded'`, p.FunctionID)
		if err != nil {
			return err
		}
	}
	if err = commitFunctionInstance(ctx, tx, op, worker); err != nil {
		return err
	}
	if committed {
		return s.setCreateStep(ctx, op, worker, 2, "succeeded", "Original paused copy publication confirmed; no Guest readiness claim")
	}
	if err = s.functionDeployStep(ctx, op, worker, 0, func() error { return s.prepareFunctionCopyBacking(ctx, op, worker, p, source) }); err != nil {
		return err
	}
	if err = s.functionDeployStep(ctx, op, worker, 1, func() error { _, e := s.reconcileFunctionSQL(ctx, op, worker, p); return e }); err != nil {
		return err
	}
	return s.functionDeployStep(ctx, op, worker, 2, func() error {
		if err := s.authorizeFunctionCopyActor(ctx, p.ProjectID, source.Actor); err != nil {
			return err
		}
		call, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		final, err := s.db.Begin(call)
		if err != nil {
			return err
		}
		defer final.Rollback(call)
		committed, err := lockFunctionCopyLease(call, final, op, worker, p)
		if err != nil {
			return err
		}
		if !committed {
			var verified bool
			err = final.QueryRow(call, `SELECT EXISTS(SELECT 1 FROM function_sql_credentials c JOIN function_definitions f ON f.id=c.function_id JOIN function_deployment_backings b ON b.deployment_id=$2 WHERE c.function_id=$1 AND c.project_id=$3 AND c.branch_id=$4 AND c.writer_id=f.endpoint_id AND c.database_name=f.database_name AND c.sql_schema=f.sql_schema AND b.function_id=f.id AND b.project_id=c.project_id AND b.branch_id=c.branch_id)`, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID).Scan(&verified)
			if err != nil || !verified {
				return errFunctionCopy
			}
			if _, err = final.Exec(call, `UPDATE function_deployments SET state='completed',finished_at=now() WHERE id=$1`, p.DeploymentID); err != nil {
				return err
			}
			if _, err = final.Exec(call, `UPDATE function_definitions SET state='suspended',active_deployment_id=$2,target_deployment_id=NULL,updated_at=now() WHERE id=$1`, p.FunctionID, p.DeploymentID); err != nil {
				return err
			}
		}
		return commitFunctionInstance(call, final, op, worker)
	})
}
