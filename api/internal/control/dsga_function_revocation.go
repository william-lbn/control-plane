package control

import (
	"context"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

func (b *dsgaCreationBinding) revokeDeletedDSGAFunctionsOnResume(ctx context.Context, boot dsgapi.Uint64) error {
	server := b.server
	scope := b.intent.createPayload
	rows, err := server.db.Query(ctx, `SELECT f.id,c.database_name,c.sql_schema,c.credential_generation
        FROM function_definitions f JOIN function_sql_credentials c
          ON c.function_id=f.id AND c.project_id=f.project_id AND c.branch_id=f.branch_id
          AND c.writer_id=f.endpoint_id AND c.credential_generation=f.sql_credential_generation
        WHERE f.project_id=$1 AND f.branch_id=$2 AND f.endpoint_id=$3 AND f.state='deleted' AND f.deleted_at IS NOT NULL
        ORDER BY f.id`, scope.ProjectID, scope.BranchID, scope.EndpointID)
	if err != nil {
		return err
	}
	var retired []functionSQLReference
	for rows.Next() {
		ref := functionSQLReference{WriterID: scope.EndpointID}
		ref.Scope.ProjectID, ref.Scope.BranchID = scope.ProjectID, scope.BranchID
		if err = rows.Scan(&ref.Scope.FunctionID, &ref.Scope.Database, &ref.Scope.Schema, &ref.Generation); err != nil {
			rows.Close()
			return err
		}
		retired = append(retired, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(retired) == 0 {
		return err
	}
	current, err := server.loadDSGACreationBinding(ctx, b.operationID, b.workerID)
	if err != nil || current.digest != b.digest || boot == 0 {
		return errDSGACreationIdentity
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		return err
	}
	runtime := &dsgaEndpointRuntime{Operation: b.operationID, Payload: scope, Plan: plan, VMUID: current.vmUID, ConfigUID: current.configUID}
	for _, ref := range retired {
		input := functionServiceLoginRequest(ref, ref.Generation+1, nil)
		input.PGBootID = boot
		if err := server.applyDSGAServiceLogin(ctx, runtime, input, func(ctx context.Context) error {
			tx, err := current.lockOperation(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			vm, config, err := current.read(ctx, tx)
			if err != nil || vm != runtime.VMUID || config != runtime.ConfigUID {
				return errDSGACreationIdentity
			}
			var exact bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM function_definitions f JOIN function_sql_credentials c
              ON c.function_id=f.id AND c.project_id=f.project_id AND c.branch_id=f.branch_id
              AND c.writer_id=f.endpoint_id AND c.credential_generation=f.sql_credential_generation
              WHERE f.id=$1 AND f.project_id=$2 AND f.branch_id=$3 AND f.endpoint_id=$4 AND f.state='deleted'
                AND f.deleted_at IS NOT NULL AND c.credential_generation=$5 AND c.database_name=$6 AND c.sql_schema=$7)`,
				ref.Scope.FunctionID, scope.ProjectID, scope.BranchID, scope.EndpointID, ref.Generation, ref.Scope.Database, ref.Scope.Schema).Scan(&exact)
			if err != nil || !exact {
				return errDSGACreationIdentity
			}
			return tx.Commit(ctx)
		}); err != nil {
			return err
		}
	}
	return nil
}
