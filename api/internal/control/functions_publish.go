package control

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

// This is a private Driver observation, never a client request. The trusted
// caller must verify actual VM/Service/bootstrap ownership, signed guest TLS
// status and restricted branch SQL outside the metadata transaction. These
// fields alone are not evidence that any external system was checked.
type functionReadyObservation struct {
	Status       functions.InstanceStatus `json:"-"`
	VMUID        string                   `json:"-"`
	ServiceUID   string                   `json:"-"`
	BootstrapUID string                   `json:"-"`
	ObservedAt   time.Time                `json:"-"`
}

var functionBootID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var errFunctionPublication = errors.New("Functions publication identity or readiness rejected")

func (o functionReadyObservation) valid(p functionInstanceTarget) bool {
	s := o.Status
	return p.valid() && s.Scope.Validate() == nil && s.Scope.ProjectID == p.ProjectID && s.Scope.BranchID == p.BranchID &&
		s.Scope.DeploymentID == p.DeploymentID && s.Scope.Slug == p.Slug && s.Scope.Generation == p.Generation &&
		functionBootID.MatchString(s.BootID) && s.Runtime == functions.Runtime && s.NodeReady && !s.Draining &&
		!s.BackgroundCountersTrusted && s.ActiveInvocations >= 0 && s.ActiveInvocations <= functions.MaxInvocations &&
		!s.LastActivity.IsZero() && !o.ObservedAt.IsZero() && !s.LastActivity.After(o.ObservedAt.Add(2*time.Second)) &&
		functionResourceUID.MatchString(o.VMUID) && functionResourceUID.MatchString(o.ServiceUID) && functionResourceUID.MatchString(o.BootstrapUID)
}

// Publication needs its own terminal-replay lease check: ordinary reservation
// correctly refuses a completed deployment. An unknown publication commit may
// replay only the same still-current completed active deployment under its own
// running Operation. A newer intent, old Worker or deleted parent rejects it.
func lockFunctionPublicationLease(ctx context.Context, tx pgx.Tx, operation, worker string, p functionInstanceTarget) error {
	if !p.valid() || operation == "" || worker == "" || len(worker) > 128 {
		return errFunctionPublication
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT project_id=$3 AND resource_id=$4 AND resource_type='function'
 AND action='deploy_function' AND state='running' AND lease_owner=$2
 AND lease_expires_at>clock_timestamp() FROM operations WHERE id=$1 FOR UPDATE`, operation, worker, p.ProjectID, p.FunctionID).Scan(&valid)
	if err != nil || !valid {
		return errLeaseLost
	}
	err = tx.QueryRow(ctx, `SELECT f.generation=$5 AND f.slug=$6 AND f.state NOT IN ('deleting','deleted')
 AND d.operation_id=$7 AND ((f.target_deployment_id=$4 AND d.state IN ('pending','building')) OR
  (f.active_deployment_id=$4 AND f.target_deployment_id IS NULL AND f.state='active' AND d.state='completed'))
 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL
 AND b.state='ready' AND b.deleted_at IS NULL
 AND e.endpoint_type='read_write' AND e.workload_kind='neonvm'
 AND e.state IN ('active','suspended') AND e.deleted_at IS NULL
 FROM function_definitions f JOIN function_deployments d ON d.id=$4 AND d.function_id=f.id
 JOIN projects p ON p.id=f.project_id JOIN branches b ON b.id=f.branch_id AND b.project_id=f.project_id
 JOIN endpoints e ON e.id=f.endpoint_id AND e.branch_id=f.branch_id AND e.project_id=f.project_id
 WHERE f.id=$1 AND f.project_id=$2 AND f.branch_id=$3 FOR UPDATE OF f,d,p,b,e`,
		p.FunctionID, p.ProjectID, p.BranchID, p.DeploymentID, p.Generation, p.Slug, operation).Scan(&valid)
	if err != nil || !valid {
		return errFunctionPublication
	}
	return nil
}

// Atomically make an already verified candidate and deployment current.
// Never infer draining/retirement from a health error or clear an old VM:
// a different nonretired instance is a hard conflict until the external Driver
// has actually drained/retired it. This conservative preview transaction does
// not promise uninterrupted replacement or act as an external admission fence.
// No Kubernetes, HTTP, certificate or branch SQL I/O occurs in this transaction.
// It does not register an execution API or enable the Functions capability.
func (s *server) publishFunctionInstance(ctx context.Context, operation, worker string, p functionInstanceTarget, observation functionReadyObservation) (bool, error) {
	if !observation.valid(p) {
		return false, errFunctionPublication
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, errFunctionInstanceStore
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionPublicationLease(ctx, tx, operation, worker, p); err != nil {
		return false, err
	}
	var state, deployment, active, target string
	var boot *string
	var exact bool
	err = tx.QueryRow(ctx, `SELECT i.state,i.boot_id,d.state,COALESCE(f.active_deployment_id,''),COALESCE(f.target_deployment_id,''),
 i.vm_uid=$6 AND i.service_uid=$7 AND b.secret_uid=$8 AND b.secret_name=i.bootstrap_secret_ref
 FROM function_instances i JOIN function_deployments d ON d.id=i.deployment_id
 JOIN function_definitions f ON f.id=i.function_id
 JOIN function_instance_bootstraps b ON b.instance_id=i.id
 JOIN function_deployment_backings a ON a.deployment_id=i.deployment_id
 WHERE i.id=$1 AND i.function_id=$2 AND i.deployment_id=$3 AND i.project_id=$4 AND i.branch_id=$5
 AND i.generation=$9 FOR UPDATE OF i`, observation.Status.Scope.InstanceID, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID,
		observation.VMUID, observation.ServiceUID, observation.BootstrapUID, p.Generation).Scan(&state, &boot, &deployment, &active, &target, &exact)
	if err != nil || !exact {
		return false, errFunctionPublication
	}
	if state == "ready" {
		if deployment != "completed" || active != p.DeploymentID || target != "" || boot == nil || *boot != observation.Status.BootID {
			return false, errFunctionPublication
		}
		// Replay confirms a prior atomic commit; it is not a fresh guest health claim.
		return true, commitFunctionInstance(ctx, tx, operation, worker)
	}
	if state != "starting" || deployment != "building" || target != p.DeploymentID || boot != nil {
		return false, errFunctionPublication
	}
	var fresh, foreignLive bool
	err = tx.QueryRow(ctx, `SELECT $1<=clock_timestamp()+interval '2 seconds' AND $1>=clock_timestamp()-interval '30 seconds',
 EXISTS(SELECT 1 FROM function_instances WHERE function_id=$2 AND state<>'retired' AND id<>$3)`, observation.ObservedAt, p.FunctionID, observation.Status.Scope.InstanceID).Scan(&fresh, &foreignLive)
	if err != nil || !fresh || foreignLive {
		return false, errFunctionPublication
	}
	if _, err = tx.Exec(ctx, `UPDATE function_instances SET state='ready',boot_id=$2,updated_at=now() WHERE id=$1`, observation.Status.Scope.InstanceID, observation.Status.BootID); err != nil {
		return false, errFunctionInstanceStore
	}
	if _, err = tx.Exec(ctx, `UPDATE function_deployments SET state='completed',finished_at=now() WHERE id=$1`, p.DeploymentID); err != nil {
		return false, errFunctionInstanceStore
	}
	// Version/generation belong to the admitted immutable intent; publication
	// does not create another spec revision or change the guest's generation.
	if _, err = tx.Exec(ctx, `UPDATE function_definitions SET active_deployment_id=$2,target_deployment_id=NULL,state='active',updated_at=now() WHERE id=$1`, p.FunctionID, p.DeploymentID); err != nil {
		return false, errFunctionInstanceStore
	}
	// A trigger/row lock may have waited after the first freshness check.
	if err = tx.QueryRow(ctx, `SELECT $1<=clock_timestamp()+interval '2 seconds' AND $1>=clock_timestamp()-interval '30 seconds'`, observation.ObservedAt).Scan(&fresh); err != nil || !fresh {
		return false, errFunctionPublication
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return false, err
	}
	return false, nil
}
