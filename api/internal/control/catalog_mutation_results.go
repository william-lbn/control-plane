package control

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func catalogMutationAction(action string) bool {
	switch action {
	case "create_role", "rotate_role_password", "delete_role", "create_database", "update_database_owner", "delete_database":
		return true
	}
	return false
}
func catalogMutationDigest(action string, p catalogPayload) string {
	raw, _ := json.Marshal(struct {
		Action  string         `json:"action"`
		Payload catalogPayload `json:"payload"`
	}{action, p})
	return catalogMarkerDigest(string(raw))
}

// The original Operation is authoritative, even when its resource subsequently
// changes generation or its Writer retires. Recovery never reads current SQL.
func verifyCatalogMutationOperationTx(ctx context.Context, tx pgx.Tx, operation, worker, action string, p catalogPayload) error {
	if !catalogMutationAction(action) || !functionWriterEndpointID.MatchString(p.EndpointID) || !validPGIdentifier(p.Name) || p.ExpectedVersion < 1 {
		return errors.New("original catalog mutation version and scope required; preserve legacy unknown outcome")
	}
	if err := assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1 AND project_id=$2 AND resource_type='branch_catalog' AND resource_id=$3 AND action=$4`, operation, p.ProjectID, p.BranchID, action).Scan(&raw)
	var original catalogPayload
	if err != nil || json.Unmarshal(raw, &original) != nil || catalogMutationDigest(action, original) != catalogMutationDigest(action, p) {
		return errors.New("original catalog mutation payload rejected")
	}
	return nil
}

func catalogMutationCommittedTx(ctx context.Context, tx pgx.Tx, operation, action string, p catalogPayload) (bool, error) {
	var project, branch, storedAction, digest string
	err := tx.QueryRow(ctx, `SELECT project_id,branch_id,action,payload_sha256 FROM catalog_mutation_results WHERE operation_id=$1`, operation).Scan(&project, &branch, &storedAction, &digest)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if project != p.ProjectID || branch != p.BranchID || storedAction != action || digest != catalogMutationDigest(action, p) {
		return false, errors.New("original committed catalog result scope rejected")
	}
	return true, nil
}

func lockCatalogMutationIntentTx(ctx context.Context, tx pgx.Tx, operation, action string, p catalogPayload) error {
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT pr.state='ready' AND pr.source='managed' AND pr.deleted_at IS NULL AND org.state='active' AND b.state='ready' AND b.deleted_at IS NULL AND e.state='active' AND e.deleted_at IS NULL AND e.endpoint_type='read_write'
 FROM projects pr JOIN organizations org ON org.id=pr.org_id JOIN branches b ON b.project_id=pr.id JOIN endpoints e ON e.branch_id=b.id AND e.project_id=pr.id WHERE pr.id=$1 AND b.id=$2 AND e.id=$3 FOR UPDATE OF pr,b,e`, p.ProjectID, p.BranchID, p.EndpointID).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return errors.New("original catalog mutation parent or Writer changed")
	}
	var competing bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND id<>$3 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, p.ProjectID, p.BranchID, operation).Scan(&competing)
	if err != nil {
		return err
	}
	if competing {
		return errors.New("another original branch operation is active")
	}
	kind, table, expectedState := "database", "branch_databases", "creating"
	if strings.Contains(action, "role") {
		kind, table = "role", "branch_roles"
	}
	if strings.HasPrefix(action, "delete_") {
		expectedState = "deleting"
	} else if action == "rotate_role_password" || action == "update_database_owner" {
		expectedState = "updating"
	}
	if err = checkCatalogForkExclusion(ctx, tx, p.ProjectID, p.BranchID, kind, p.Name); err != nil {
		return err
	}
	if p.Owner != "" {
		if err = checkCatalogForkExclusion(ctx, tx, p.ProjectID, p.BranchID, "role", p.Owner); err != nil {
			return err
		}
	}
	var state string
	var version int64
	err = tx.QueryRow(ctx, "SELECT state,version FROM "+table+" WHERE branch_id=$1 AND name=$2 FOR UPDATE", p.BranchID, p.Name).Scan(&state, &version)
	if err != nil {
		return err
	}
	if state != expectedState || version != p.ExpectedVersion {
		return errors.New("original catalog mutation intent generation changed")
	}
	if kind == "role" {
		var ref string
		err = tx.QueryRow(ctx, `SELECT COALESCE(credential_ref,'') FROM branch_roles WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name).Scan(&ref)
		expected := p.PreviousCredentialRef
		if action == "create_role" {
			expected = p.CredentialRef
		}
		if err != nil {
			return err
		}
		if ref != expected {
			return errors.New("original catalog mutation credential generation changed")
		}
	} else {
		var owner string
		err = tx.QueryRow(ctx, `SELECT owner_name FROM branch_databases WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name).Scan(&owner)
		expected := p.Owner
		if action == "update_database_owner" {
			expected = p.PreviousOwner
		}
		if err != nil {
			return err
		}
		if owner != expected {
			return errors.New("original catalog mutation owner generation changed")
		}
	}
	return verifyCatalogMutationBindingsTx(ctx, tx, p, action)
}

func (s *server) prepareCatalogMutation(ctx context.Context, operation, worker, action string, p catalogPayload) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err = verifyCatalogMutationOperationTx(ctx, tx, operation, worker, action, p); err != nil {
		return false, err
	}
	committed, err := catalogMutationCommittedTx(ctx, tx, operation, action, p)
	if err != nil {
		return false, err
	}
	if committed {
		// A result proves all four external stages completed. Only settle the same
		// original steps; do not republish old SCRAM/Owner after a later change.
		tag, e := tx.Exec(ctx, `UPDATE operation_steps SET state='succeeded',detail='Recovered original committed catalog result',updated_at=now() WHERE operation_id=$1 AND ordinal BETWEEN 0 AND 3`, operation)
		if e != nil {
			return false, e
		}
		if tag.RowsAffected() != 4 {
			return false, errors.New("original catalog mutation steps rejected")
		}
	} else if err = lockCatalogMutationIntentTx(ctx, tx, operation, action, p); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return committed, nil
}

func commitCatalogMutationResultTx(ctx context.Context, tx pgx.Tx, operation, worker, action string, p catalogPayload) error {
	if err := verifyCatalogMutationOperationTx(ctx, tx, operation, worker, action, p); err != nil {
		return err
	}
	committed, err := catalogMutationCommittedTx(ctx, tx, operation, action, p)
	if err != nil || committed {
		return err
	}
	if err = lockCatalogMutationIntentTx(ctx, tx, operation, action, p); err != nil {
		return err
	}
	state, kind := "ready", "database"
	if strings.HasPrefix(action, "delete_") {
		state = "deleted"
	}
	var version int64
	if strings.Contains(action, "role") {
		kind = "role"
		err = tx.QueryRow(ctx, `UPDATE branch_roles SET state=$3,credential_ref=NULLIF($4,''),version=version+1,updated_at=now() WHERE branch_id=$1 AND name=$2 AND version=$5 RETURNING version`, p.BranchID, p.Name, state, p.CredentialRef, p.ExpectedVersion).Scan(&version)
	} else {
		err = tx.QueryRow(ctx, `UPDATE branch_databases SET state=$3,owner_name=$4,version=version+1,updated_at=now() WHERE branch_id=$1 AND name=$2 AND version=$5 RETURNING version`, p.BranchID, p.Name, state, p.Owner, p.ExpectedVersion).Scan(&version)
	}
	if err != nil {
		return err
	}
	if version != p.ExpectedVersion+1 {
		return errors.New("original catalog result version rejected")
	}
	if err = commitCatalogObjectBindingTx(ctx, tx, operation, action, p, ""); err != nil {
		return err
	}
	response := record{"name": p.Name, "kind": kind, "state": state, "version_token": strconv.FormatInt(version, 10)}
	if kind == "database" {
		response["owner"] = p.Owner
	}
	raw, _ := json.Marshal(response)
	_, err = tx.Exec(ctx, `INSERT INTO catalog_mutation_results(operation_id,project_id,branch_id,action,payload_sha256,result) VALUES($1,$2,$3,$4,$5,$6)`, operation, p.ProjectID, p.BranchID, action, catalogMutationDigest(action, p), raw)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) SELECT actor_id,action,'branch_catalog',resource_id,request_id,'succeeded' FROM operations WHERE id=$1`, operation)
	return err
}
