package control

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
	"strings"
)

// One acknowledged rename retains PostgreSQL OID, ACLs, data and the managed
// SCRAM credential. Unknown outcomes are recovered by OID and old-or-new name,
// never by adopting an arbitrary object that occupies the requested target.
type catalogRenamePayload struct {
	catalogPayload
	Kind          string            `json:"kind"`
	NewName       string            `json:"new_name"`
	ObjectOID     uint32            `json:"object_oid"`
	ConfigUIDs    map[string]string `json:"config_uids"`
	RoutesUID     string            `json:"routes_uid"`
	CredentialUID string            `json:"credential_uid,omitempty"`
}

func (p catalogRenamePayload) valid() bool {
	return p.ProjectID != "" && p.BranchID != "" && p.EndpointID != "" && (p.Kind == "role" || p.Kind == "database") && validPGIdentifier(p.Name) && validPGIdentifier(p.NewName) && p.Name != p.NewName && p.ObjectOID > 0 && p.ExpectedVersion > 0 &&
		((p.Kind == "role" && !protectedPGRole(p.Name) && !protectedPGRole(p.NewName) && p.CredentialRef != "") || (p.Kind == "database" && !protectedPGDatabase(p.Name) && !protectedPGDatabase(p.NewName) && validPGIdentifier(p.Owner) && !protectedPGRole(p.Owner)))
}

func applyCatalogRenameSQL(ctx context.Context, conn *pgx.Conn, p catalogRenamePayload, verifier string) error {
	if !p.valid() || (p.Kind == "role" && !functionsql.ValidVerifier(verifier)) {
		return errors.New("invalid managed catalog rename intent")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='30s'`, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,794210063))`, p.BranchID); err != nil {
		return err
	}
	// SQL DDL takes its own PostgreSQL object locks. The postcondition inside the
	// same transaction rejects and rolls back even an external drop/recreate race
	// that changed the object between this observation and ALTER by name.
	var current, marker, owner string
	var superuser bool
	if p.Kind == "role" {
		err = tx.QueryRow(ctx, `SELECT rolname,COALESCE(shobj_description(oid,'pg_authid'),''),rolsuper FROM pg_roles WHERE oid=$1`, p.ObjectOID).Scan(&current, &marker, &superuser)
	} else {
		err = tx.QueryRow(ctx, `SELECT datname,COALESCE(shobj_description(oid,'pg_database'),''),pg_get_userbyid(datdba) FROM pg_database WHERE oid=$1`, p.ObjectOID).Scan(&current, &marker, &owner)
	}
	if err != nil || marker != "neon-control/branch/"+p.BranchID || superuser || (current != p.Name && current != p.NewName) || (p.Kind == "database" && owner != p.Owner) {
		return errors.New("original rename OID, marker, name or owner rejected")
	}
	table, column := "pg_roles", "rolname"
	if p.Kind == "database" {
		table, column = "pg_database", "datname"
	}
	var collision bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE "+column+"=ANY($1::text[]) AND oid<>$2)", []string{p.Name, p.NewName}, p.ObjectOID).Scan(&collision); err != nil {
		return err
	}
	if collision {
		return errors.New("rename source or target occupied by a different PostgreSQL OID")
	}
	if current == p.Name {
		if _, err = tx.Exec(ctx, "ALTER "+strings.ToUpper(p.Kind)+" "+pgQuote(p.Name)+" RENAME TO "+pgQuote(p.NewName)); err != nil {
			return sqlExecutionError{"catalog_rename", err}
		}
	}
	if p.Kind == "role" {
		literal, err := conn.PgConn().EscapeString(verifier)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "ALTER ROLE "+pgQuote(p.NewName)+" PASSWORD '"+literal+"'"); err != nil {
			return sqlExecutionError{"catalog_rename", err}
		}
	}
	var oid uint32
	if err = tx.QueryRow(ctx, "SELECT oid FROM "+table+" WHERE "+column+"=$1", p.NewName).Scan(&oid); err != nil || oid != p.ObjectOID {
		return errors.New("rename postcondition rejected; transaction rolled back")
	}
	return tx.Commit(ctx)
}

// Only desired catalog entries change; a committed rename is already durable
// in PostgreSQL WAL before this step. Cold-start specs must retain the renamed
// objects and owner references. No stale name-based delete is added for the old
// name: it may legitimately be used by a later, independent object.
func catalogRenameDelta(config map[string]any, p catalogRenamePayload, verifier string) error {
	if !p.valid() {
		return errors.New("invalid rename spec intent")
	}
	cluster, ok := nested(config, "spec", "cluster").(map[string]any)
	if !ok {
		return errors.New("invalid compute catalog")
	}
	key := "roles"
	if p.Kind == "database" {
		key = "databases"
	}
	values, ok := cluster[key].([]any)
	if !ok {
		return errors.New("missing compute catalog entries")
	}
	var source, target map[string]any
	for _, value := range values {
		row, ok := value.(map[string]any)
		if !ok {
			return errors.New("invalid compute catalog entry")
		}
		if row["name"] == p.Name {
			if source != nil {
				return errors.New("duplicate rename source")
			}
			source = row
		}
		if row["name"] == p.NewName {
			if target != nil {
				return errors.New("duplicate rename target")
			}
			target = row
		}
	}
	if source != nil && target != nil {
		return errors.New("compute target name occupied")
	}
	if source == nil && target == nil {
		return errors.New("managed compute rename source absent")
	}
	row := source
	if row == nil {
		row = target
	}
	if p.Kind == "role" && row["encrypted_password"] != verifier {
		return errors.New("managed compute credential differs from original rename intent")
	}
	if p.Kind == "database" && row["owner"] != p.Owner {
		return errors.New("managed compute owner differs from original rename intent")
	}
	row["name"] = p.NewName
	if p.Kind == "role" {
		databases, ok := cluster["databases"].([]any)
		if !ok {
			return errors.New("missing compute database entries")
		}
		for _, value := range databases {
			db, ok := value.(map[string]any)
			if !ok {
				return errors.New("invalid compute database entry")
			}
			if db["owner"] == p.Name {
				db["owner"] = p.NewName
			}
		}
	}
	return nil
}

func commitCatalogRenameTx(ctx context.Context, tx pgx.Tx, p catalogRenamePayload) error {
	table := "branch_roles"
	if p.Kind == "database" {
		table = "branch_databases"
	}
	tag, err := tx.Exec(ctx, "UPDATE "+table+" SET name=$3,state='ready',version=version+1,updated_at=now() WHERE branch_id=$1 AND name=$2 AND state='updating' AND version=$4", p.BranchID, p.Name, p.NewName, p.ExpectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		if p.Kind == "role" {
			_, err = tx.Exec(ctx, `UPDATE branch_databases SET owner_name=$3,version=version+1,updated_at=now() WHERE branch_id=$1 AND owner_name=$2`, p.BranchID, p.Name, p.NewName)
		}
		return err
	}
	var originalAbsent, committed bool
	if err = tx.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM "+table+" WHERE branch_id=$1 AND name=$2),EXISTS(SELECT 1 FROM "+table+" WHERE branch_id=$1 AND name=$3 AND state='ready' AND version=$4)", p.BranchID, p.Name, p.NewName, p.ExpectedVersion+1).Scan(&originalAbsent, &committed); err != nil {
		return err
	}
	if !originalAbsent || !committed {
		return errors.New("rename metadata intent changed before result confirmation")
	}
	return nil
}
