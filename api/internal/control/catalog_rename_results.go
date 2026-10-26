package control

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func catalogRenameResultDigest(p catalogRenamePayload) string {
	raw, _ := json.Marshal(p)
	return catalogMarkerDigest(string(raw))
}

func verifyCatalogRenameOperationTx(ctx context.Context, tx pgx.Tx, operation, worker string, p catalogRenamePayload) error {
	if !p.valid() || !p.bindingsValid() {
		return errors.New("original catalog rename identity required")
	}
	if err := assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1 AND project_id=$2 AND resource_type='branch_catalog' AND resource_id=$3 AND action=$4`, operation, p.ProjectID, p.BranchID, "rename_"+p.Kind).Scan(&raw)
	var original catalogRenamePayload
	if err != nil || json.Unmarshal(raw, &original) != nil || catalogRenameResultDigest(original) != catalogRenameResultDigest(p) {
		return errors.New("original catalog rename payload rejected")
	}
	return nil
}
func catalogRenameCommittedTx(ctx context.Context, tx pgx.Tx, operation string, p catalogRenamePayload) (bool, error) {
	var project, branch, kind, digest string
	err := tx.QueryRow(ctx, `SELECT project_id,branch_id,kind,payload_sha256 FROM catalog_rename_results WHERE operation_id=$1`, operation).Scan(&project, &branch, &kind, &digest)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if project != p.ProjectID || branch != p.BranchID || kind != p.Kind || digest != catalogRenameResultDigest(p) {
		return false, errors.New("original committed rename result scope rejected")
	}
	return true, nil
}
func lockCatalogRenameResultIntentTx(ctx context.Context, tx pgx.Tx, operation string, p catalogRenamePayload) error {
	var orgRole, projectRole string
	err := tx.QueryRow(ctx, `SELECT m.role,COALESCE(g.role,'') FROM projects pr JOIN branches b ON b.project_id=pr.id JOIN organizations org ON org.id=pr.org_id
 JOIN endpoints e ON e.project_id=pr.id AND e.branch_id=b.id JOIN operations op ON op.id=$4 JOIN users u ON u.id=op.actor_id AND NOT u.disabled
 JOIN organization_members m ON m.org_id=org.id AND m.user_id=u.id LEFT JOIN project_grants g ON g.project_id=pr.id AND g.user_id=u.id
 WHERE pr.id=$1 AND b.id=$2 AND e.id=$3 AND pr.state='ready' AND pr.source='managed' AND pr.deleted_at IS NULL AND org.state='active' AND b.state='ready' AND b.deleted_at IS NULL AND e.deleted_at IS NULL AND e.state='active' AND e.endpoint_type='read_write' FOR UPDATE OF pr,b,e`, p.ProjectID, p.BranchID, p.EndpointID, operation).Scan(&orgRole, &projectRole)
	if err != nil {
		return err
	}
	required := 2
	if p.Backend != nil {
		required = 3
	}
	if effectivePermission(orgRole, projectRole) < required {
		return errors.New("current original rename actor permission rejected")
	}
	var competing bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND id<>$3 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, p.ProjectID, p.BranchID, operation).Scan(&competing)
	if err != nil {
		return err
	}
	if competing {
		return errors.New("another branch operation is active before rename commit")
	}
	if err = checkCatalogRenameDependencies(ctx, tx, p); err != nil {
		return err
	}
	if err = verifyCatalogRenameEndpointSet(ctx, tx, p); err != nil {
		return err
	}
	table, reference, expected := "branch_databases", "owner_name", p.Owner
	if p.Kind == "role" {
		table, reference, expected = "branch_roles", "credential_ref", p.CredentialRef
	}
	var state, currentReference string
	var version int64
	err = tx.QueryRow(ctx, "SELECT state,version,"+reference+" FROM "+table+" WHERE branch_id=$1 AND name=$2 FOR UPDATE", p.BranchID, p.Name).Scan(&state, &version, &currentReference)
	if err != nil {
		return err
	}
	if state != "updating" || version != p.ExpectedVersion || currentReference != expected {
		return errors.New("original catalog rename intent generation changed; do not infer a legacy result")
	}
	return verifyCatalogMutationBindingsTx(ctx, tx, p.catalogPayload, "rename_"+p.Kind)
}
func (s *server) prepareCatalogRenameResult(ctx context.Context, operation, worker string, p catalogRenamePayload) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err = verifyCatalogRenameOperationTx(ctx, tx, operation, worker, p); err != nil {
		return false, err
	}
	committed, err := catalogRenameCommittedTx(ctx, tx, operation, p)
	if err != nil {
		return false, err
	}
	if committed {
		tag, e := tx.Exec(ctx, `UPDATE operation_steps SET state='succeeded',detail='Recovered original committed rename result',updated_at=now() WHERE operation_id=$1 AND ordinal BETWEEN 0 AND 4`, operation)
		if e != nil {
			return false, e
		}
		if tag.RowsAffected() != 5 {
			return false, errors.New("original catalog rename steps rejected")
		}
	} else if err = lockCatalogRenameResultIntentTx(ctx, tx, operation, p); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return committed, nil
}
func commitCatalogRenameResultTx(ctx context.Context, tx pgx.Tx, operation, worker string, p catalogRenamePayload, backings ...catalogBackendRenameBackings) error {
	if err := verifyCatalogRenameOperationTx(ctx, tx, operation, worker, p); err != nil {
		return err
	}
	committed, err := catalogRenameCommittedTx(ctx, tx, operation, p)
	if err != nil || committed {
		return err
	}
	if err = lockCatalogRenameResultIntentTx(ctx, tx, operation, p); err != nil {
		return err
	}
	if p.Backend != nil {
		if len(backings) != 1 {
			return errors.New("original migrated Backend backings required")
		}
		if err = commitCatalogBackendRenameTx(ctx, tx, operation, p, backings[0]); err != nil {
			return err
		}
	}
	if err = commitCatalogRenameBindingTx(ctx, tx, operation, p); err != nil {
		return err
	}
	if err = commitCatalogRenameTx(ctx, tx, p); err != nil {
		return err
	}
	raw, _ := json.Marshal(record{"name": p.NewName, "previous_name": p.Name, "kind": p.Kind, "object_oid": p.ObjectOID, "state": "ready", "version_token": strconv.FormatInt(p.ExpectedVersion+1, 10)})
	_, err = tx.Exec(ctx, `INSERT INTO catalog_rename_results(operation_id,project_id,branch_id,kind,payload_sha256,result) VALUES($1,$2,$3,$4,$5,$6)`, operation, p.ProjectID, p.BranchID, p.Kind, catalogRenameResultDigest(p), raw)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) SELECT actor_id,action,'branch_catalog',resource_id,request_id,'succeeded' FROM operations WHERE id=$1`, operation)
	return err
}
