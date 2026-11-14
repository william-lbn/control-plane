package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s *server) revokeDSGAAuthLogin(ctx context.Context, p managedAuthPayload, authorize func(context.Context) error) error {
	release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
	if err != nil {
		return err
	}
	defer release()
	runtime, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.EndpointID)
	if err != nil || runtime == nil {
		return err
	}
	var state string
	err = s.db.QueryRow(ctx, `SELECT state FROM endpoints
 WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND endpoint_type='read_write' AND deleted_at IS NULL`,
		p.EndpointID, p.ProjectID, p.BranchID).Scan(&state)
	if err != nil {
		return err
	}
	switch state {
	case "active":
		return s.reconcileDSGAAuthLogin(ctx, runtime, p, nil, authorize)
	case "suspended":
		return s.deferDSGAAuthRevocation(ctx, runtime, authorize)
	default:
		return errDSGACreationIdentity
	}
}

// Confirm that there is no live Writer to revoke. The disabled metadata
// generation remains the durable intent and is applied before publishing a
// successor's route, even for rows disabled by older releases.
func (s *server) deferDSGAAuthRevocation(ctx context.Context, runtime *dsgaEndpointRuntime, authorize func(context.Context) error) error {
	p := runtime.Payload
	var suspension string
	err := s.db.QueryRow(ctx, `SELECT o.id FROM operations o JOIN dsga_suspension_retirements r ON r.operation_id=o.id
 WHERE o.project_id=$1 AND o.resource_type='endpoint' AND o.resource_id=$2
 AND o.action=$3 AND o.state='succeeded' AND o.payload->>'creation_operation_id'=$4 AND r.vm_uid=$5`,
		p.ProjectID, p.EndpointID, dsgaSuspendAction, runtime.Operation, runtime.VMUID).Scan(&suspension)
	if err != nil {
		return err
	}
	original, err := readDSGAResumeSource(ctx, s.db, p, suspension)
	if err != nil || original.Creation != runtime.Operation || original.VMUID != runtime.VMUID {
		return errDSGACreationIdentity
	}
	_, vms, err := newDSGAKubeClients(s.kube)
	if err != nil {
		return err
	}
	if _, err = vms.NeonvmV1().VirtualMachines(runtime.Plan.Namespace).Get(ctx, runtime.binding().Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return errDSGACreationIdentity
	}
	if err = managedSQLReplacementRetired(ctx, s.kube, adapter.ManagedSQLBinding{
		Namespace: runtime.Plan.Namespace, EndpointID: p.EndpointID, VMUID: original.VMUID, Incarnation: original.Incarnation,
	}); err != nil {
		return err
	}
	return authorize(ctx)
}

func readDisabledDSGAAuth(ctx context.Context, db connectionDefaultReader, scope createPayload) (managedAuthPayload, bool, error) {
	p := managedAuthPayload{ProjectID: scope.ProjectID, BranchID: scope.BranchID, EndpointID: scope.EndpointID}
	var raw []byte
	var state string
	err := db.QueryRow(ctx, `SELECT generation,state,spec,secret_ref FROM managed_auth_instances
 WHERE project_id=$1 AND branch_id=$2 AND endpoint_id=$3`, p.ProjectID, p.BranchID, p.EndpointID).
		Scan(&p.Generation, &state, &raw, &p.SecretRef)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && state == "active" {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if state != "disabled" || p.Generation <= 0 || json.Unmarshal(raw, &p.Spec) != nil || !validPGIdentifier(p.Spec.Database) {
		return p, false, errDSGACreationIdentity
	}
	return p, true, nil
}

func (b *dsgaCreationBinding) revokeDisabledDSGAAuthOnResume(ctx context.Context, boot dsgapi.Uint64) error {
	s := b.server
	scope := b.intent.createPayload
	p, disabled, err := readDisabledDSGAAuth(ctx, s.db, scope)
	if !disabled || err != nil {
		return err
	}
	current, err := s.loadDSGACreationBinding(ctx, b.operationID, b.workerID)
	if err != nil || current.digest != b.digest || boot == 0 {
		return errDSGACreationIdentity
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		return err
	}
	runtime := &dsgaEndpointRuntime{Operation: b.operationID, Payload: scope, Plan: plan,
		VMUID: current.vmUID, ConfigUID: current.configUID}
	return s.applyDSGAAuthLogin(ctx, runtime, p, nil, boot, func(ctx context.Context) error {
		tx, err := current.lockOperation(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		vm, config, err := current.read(ctx, tx)
		if err != nil || vm != runtime.VMUID || config != runtime.ConfigUID {
			return errDSGACreationIdentity
		}
		latest, disabled, err := readDisabledDSGAAuth(ctx, tx, scope)
		if err != nil || !disabled || latest.Generation != p.Generation || latest.SecretRef != p.SecretRef || latest.Spec.Database != p.Spec.Database {
			return errDSGACreationIdentity
		}
		return tx.Commit(ctx)
	})
}
