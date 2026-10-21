package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strconv"
	"strings"
)

type catalogRenameFault struct {
	Status        int
	Code, Message string
}

func (e *catalogRenameFault) Error() string { return e.Message }
func renameFault(status int, code, message string) error {
	return &catalogRenameFault{status, code, message}
}

func (s *server) catalogRenameMutation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if accessFrom(r).Level < 2 {
		fail(w, r, 403, "forbidden", "Editor or Admin required")
		return
	}
	p := catalogRenamePayload{catalogPayload: catalogPayload{ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch")}, Kind: "database"}
	p.Name = r.PathValue("database")
	if r.PathValue("role") != "" {
		p.Kind = "role"
		p.Name = r.PathValue("role")
	}
	var body struct {
		NewName string `json:"new_name"`
	}
	if readJSON(r, &body) != nil || !validPGIdentifier(p.Name) || !validPGIdentifier(body.NewName) || body.NewName == p.Name || (p.Kind == "role" && (protectedPGRole(p.Name) || protectedPGRole(body.NewName))) || (p.Kind == "database" && (protectedPGDatabase(p.Name) || protectedPGDatabase(body.NewName))) {
		fail(w, r, 422, "invalid_catalog_rename", "Distinct valid non-system PostgreSQL identifiers required")
		return
	}
	p.NewName = body.NewName
	header := r.Header.Get("If-Match")
	if header == "" {
		fail(w, r, 428, "version_required", "Quoted original catalog If-Match version required")
		return
	}
	version, err := strconv.ParseInt(strings.Trim(header, `"`), 10, 64)
	if err != nil || version < 1 || header != `"`+strconv.FormatInt(version, 10)+`"` {
		fail(w, r, 422, "invalid_version", "Use one canonical quoted positive version")
		return
	}
	p.ExpectedVersion = version
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, record{"new_name": p.NewName, "expected_version": version})
	if s.replayCatalog(w, r, key, hash) {
		return
	}
	if err = checkDefaultOwnerReservation(r.Context(), s.db, p.ProjectID, p.BranchID); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	if err = checkCatalogForkExclusion(r.Context(), s.db, p.ProjectID, p.BranchID, p.Kind, p.Name); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	table, column := "branch_databases", "owner_name"
	if p.Kind == "role" {
		table, column = "branch_roles", "credential_ref"
	}
	var state, reference string
	var observed int64
	err = s.db.QueryRow(r.Context(), "SELECT state,version,COALESCE("+column+",'') FROM "+table+" WHERE branch_id=$1 AND name=$2", p.BranchID, p.Name).Scan(&state, &observed, &reference)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read original catalog intent")
		return
	}
	if isNoRows(err) || state != "ready" {
		fail(w, r, 409, "managed_resource_required", "Ready API-managed catalog object required")
		return
	}
	if observed != version {
		fail(w, r, 412, "version_conflict", "Catalog changed; refresh before retrying")
		return
	}
	if p.Kind == "role" {
		p.CredentialRef = reference
	} else {
		p.Owner = reference
		if protectedPGRole(p.Owner) {
			fail(w, r, 409, "managed_owner_required", "Non-system managed database owner required")
			return
		}
	}
	if err = checkCatalogRenameDependencies(r.Context(), s.db, p); err != nil {
		s.catalogRenameFailure(w, r, err)
		return
	}
	p.EndpointID, err = s.catalogWriter(r.Context(), p.ProjectID, p.BranchID)
	if err != nil {
		s.catalogRenameFailure(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
	if err != nil {
		s.catalogRenameFailure(w, r, err)
		return
	}
	defer release()
	conn, err := s.catalogConnection(ctx, p.catalogPayload)
	if err != nil {
		s.sqlFailureResponse(w, r, err)
		return
	}
	var marker string
	var superuser bool
	ownerMismatch := false
	if p.Kind == "role" {
		err = conn.QueryRow(ctx, `SELECT oid,COALESCE(shobj_description(oid,'pg_authid'),''),rolsuper FROM pg_roles WHERE rolname=$1`, p.Name).Scan(&p.ObjectOID, &marker, &superuser)
	} else {
		var owner string
		err = conn.QueryRow(ctx, `SELECT oid,COALESCE(shobj_description(oid,'pg_database'),''),pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1`, p.Name).Scan(&p.ObjectOID, &marker, &owner)
		ownerMismatch = err == nil && owner != p.Owner
	}
	conn.Close(context.Background())
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "catalog_observation_unavailable", "Could not establish original PostgreSQL identity; retain the original request")
		return
	}
	if isNoRows(err) || ownerMismatch || superuser || marker != "neon-control/branch/"+p.BranchID || !p.valid() {
		fail(w, r, 409, "catalog_identity_conflict", "Original PostgreSQL OID, owner or marker differs from managed intent")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.catalogRenameFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	var parentsReady bool
	err = tx.QueryRow(ctx, `SELECT pr.source='managed' AND pr.state='ready' AND pr.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL FROM projects pr JOIN branches b ON b.project_id=pr.id WHERE pr.id=$1 AND b.id=$2 FOR UPDATE OF pr,b`, p.ProjectID, p.BranchID).Scan(&parentsReady)
	if err != nil || !parentsReady {
		s.catalogRenameFailure(w, r, renameFault(409, "catalog_intent_changed", "Ready catalog parents required"))
		return
	}
	if err = s.captureCatalogMutationIdentity(ctx, tx, &p.catalogPayload, "rename_"+p.Kind); err != nil {
		s.catalogRenameFailure(w, r, err)
		return
	}
	if p.NativeIdentity.Object.OID != p.ObjectOID {
		fail(w, r, 409, "catalog_identity_conflict", "Rename target differs from original native binding")
		return
	}
	if err = s.captureCatalogRenameBindings(ctx, &p); err != nil {
		s.catalogRenameFailure(w, r, err)
		return
	}
	operation, err := admitCatalogRenameTx(r.Context(), tx, p, userFrom(r).ID, requestID(r), keyHash(key), hash)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayCatalog(w, r, key, hash) {
			return
		}
		s.catalogRenameFailure(w, r, err)
		return
	}
	s.acceptCatalog(w, r, operation)
}

func (s *server) catalogRenameFailure(w http.ResponseWriter, r *http.Request, err error) {
	var failure *catalogRenameFault
	if errors.As(err, &failure) {
		fail(w, r, failure.Status, failure.Code, failure.Message)
		return
	}
	fail(w, r, 503, "catalog_rename_unavailable", "Catalog rename unavailable; retain original input, key and request ID")
}

type catalogRenameReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func checkCatalogRenameDependencies(ctx context.Context, db catalogRenameReader, p catalogRenamePayload) error {
	if err := checkDefaultOwnerDependency(ctx, db, p.ProjectID, p.BranchID, p.Kind, p.Name); err != nil {
		return err
	}
	if err := checkConnectionDefaultDependency(ctx, db, p.BranchID, p.Kind, p.Name); err != nil {
		return err
	}
	table := "branch_databases"
	if p.Kind == "role" {
		table = "branch_roles"
	}
	var reserved bool
	if err := db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE branch_id=$1 AND name=$2)", p.BranchID, p.NewName).Scan(&reserved); err != nil {
		return err
	}
	if reserved {
		return renameFault(409, "catalog_name_reserved", "Target name is already present or retained by a managed catalog record")
	}
	var dependency bool
	if p.Kind == "role" {
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND role_name=$3)`, p.ProjectID, p.BranchID, p.Name).Scan(&dependency); err != nil {
			return err
		}
	} else {
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND database_name=$3)
 OR EXISTS(SELECT 1 FROM data_api_instances WHERE branch_id=$2 AND spec->>'database'=$3)
 OR EXISTS(SELECT 1 FROM managed_auth_instances WHERE branch_id=$2 AND spec->>'database'=$3)
 OR EXISTS(SELECT 1 FROM object_storage_instances WHERE branch_id=$2 AND spec->>'database'=$3)
 OR EXISTS(SELECT 1 FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND database_name=$3)`, p.ProjectID, p.BranchID, p.Name).Scan(&dependency); err != nil {
			return err
		}
	}
	if dependency {
		return renameFault(409, "catalog_dependencies_present", "Default connections or retained Backend identities reference this name; migrate those dependencies before renaming")
	}
	return nil
}

func admitCatalogRenameTx(ctx context.Context, tx pgx.Tx, p catalogRenamePayload, actor, request, key, hash string) (string, error) {
	if !p.valid() || !p.bindingsValid() {
		return "", errors.New("invalid catalog rename admission")
	}
	var orgRole, projectRole string
	err := tx.QueryRow(ctx, `SELECT m.role,COALESCE(g.role,'') FROM projects p JOIN branches b ON b.project_id=p.id JOIN organizations o ON o.id=p.org_id
 JOIN users u ON u.id=$3 AND NOT u.disabled JOIN organization_members m ON m.org_id=o.id AND m.user_id=u.id LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id
 WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active' FOR UPDATE OF p,b`, p.ProjectID, p.BranchID, actor).Scan(&orgRole, &projectRole)
	if err != nil && !isNoRows(err) {
		return "", err
	}
	if isNoRows(err) || effectivePermission(orgRole, projectRole) < 2 {
		return "", renameFault(403, "forbidden", "Current project Editor or Admin required")
	}
	var pending bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, p.ProjectID, p.BranchID).Scan(&pending)
	if err != nil {
		return "", err
	}
	if pending {
		return "", renameFault(409, "branch_operation_in_progress", "Another branch Operation is active")
	}
	if err = checkCatalogRenameDependencies(ctx, tx, p); err != nil {
		return "", err
	}
	if err = verifyCatalogRenameEndpointSet(ctx, tx, p); err != nil {
		return "", err
	}
	if err = verifyCatalogMutationBindingsTx(ctx, tx, p.catalogPayload, "rename_"+p.Kind); err != nil {
		return "", err
	}
	table, reference := "branch_databases", "owner_name"
	expected := p.Owner
	if p.Kind == "role" {
		table, reference, expected = "branch_roles", "credential_ref", p.CredentialRef
	}
	tag, err := tx.Exec(ctx, "UPDATE "+table+" SET state='updating',updated_at=now() WHERE branch_id=$1 AND name=$2 AND state='ready' AND version=$3 AND "+reference+"=$4", p.BranchID, p.Name, p.ExpectedVersion, expected)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", renameFault(412, "version_conflict", "Original catalog intent changed before admission")
	}
	op := newID("op_")
	payload, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	action := "rename_" + p.Kind
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'branch_catalog',$3,$4,'queued',$5,$6,$7)`, op, p.ProjectID, p.BranchID, action, actor, request, payload)
	for ordinal, name := range []string{"verify_original_compute_backings", "rename_postgres_original_oid", "persist_all_compute_catalogs", "publish_proxy_credentials", "verify_oid_and_commit_metadata"} {
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, op, ordinal, name)
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, actor, key, hash, op)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,$2,'branch_catalog',$3,$4,'accepted')`, actor, action, p.BranchID, request)
	}
	return op, err
}
