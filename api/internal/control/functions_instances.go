package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

// This repository is a Driver prerequisite, not an execution admission flag.
// It performs only short metadata transactions. The future Driver must verify
// actual resource labels/UIDs and guest TLS before recording observations;
// lease validity in PostgreSQL is not external Kubernetes/Proxy fencing.
type functionInstanceTarget struct {
	FunctionID   string
	ProjectID    string
	BranchID     string
	DeploymentID string
	Generation   int64
	Slug         string
}

type functionInstanceRecord struct {
	Scope       functions.Scope
	VMName      string
	ServiceName string
	SecretName  string
	VMUID       *string
	ServiceUID  *string
	State       string
}

var functionDefinitionID = regexp.MustCompile(`^fnc_[a-f0-9]{16}$`)
var functionResourceUID = regexp.MustCompile(`^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
var errFunctionInstanceConflict = errors.New("Functions candidate identity or generation conflict")
var errFunctionInstanceCapacity = errors.New("Functions instance capacity exhausted")
var errFunctionInstanceStore = errors.New("Functions instance metadata unavailable")

func (p functionInstanceTarget) valid() bool {
	return functionDefinitionID.MatchString(p.FunctionID) && (functions.Scope{
		InstanceID: "fni_0000000000000000", DeploymentID: p.DeploymentID,
		ProjectID: p.ProjectID, BranchID: p.BranchID, Generation: p.Generation, Slug: p.Slug,
	}).Validate() == nil
}

// Lock the authoritative Operation before the definition, then check a live
// wall clock again immediately before commit. now() is transaction-start time
// and could accept a lease that expired while waiting on another row lock.
func lockFunctionInstanceLease(ctx context.Context, tx pgx.Tx, operation, worker string, p functionInstanceTarget) error {
	if !p.valid() || operation == "" || worker == "" || len(worker) > 128 {
		return errFunctionInstanceConflict
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT project_id=$3 AND resource_id=$4 AND resource_type='function'
 AND action='deploy_function' AND state='running' AND lease_owner=$2
 AND lease_expires_at>clock_timestamp() FROM operations WHERE id=$1 FOR UPDATE`, operation, worker, p.ProjectID, p.FunctionID).Scan(&valid)
	if err != nil || !valid {
		return errLeaseLost
	}
	// An Operation payload is not authoritative for parent state or deployment
	// membership. A concurrent deletion/intent advance must stop this candidate.
	err = tx.QueryRow(ctx, `SELECT f.generation=$5 AND f.target_deployment_id=$4
 AND f.slug=$6 AND f.state NOT IN ('deleting','deleted')
 AND d.operation_id=$7 AND d.state IN ('pending','building')
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
		return errFunctionInstanceConflict
	}
	return nil
}

func commitFunctionInstance(ctx context.Context, tx pgx.Tx, operation, worker string) error {
	var valid bool
	if tx.QueryRow(ctx, `SELECT state='running' AND lease_owner=$2 AND lease_expires_at>clock_timestamp()
 FROM operations WHERE id=$1`, operation, worker).Scan(&valid) != nil || !valid {
		return errLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return errFunctionInstanceStore
	}
	return nil
}

func functionInstanceStoreError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		if pgerr.Code == "23514" && pgerr.Message == "function VM budget exhausted" {
			return errFunctionInstanceCapacity
		}
		if pgerr.Code == "23505" {
			return errFunctionInstanceConflict
		}
	}
	return errFunctionInstanceStore
}

func scanFunctionInstance(row pgx.Row, p functionInstanceTarget) (functionInstanceRecord, error) {
	var item functionInstanceRecord
	item.Scope = functions.Scope{DeploymentID: p.DeploymentID, ProjectID: p.ProjectID, BranchID: p.BranchID, Generation: p.Generation, Slug: p.Slug}
	err := row.Scan(&item.Scope.InstanceID, &item.VMName, &item.ServiceName, &item.SecretName, &item.VMUID, &item.ServiceUID, &item.State)
	return item, err
}

const functionInstanceColumns = `id,vm_name,service_name,bootstrap_secret_ref,vm_uid,service_uid,state`

// reserveFunctionInstance survives lost replies and Worker restarts. It returns
// the same immutable candidate before generating another ID. Failed/draining
// instances hold the migration's global budget until a separate real retirement
// Driver has confirmed physical absence; this method never releases capacity.
func (s *server) reserveFunctionInstance(ctx context.Context, operation, worker string, p functionInstanceTarget) (functionInstanceRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var empty functionInstanceRecord
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, errFunctionInstanceStore
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, operation, worker, p); err != nil {
		return empty, err
	}
	item, err := scanFunctionInstance(tx.QueryRow(ctx, `SELECT `+functionInstanceColumns+`
 FROM function_instances WHERE function_id=$1 AND deployment_id=$2 AND project_id=$3 AND branch_id=$4
 AND generation=$5 AND state<>'retired' FOR UPDATE`, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID, p.Generation), p)
	if err == nil {
		if item.State != "provisioning" && item.State != "starting" {
			return empty, errFunctionInstanceConflict
		}
		if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
			return empty, err
		}
		return item, nil
	}
	if !isNoRows(err) {
		return empty, errFunctionInstanceStore
	}
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		return empty, errFunctionInstanceStore
	}
	id := "fni_" + hex.EncodeToString(suffix[:])
	name := "fn-" + hex.EncodeToString(suffix[:])
	item, err = scanFunctionInstance(tx.QueryRow(ctx, `INSERT INTO function_instances
 (id,function_id,deployment_id,project_id,branch_id,generation,vm_name,service_name,bootstrap_secret_ref,state)
 VALUES($1,$2,$3,$4,$5,$6,$7,$7,$7||'-bootstrap','provisioning') RETURNING `+functionInstanceColumns,
		id, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID, p.Generation, name), p)
	if err != nil {
		return empty, functionInstanceStoreError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE function_deployments SET state='building' WHERE id=$1 AND state='pending'`, p.DeploymentID); err != nil {
		return empty, errFunctionInstanceStore
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return empty, err
	}
	return item, nil
}

// recordFunctionInstanceResources records only a previously ownership-verified
// response/GET observation. Partial creation is durable, making unknown Service
// or VM writes recoverable by exact identity instead of blind replacement.
// It does not create/adopt resources, accept a new boot, or mark runtime ready.
func (s *server) recordFunctionInstanceResources(ctx context.Context, operation, worker string, p functionInstanceTarget, instance, vmUID, serviceUID string) (functionInstanceRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var empty functionInstanceRecord
	if (functions.Scope{InstanceID: instance, DeploymentID: p.DeploymentID, ProjectID: p.ProjectID, BranchID: p.BranchID, Slug: p.Slug, Generation: p.Generation}).Validate() != nil {
		return empty, errFunctionInstanceConflict
	}
	if (vmUID == "" && serviceUID == "") || (vmUID != "" && !functionResourceUID.MatchString(vmUID)) || (serviceUID != "" && !functionResourceUID.MatchString(serviceUID)) {
		return empty, errFunctionInstanceConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, errFunctionInstanceStore
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, operation, worker, p); err != nil {
		return empty, err
	}
	item, err := scanFunctionInstance(tx.QueryRow(ctx, `UPDATE function_instances SET
 vm_uid=COALESCE(vm_uid,NULLIF($7,'')),service_uid=COALESCE(service_uid,NULLIF($8,'')),updated_at=now(),
 state=CASE WHEN COALESCE(vm_uid,NULLIF($7,'')) IS NOT NULL AND COALESCE(service_uid,NULLIF($8,'')) IS NOT NULL THEN 'starting' ELSE 'provisioning' END
 WHERE id=$1 AND function_id=$2 AND deployment_id=$3 AND project_id=$4 AND branch_id=$5 AND generation=$6
 AND state IN ('provisioning','starting')
 AND ($7='' OR vm_uid IS NULL OR vm_uid=$7) AND ($8='' OR service_uid IS NULL OR service_uid=$8)
 RETURNING `+functionInstanceColumns, instance, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID, p.Generation, vmUID, serviceUID), p)
	if err != nil {
		if isNoRows(err) {
			return empty, errFunctionInstanceConflict
		}
		return empty, functionInstanceStoreError(err)
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return empty, err
	}
	return item, nil
}
