package control

import (
	"context"
	"time"
)

func functionSQLMatchesTarget(p functionInstanceTarget, r functionSQLReference) bool {
	return p.valid() && r.valid() && r.UID != "" && r.MAC != "" && r.Generation > 0 && r.Scope.ProvisioningRole == "control_probe" &&
		r.Scope.FunctionID == p.FunctionID && r.Scope.ProjectID == p.ProjectID && r.Scope.BranchID == p.BranchID
}

// The same operation/parent lock and wall-clock commit fence used by candidate
// reservation apply here. The caller must first verify the actual Secret. No
// Kubernetes request, SQL-role mutation or credential value occurs in this tx.
func (s *server) recordFunctionSQLCredential(ctx context.Context, operation, worker string, p functionInstanceTarget, r functionSQLReference) (functionSQLReference, error) {
	var empty functionSQLReference
	if !functionSQLMatchesTarget(p, r) {
		return empty, errFunctionSQLSecret
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, errFunctionSQLSecret
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, operation, worker, p); err != nil {
		return empty, err
	}
	var matches bool
	err = tx.QueryRow(ctx, `SELECT endpoint_id=$2 AND database_name=$3 AND sql_schema=$4 AND sql_role=$5 AND sql_credential_generation=$6
 FROM function_definitions WHERE id=$1`, p.FunctionID, r.WriterID, r.Scope.Database, r.Scope.Schema, r.Scope.Role(), r.Generation).Scan(&matches)
	if err != nil || !matches {
		return empty, errFunctionSQLSecret
	}
	_, err = tx.Exec(ctx, `INSERT INTO function_sql_credentials
 (function_id,project_id,branch_id,credential_generation,writer_id,database_name,sql_schema,provisioning_role,
 namespace,secret_name,secret_uid,content_mac,mac_key_version,created_operation_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT(function_id,credential_generation) DO NOTHING`,
		p.FunctionID, p.ProjectID, p.BranchID, r.Generation, r.WriterID, r.Scope.Database, r.Scope.Schema, r.Scope.ProvisioningRole,
		r.Namespace, r.Name, r.UID, r.MAC, r.MACKeyVersion, operation)
	if err != nil {
		return empty, errFunctionSQLSecret
	}
	actual := r
	err = tx.QueryRow(ctx, `SELECT namespace,secret_name,secret_uid,content_mac,mac_key_version,writer_id,database_name,sql_schema,provisioning_role
 FROM function_sql_credentials WHERE function_id=$1 AND project_id=$2 AND branch_id=$3 AND credential_generation=$4`,
		p.FunctionID, p.ProjectID, p.BranchID, r.Generation).Scan(&actual.Namespace, &actual.Name, &actual.UID, &actual.MAC, &actual.MACKeyVersion,
		&actual.WriterID, &actual.Scope.Database, &actual.Scope.Schema, &actual.Scope.ProvisioningRole)
	if err != nil || actual != r {
		return empty, errFunctionSQLSecret
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return empty, err
	}
	return r, nil
}

// A missing receipt is a failure. Only the original leased deployment may
// resolve an unknown initial result by finding/verifying its original Secret,
// then recording that exact identity. Never fabricate a receipt from a name.
func (s *server) loadFunctionSQLCredential(ctx context.Context, operation, worker string, p functionInstanceTarget) (functionSQLReference, error) {
	var r functionSQLReference
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return r, errFunctionSQLSecret
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, operation, worker, p); err != nil {
		return r, err
	}
	r.Scope.FunctionID = p.FunctionID
	r.Scope.ProjectID = p.ProjectID
	r.Scope.BranchID = p.BranchID
	err = tx.QueryRow(ctx, `SELECT c.credential_generation,c.namespace,c.secret_name,c.secret_uid,c.content_mac,c.mac_key_version,
 c.writer_id,c.database_name,c.sql_schema,c.provisioning_role
 FROM function_sql_credentials c JOIN function_definitions f ON f.id=c.function_id
 WHERE c.function_id=$1 AND c.project_id=$2 AND c.branch_id=$3 AND c.credential_generation=f.sql_credential_generation
 AND f.endpoint_id=c.writer_id AND f.database_name=c.database_name AND f.sql_schema=c.sql_schema
 AND f.sql_role=$4`, p.FunctionID, p.ProjectID, p.BranchID, r.Scope.Role()).Scan(&r.Generation, &r.Namespace, &r.Name, &r.UID, &r.MAC, &r.MACKeyVersion,
		&r.WriterID, &r.Scope.Database, &r.Scope.Schema, &r.Scope.ProvisioningRole)
	if err != nil || !functionSQLMatchesTarget(p, r) {
		return functionSQLReference{}, errFunctionSQLSecret
	}
	if err = commitFunctionInstance(ctx, tx, operation, worker); err != nil {
		return functionSQLReference{}, err
	}
	return r, nil
}
