// Package functionsql owns the database permission boundary for Functions.
// It is separate from the guest package: the customer VM never needs an
// administrative SQL client, a provisioning credential, or the Proxy registry.
package functionsql

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var identityPattern = regexp.MustCompile(`^fnc_[a-f0-9]{16}$`)
var projectPattern = regexp.MustCompile(`^prj_[a-f0-9]{16}$`)
var branchPattern = regexp.MustCompile(`^br_[a-f0-9]{16}$`)
var identifierPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)
var verifierPattern = regexp.MustCompile(`^SCRAM-SHA-256\$4096:[A-Za-z0-9+/]+=*\$[A-Za-z0-9+/]+=*:[A-Za-z0-9+/]+=*$`)

// Scope is immutable control metadata. The caller must authenticate project
// access, lock the branch intent and connect to its recorded Writer selector.
// This package neither accepts customer DSNs nor mutates a database lifecycle.
type Scope struct {
	FunctionID string
	ProjectID  string
	BranchID   string
	Database   string
	Schema     string
}

func (s Scope) Validate() error {
	if !identityPattern.MatchString(s.FunctionID) || !projectPattern.MatchString(s.ProjectID) || !branchPattern.MatchString(s.BranchID) || !identifierPattern.MatchString(s.Database) || !identifierPattern.MatchString(s.Schema) || strings.HasPrefix(strings.ToLower(s.Schema), "pg_") || strings.HasPrefix(strings.ToLower(s.Schema), "neon_") || strings.HasPrefix(strings.ToLower(s.Schema), "control_") || s.Schema == "information_schema" {
		return errors.New("invalid immutable Functions SQL scope")
	}
	return nil
}
func (s Scope) Role() string { return "fn_" + strings.TrimPrefix(s.FunctionID, "fnc_") }
func (s Scope) marker() string {
	return "neon-control:function:" + s.ProjectID + ":" + s.BranchID + ":" + s.FunctionID
}
func quote(value string) string   { return pgx.Identifier{value}.Sanitize() }
func literal(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

// ProvisionTx grants DML on existing ordinary tables and sequences in exactly
// one explicitly selected schema. It does not grant ownership, CREATE, role
// membership, functions/views, BYPASSRLS or database administration. New tables
// require a new owner-authorized reconciliation; no blanket default privileges
// are installed on behalf of customer owners. All failures roll back with the
// caller transaction, including a role unexpectedly present on another branch.
func ProvisionTx(ctx context.Context, tx pgx.Tx, s Scope, verifier string) error {
	if s.Validate() != nil || !verifierPattern.MatchString(verifier) {
		return errors.New("Functions SQL scope or SCRAM verifier rejected")
	}
	var database string
	if err := tx.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil || database != s.Database {
		return errors.New("Functions SQL database identity mismatch")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,922743))`, s.FunctionID); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, s.Schema).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("Functions target schema must already exist")
	}
	role := s.Role()
	var marker string
	err := tx.QueryRow(ctx, `SELECT COALESCE(shobj_description(oid,'pg_authid'),'') FROM pg_roles WHERE rolname=$1`, role).Scan(&marker)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && marker != s.marker() {
		return errors.New("Functions SQL role ownership conflict")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, "CREATE ROLE "+quote(role)+" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "COMMENT ON ROLE "+quote(role)+" IS "+literal(s.marker())); err != nil {
			return err
		}
	}
	// Test flags before ALTER: never silently repair or adopt an elevated role.
	if err = verifyRole(ctx, tx, s); err != nil {
		return err
	}
	for _, statement := range []string{
		"GRANT CONNECT ON DATABASE " + quote(s.Database) + " TO " + quote(role),
		"GRANT USAGE ON SCHEMA " + quote(s.Schema) + " TO " + quote(role),
		"ALTER ROLE " + quote(role) + " LOGIN PASSWORD " + literal(verifier) + " CONNECTION LIMIT 4",
		"ALTER ROLE " + quote(role) + " SET search_path=" + quote(s.Schema) + ",pg_catalog",
		"ALTER ROLE " + quote(role) + " SET idle_session_timeout='5s'",
		"ALTER ROLE " + quote(role) + " SET statement_timeout='30s'",
	} {
		if _, err = tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT c.relname,c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p','S') ORDER BY c.relname`, s.Schema)
	if err != nil {
		return err
	}
	type object struct{ name, kind string }
	var objects []object
	for rows.Next() {
		var o object
		if err = rows.Scan(&o.name, &o.kind); err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, o := range objects {
		statement := "GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE " + quote(s.Schema) + "." + quote(o.name) + " TO " + quote(role)
		if o.kind == "S" {
			statement = "GRANT USAGE,SELECT ON SEQUENCE " + quote(s.Schema) + "." + quote(o.name) + " TO " + quote(role)
		}
		if _, err = tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return verifyRole(ctx, tx, s)
}

func verifyRole(ctx context.Context, tx pgx.Tx, s Scope) error {
	var safe bool
	err := tx.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolbypassrls AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolinherit
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=pg_roles.oid OR roleid=pg_roles.oid)
 AND NOT EXISTS(SELECT 1 FROM pg_namespace n WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' AND has_schema_privilege(pg_roles.oid,n.oid,'CREATE'))
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' AND p.prosecdef AND has_function_privilege(pg_roles.oid,p.oid,'EXECUTE'))
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' AND c.relkind IN ('r','p','v','m','f') AND
 (c.relowner=pg_roles.oid OR (n.nspname<>$2 AND (has_table_privilege(pg_roles.oid,c.oid,'SELECT') OR has_table_privilege(pg_roles.oid,c.oid,'INSERT') OR has_table_privilege(pg_roles.oid,c.oid,'UPDATE') OR has_table_privilege(pg_roles.oid,c.oid,'DELETE')))))
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' AND n.nspname<>$2 AND c.relkind='S' AND (has_sequence_privilege(pg_roles.oid,c.oid,'USAGE') OR has_sequence_privilege(pg_roles.oid,c.oid,'SELECT') OR has_sequence_privilege(pg_roles.oid,c.oid,'UPDATE')))
 AND NOT has_database_privilege(pg_roles.oid,current_database(),'CREATE') AND NOT has_database_privilege(pg_roles.oid,current_database(),'TEMP')
 FROM pg_roles WHERE rolname=$1`, s.Role(), s.Schema).Scan(&safe)
	if err != nil {
		return err
	}
	if !safe {
		return errors.New("Functions SQL role has inherited or unexpected privileges; database owner must remove broad PUBLIC grants")
	}
	return nil
}
