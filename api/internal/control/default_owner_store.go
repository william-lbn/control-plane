package control

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func checkDefaultOwnerReservation(ctx context.Context, db connectionDefaultReader, project, branch string) error {
	var held bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM default_database_owners WHERE project_id=$1 AND branch_id=$2 AND state='updating')`, project, branch).Scan(&held)
	if err != nil {
		return err
	}
	if held {
		return renameFault(409, "default_owner_operation_in_progress", "Complete or retry the original default database Owner Operation first")
	}
	return nil
}
func checkDefaultOwnerDependency(ctx context.Context, db connectionDefaultReader, project, branch, kind, name string) error {
	if err := checkCatalogForkExclusion(ctx, db, project, branch, kind, name); err != nil {
		return err
	}
	if err := checkDefaultOwnerReservation(ctx, db, project, branch); err != nil {
		return err
	}
	var held bool
	if kind == "role" {
		err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM default_database_owners WHERE project_id=$1 AND branch_id=$2 AND mode='managed' AND owner_name=$3)`, project, branch, name).Scan(&held)
		if err != nil {
			return err
		}
	} else {
		err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND database_name=$3 AND deleted_at IS NULL)`, project, branch, name).Scan(&held)
		if err != nil {
			return err
		}
	}
	if held {
		return renameFault(409, "default_database_owner_dependency", "Migrate the original default database Owner or use its dedicated workflow before changing this object")
	}
	return nil
}
func verifyDefaultOwnerMetadata(ctx context.Context, db connectionDefaultReader, p defaultOwnerPayload, operation string, updating bool) error {
	c, err := readDefaultOwnerContext(ctx, db, p.ProjectID, p.BranchID)
	if err != nil {
		return err
	}
	if c.EndpointID != p.EndpointID || c.Database != p.Name || c.PlatformRole != p.Native.PlatformRole || c.Scope != p.Native.Scope {
		return renameFault(409, "default_owner_scope_changed", "Original default database, Writer or timeline changed")
	}
	b, err := readDefaultOwnerBinding(ctx, db, p.ProjectID, p.BranchID)
	if err != nil {
		return err
	}
	if b == nil {
		if !p.Initial || p.ExpectedVersion != 1 || updating {
			return renameFault(412, "version_conflict", "Original default database Owner binding changed")
		}
	} else {
		expectedState := "ready"
		if updating {
			expectedState = "updating"
		}
		if b.Version != p.ExpectedVersion || b.State != expectedState || b.Database != p.Name || b.DatabaseOID != p.DatabaseOID || b.Scope != p.Native.Scope || b.PlatformRole != p.Native.PlatformRole || b.PlatformRoleOID != p.Native.PlatformRoleOID || b.Owner != p.PreviousOwner || b.OwnerOID != p.Native.PreviousOwnerOID || (updating && b.OperationID != operation) || (!updating && p.Initial) {
			return renameFault(412, "default_owner_intent_changed", "Original default database Owner intent changed")
		}
	}
	if p.Mode == "managed" {
		var valid bool
		err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=$1 AND name=$2 AND state='ready' AND version=$3 AND credential_ref=$4)`, p.BranchID, p.Owner, p.TargetRoleVersion, p.TargetCredentialRef).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return renameFault(409, "managed_owner_intent_changed", "Original managed Owner intent changed")
		}
	}
	var state, owner string
	var version int64
	err = db.QueryRow(ctx, `SELECT state,owner_name,version FROM branch_databases WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name).Scan(&state, &owner, &version)
	if err != nil && !isNoRows(err) {
		return err
	}
	if p.LegacyDatabaseVersion == 0 {
		if err == nil {
			return renameFault(409, "default_owner_legacy_intent_changed", "Default database custom-catalog intent appeared")
		}
	} else if isNoRows(err) || state != "ready" || owner != p.PreviousOwner || version != p.LegacyDatabaseVersion {
		return renameFault(412, "default_owner_legacy_intent_changed", "Original default database custom-catalog intent changed")
	}
	return nil
}
func lockDefaultOwnerParents(ctx context.Context, tx pgx.Tx, p defaultOwnerPayload) error {
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active'
 FROM projects p JOIN branches b ON b.project_id=p.id JOIN organizations o ON o.id=p.org_id WHERE p.id=$1 AND b.id=$2 FOR UPDATE OF p,b`, p.ProjectID, p.BranchID).Scan(&eligible)
	if err != nil && !isNoRows(err) {
		return err
	}
	if isNoRows(err) || !eligible {
		return renameFault(409, "default_owner_parent_changed", "Original ready project and branch required")
	}
	return nil
}
func verifyDefaultOwnerEndpointSet(ctx context.Context, tx pgx.Tx, p defaultOwnerPayload) error {
	return verifyCatalogRenameEndpointSet(ctx, tx, catalogRenamePayload{catalogPayload: p.catalogPayload, ConfigUIDs: p.ConfigUIDs})
}
func admitDefaultOwnerTx(ctx context.Context, tx pgx.Tx, p defaultOwnerPayload, operation, actor, request, key, hash, keyVersion string, sealed []byte) error {
	if !p.valid() || !defaultOwnerOperationID.MatchString(operation) || keyVersion == "" || len(sealed) < 28 || len(sealed) > 284 {
		return errors.New("invalid default database Owner admission")
	}
	var orgRole, grant string
	err := tx.QueryRow(ctx, `SELECT m.role,COALESCE(g.role,'') FROM projects p JOIN branches b ON b.project_id=p.id JOIN organizations o ON o.id=p.org_id
 JOIN users u ON u.id=$3 AND NOT u.disabled JOIN organization_members m ON m.org_id=o.id AND m.user_id=u.id LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id
 WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active' FOR UPDATE OF p,b`, p.ProjectID, p.BranchID, actor).Scan(&orgRole, &grant)
	if err != nil && !isNoRows(err) {
		return err
	}
	if isNoRows(err) || effectivePermission(orgRole, grant) < 3 {
		return renameFault(403, "forbidden", "Current project Admin required")
	}
	if err = verifyDefaultOwnerMetadata(ctx, tx, p, operation, false); err != nil {
		return err
	}
	if err = verifyDefaultOwnerEndpointSet(ctx, tx, p); err != nil {
		return err
	}
	var pending bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))
 OR EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=$2 AND state IN ('creating','updating','deleting')) OR EXISTS(SELECT 1 FROM branch_databases WHERE branch_id=$2 AND state IN ('creating','updating','deleting'))`, p.ProjectID, p.BranchID).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return renameFault(409, "branch_operation_in_progress", "Complete the original branch catalog Operation first")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'branch_catalog',$3,'update_default_database_owner','queued',$4,$5,$6)`, operation, p.ProjectID, p.BranchID, actor, request, raw)
	if err != nil {
		return err
	}
	if p.Initial {
		flags, _ := json.Marshal(p.Native.Flags)
		_, err = tx.Exec(ctx, `INSERT INTO default_database_owners(branch_id,project_id,database_name,database_oid,tenant_id,timeline_id,platform_role,platform_role_oid,initial_owner,initial_owner_oid,comment_sha256,database_flags,mode,owner_name,owner_oid,version,state,current_operation_id,basis,updated_by)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'uninitialized',$9,$10,1,'updating',$13,'admin_initial_observation',$14)`, p.BranchID, p.ProjectID, p.Name, int64(p.DatabaseOID), p.Native.Scope.TenantID, p.Native.Scope.TimelineID, p.Native.PlatformRole, int64(p.Native.PlatformRoleOID), p.PreviousOwner, int64(p.Native.PreviousOwnerOID), p.Native.CommentSHA256, flags, operation, actor)
	} else {
		var updated int64
		tag, e := tx.Exec(ctx, `UPDATE default_database_owners SET state='updating',current_operation_id=$3,updated_at=clock_timestamp() WHERE project_id=$1 AND branch_id=$2 AND version=$4 AND state='ready'`, p.ProjectID, p.BranchID, operation, p.ExpectedVersion)
		err = e
		if err == nil {
			updated = tag.RowsAffected()
			if updated != 1 {
				return renameFault(412, "version_conflict", "Original default database Owner version changed")
			}
		}
	}
	for ordinal, name := range []string{"verify_original_default_database_and_backings", "persist_all_default_database_owner_intents", "apply_original_database_owner", "verify_default_owner_and_commit"} {
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, operation, ordinal, name)
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO default_owner_credentials(operation_id,key_version,ciphertext) VALUES($1,$2,$3)`, operation, keyVersion, sealed)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, actor, key, hash, operation)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'update_default_database_owner','branch_catalog',$2,$3,'accepted')`, actor, p.BranchID, request)
	}
	return err
}
func defaultOwnerResultRecorded(ctx context.Context, db connectionDefaultReader, operation string, p defaultOwnerPayload) (bool, error) {
	var branch string
	err := db.QueryRow(ctx, `SELECT branch_id FROM default_owner_results WHERE operation_id=$1`, operation).Scan(&branch)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if branch != p.BranchID {
		return false, errors.New("original default Owner result identity rejected")
	}
	return true, nil
}
func commitDefaultOwnerResultTx(ctx context.Context, tx pgx.Tx, operation, worker string, p defaultOwnerPayload) error {
	if err := lockDefaultOwnerParents(ctx, tx, p); err != nil {
		return err
	}
	if err := assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	if done, err := defaultOwnerResultRecorded(ctx, tx, operation, p); err != nil || done {
		return err
	}
	if err := verifyDefaultOwnerMetadata(ctx, tx, p, operation, true); err != nil {
		return err
	}
	if err := verifyDefaultOwnerEndpointSet(ctx, tx, p); err != nil {
		return err
	}
	var actor, request string
	if err := tx.QueryRow(ctx, `SELECT actor_id,request_id FROM operations WHERE id=$1`, operation).Scan(&actor, &request); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE default_database_owners SET mode=$3,owner_name=$4,owner_oid=$5,owner_role_version=$6,owner_credential_ref=$7,owner_credential_uid=$8,
 version=version+1,state='ready',current_operation_id=NULL,last_operation_id=$9,updated_by=$10,updated_at=clock_timestamp()
 WHERE project_id=$1 AND branch_id=$2 AND state='updating' AND current_operation_id=$9 AND version=$11`, p.ProjectID, p.BranchID, p.Mode, p.Owner, int64(p.Native.OwnerOID), p.TargetRoleVersion, p.TargetCredentialRef, p.TargetCredentialUID, operation, actor, p.ExpectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return renameFault(412, "version_conflict", "Original default database Owner result changed")
	}
	if p.LegacyDatabaseVersion > 0 {
		tag, err = tx.Exec(ctx, `UPDATE branch_databases SET owner_name=$3,version=version+1,updated_at=now() WHERE branch_id=$1 AND name=$2 AND state='ready' AND owner_name=$4 AND version=$5`, p.BranchID, p.Name, p.Owner, p.PreviousOwner, p.LegacyDatabaseVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return renameFault(412, "default_owner_legacy_intent_changed", "Original custom-catalog default database changed")
		}
	}
	response, _ := json.Marshal(record{"database": p.Name, "database_oid": p.DatabaseOID, "mode": p.Mode, "owner": p.Owner, "owner_oid": p.Native.OwnerOID, "previous_owner": p.PreviousOwner, "previous_owner_oid": p.Native.PreviousOwnerOID, "version_token": strconv.FormatInt(p.ExpectedVersion+1, 10), "operation_id": operation})
	if _, err = tx.Exec(ctx, `INSERT INTO default_owner_results(operation_id,branch_id,response) VALUES($1,$2,$3)`, operation, p.BranchID, response); err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `UPDATE default_owner_credentials SET key_version=NULL,ciphertext=NULL,cleared_at=clock_timestamp() WHERE operation_id=$1 AND cleared_at IS NULL`, operation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("original default Owner credential escrow missing before completion")
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'update_default_database_owner','branch_catalog',$2,$3,'succeeded')`, actor, p.BranchID, request)
	return err
}
