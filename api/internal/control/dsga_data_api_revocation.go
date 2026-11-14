package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

func (s *server) revokeDSGADataAPILogin(ctx context.Context, p dataAPIPayload, authorize func(context.Context) error) error {
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
		return s.reconcileDSGAServiceLogin(ctx, runtime, dataAPILoginRequest(p, nil), authorize)
	case "suspended":
		return s.confirmSuspendedDSGAServiceWriter(ctx, runtime, authorize)
	default:
		return errDSGACreationIdentity
	}
}

// Reconcile a durable disable, including rows disabled before the native
// Driver. A failed enable is recovered by retrying its own Operation; it does
// not become a revoke at the same generation.
func readDisabledDSGADataAPI(ctx context.Context, db connectionDefaultReader, scope createPayload) (dataAPIPayload, bool, error) {
	p := dataAPIPayload{ProjectID: scope.ProjectID, BranchID: scope.BranchID, EndpointID: scope.EndpointID}
	var raw []byte
	var state string
	err := db.QueryRow(ctx, `SELECT generation,state,spec,secret_ref FROM data_api_instances
        WHERE project_id=$1 AND branch_id=$2 AND endpoint_id=$3`, p.ProjectID, p.BranchID, p.EndpointID).
		Scan(&p.Generation, &state, &raw, &p.SecretRef)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && state != "disabled" {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if p.Generation <= 0 || json.Unmarshal(raw, &p.Spec) != nil || !validPGIdentifier(p.Spec.Database) {
		return p, false, errDSGACreationIdentity
	}
	return p, true, nil
}

func (b *dsgaCreationBinding) revokeDisabledDSGADataAPIOnResume(ctx context.Context, boot dsgapi.Uint64) error {
	s := b.server
	scope := b.intent.createPayload
	p, disabled, err := readDisabledDSGADataAPI(ctx, s.db, scope)
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
	input := dataAPILoginRequest(p, nil)
	input.PGBootID = boot
	return s.applyDSGAServiceLogin(ctx, runtime, input, func(ctx context.Context) error {
		tx, err := current.lockOperation(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		vm, config, err := current.read(ctx, tx)
		if err != nil || vm != runtime.VMUID || config != runtime.ConfigUID {
			return errDSGACreationIdentity
		}
		latest, disabled, err := readDisabledDSGADataAPI(ctx, tx, scope)
		if err != nil || !disabled || latest.Generation != p.Generation || latest.SecretRef != p.SecretRef || latest.Spec.Database != p.Spec.Database {
			return errDSGACreationIdentity
		}
		return tx.Commit(ctx)
	})
}
