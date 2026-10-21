package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

// A reference names one immutable metadata generation, not an object found by
// its current SQL name. A name may be reused only after a recorded deletion.
type catalogObjectRef struct {
	ID            string `json:"id"`
	IdentityID    string `json:"identity_id"`
	OID           uint32 `json:"oid"`
	Version       int64  `json:"version"`
	CredentialRef string `json:"credential_ref,omitempty"`
	CredentialUID string `json:"credential_uid,omitempty"`
	OwnerOID      uint32 `json:"owner_oid,omitempty"`
}
type catalogMutationIdentity struct {
	Scope         catalogNativeScope `json:"scope"`
	Object        catalogObjectRef   `json:"object"`
	Owner         *catalogObjectRef  `json:"owner,omitempty"`
	PreviousOwner *catalogObjectRef  `json:"previous_owner,omitempty"`
}

func catalogObjectKind(action string) string {
	if strings.Contains(action, "role") {
		return "role"
	}
	return "database"
}
func catalogIdentityRequired() error {
	return renameFault(409, "catalog_initial_observation_required", "Record the original native identity before changing this legacy object; an SQL name or marker is not identity proof")
}

func insertCatalogObjectBinding(ctx context.Context, tx pgx.Tx, project, branch, kind, name, state, source, sourceID string, scope catalogNativeScope, ref catalogObjectRef) error {
	if !scope.valid() || ref.OID == 0 || ref.Version < 1 || ref.IdentityID == "" || ref.ID == "" || !validPGIdentifier(name) {
		return errors.New("invalid catalog identity generation")
	}
	_, err := tx.Exec(ctx, `INSERT INTO catalog_object_bindings(id,identity_id,project_id,branch_id,kind,name,object_oid,intent_version,tenant_id,timeline_id,state,credential_ref,credential_uid,owner_oid,source_type,source_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, ref.ID, ref.IdentityID, project, branch, kind, name, ref.OID, ref.Version, scope.TenantID, scope.TimelineID, state, ref.CredentialRef, ref.CredentialUID, ref.OwnerOID, source, sourceID)
	return err
}

// Existing 049 observations and 054 results are usable only at their exact
// committed intent generation. Ordinary 053 results contain no original OID.
// They must never be upgraded into an invented native identity.
func loadCatalogObjectBinding(ctx context.Context, tx pgx.Tx, project, branch, kind, name string, scope catalogNativeScope) (catalogObjectRef, error) {
	var ref catalogObjectRef
	table, column := "branch_databases", "owner_name"
	if kind == "role" {
		table, column = "branch_roles", "credential_ref"
	}
	var version int64
	var state, reference string
	err := tx.QueryRow(ctx, "SELECT version,state,COALESCE("+column+",'') FROM "+table+" WHERE branch_id=$1 AND name=$2 FOR UPDATE", branch, name).Scan(&version, &state, &reference)
	if err != nil || state != "ready" {
		return ref, catalogIdentityRequired()
	}
	var actual catalogNativeScope
	var storedState string
	err = tx.QueryRow(ctx, `SELECT id,identity_id,object_oid,intent_version,credential_ref,credential_uid,owner_oid,tenant_id,timeline_id,state FROM catalog_object_bindings WHERE project_id=$1 AND branch_id=$2 AND kind=$3 AND name=$4 AND intent_version=$5`, project, branch, kind, name, version).Scan(&ref.ID, &ref.IdentityID, &ref.OID, &ref.Version, &ref.CredentialRef, &ref.CredentialUID, &ref.OwnerOID, &actual.TenantID, &actual.TimelineID, &storedState)
	if err == nil {
		if actual != scope || storedState != "ready" || kind == "role" && ref.CredentialRef != reference {
			return ref, errors.New("current catalog binding differs from intent")
		}
		return ref, nil
	}
	if !isNoRows(err) {
		return ref, err
	}
	var sourceID string
	var expectation []byte
	source := "initial_observation"
	err = tx.QueryRow(ctx, `SELECT id,object_oid,intent_version,credential_ref,credential_uid,expectation FROM catalog_identity_baselines WHERE project_id=$1 AND branch_id=$2 AND kind=$3 AND name=$4 AND intent_version=$5 AND tenant_id=$6 AND timeline_id=$7`, project, branch, kind, name, version, scope.TenantID, scope.TimelineID).Scan(&sourceID, &ref.OID, &ref.Version, &ref.CredentialRef, &ref.CredentialUID, &expectation)
	if err == nil {
		var e catalogIdentityExpectation
		if json.Unmarshal(expectation, &e) != nil || e.Scope != scope || e.OID != ref.OID || e.Name != name {
			return ref, errors.New("original baseline scope rejected")
		}
		ref.OwnerOID = e.OwnerOID
	} else if isNoRows(err) {
		var raw []byte
		var oid uint32
		err = tx.QueryRow(ctx, `SELECT r.operation_id,o.payload,(r.result->>'object_oid')::bigint FROM catalog_rename_results r JOIN operations o ON o.id=r.operation_id WHERE r.project_id=$1 AND r.branch_id=$2 AND r.kind=$3 AND r.result->>'name'=$4 AND r.result->>'version_token'=$5`, project, branch, kind, name, strconv.FormatInt(version, 10)).Scan(&sourceID, &raw, &oid)
		if err != nil {
			if isNoRows(err) {
				return ref, catalogIdentityRequired()
			}
			return ref, err
		}
		var p catalogRenamePayload
		if json.Unmarshal(raw, &p) != nil || !p.valid() || p.ProjectID != project || p.BranchID != branch || p.Kind != kind || p.NewName != name || p.ObjectOID != oid || p.ExpectedVersion+1 != version {
			return ref, errors.New("original rename identity rejected")
		}
		ref.OID, ref.Version = oid, version
		source = "rename_result"
		if kind == "role" {
			ref.CredentialRef, ref.CredentialUID = p.CredentialRef, p.CredentialUID
		} else {
			owner, e := loadCatalogObjectBinding(ctx, tx, project, branch, "role", reference, scope)
			if e != nil {
				return ref, e
			}
			ref.OwnerOID = owner.OID
		}
	} else {
		return ref, err
	}
	if kind == "role" && (ref.CredentialRef != reference || !functionResourceUID.MatchString(ref.CredentialUID)) {
		return ref, catalogIdentityRequired()
	}
	ref.ID, ref.IdentityID = newID("cob_"), newID("coi_")
	err = insertCatalogObjectBinding(ctx, tx, project, branch, kind, name, "ready", source, sourceID, scope, ref)
	return ref, err
}

// All reads here are observations. No password, grants, comments or object
// names are repaired in order to make a binding fit.
func (s *server) captureCatalogMutationIdentity(ctx context.Context, tx pgx.Tx, p *catalogPayload, action string) error {
	var scope catalogNativeScope
	if err := tx.QueryRow(ctx, `SELECT p.tenant_id,b.timeline_id FROM projects p JOIN branches b ON b.project_id=p.id WHERE p.id=$1 AND b.id=$2`, p.ProjectID, p.BranchID).Scan(&scope.TenantID, &scope.TimelineID); err != nil {
		return err
	}
	if !scope.valid() {
		return errCatalogInventoryScope
	}
	proof := &catalogMutationIdentity{Scope: scope}
	kind := catalogObjectKind(action)
	if !strings.HasPrefix(action, "create_") {
		ref, err := loadCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, kind, p.Name, scope)
		if err != nil {
			return err
		}
		proof.Object = ref
	}
	for _, v := range []struct {
		name string
		out  **catalogObjectRef
	}{{p.Owner, &proof.Owner}, {p.PreviousOwner, &proof.PreviousOwner}} {
		if v.name != "" {
			ref, err := loadCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "role", v.name, scope)
			if err != nil {
				return err
			}
			*v.out = &ref
		}
	}
	p.NativeIdentity = proof
	if err := s.verifyCatalogCredentialBindings(ctx, *p); err != nil {
		return err
	}
	conn, err := s.catalogConnection(ctx, *p)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return verifyCatalogNativeBinding(ctx, conn, *p, action, 0, false)
}
func (s *server) verifyCatalogCredentialBindings(ctx context.Context, p catalogPayload) error {
	proof := p.NativeIdentity
	if proof == nil {
		return catalogIdentityRequired()
	}
	for _, ref := range []*catalogObjectRef{&proof.Object, proof.Owner, proof.PreviousOwner} {
		if ref == nil || ref.CredentialRef == "" {
			continue
		}
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", ref.CredentialRef), nil)
		if err != nil || stringVal(nested(secret, "metadata", "labels", "neon-control/project-id")) != p.ProjectID || secret["immutable"] != true || stringVal(nested(secret, "metadata", "uid")) != ref.CredentialUID {
			return errors.New("current immutable role credential UID rejected")
		}
	}
	return nil
}

func verifyCatalogNativeBinding(ctx context.Context, db catalogRenameReader, p catalogPayload, action string, createOID uint32, after bool) error {
	proof := p.NativeIdentity
	if proof == nil || !proof.Scope.valid() {
		return catalogIdentityRequired()
	}
	var actual catalogNativeScope
	if err := db.QueryRow(ctx, `SELECT current_setting('neon.tenant_id',true),current_setting('neon.timeline_id',true)`).Scan(&actual.TenantID, &actual.TimelineID); err != nil {
		return err
	}
	if actual != proof.Scope {
		return errCatalogInventoryScope
	}
	kind := catalogObjectKind(action)
	table, column, description := "pg_database", "datname", "pg_database"
	if kind == "role" {
		table, column, description = "pg_roles", "rolname", "pg_authid"
	}
	var oid uint32
	var marker string
	err := db.QueryRow(ctx, "SELECT oid,COALESCE(shobj_description(oid,'"+description+"'),'') FROM "+table+" WHERE "+column+"=$1", p.Name).Scan(&oid, &marker)
	expected := proof.Object.OID
	if strings.HasPrefix(action, "create_") {
		expected = createOID
		if expected == 0 {
			if isNoRows(err) {
				return verifyCatalogOwnerBindings(ctx, db, p)
			}
			if err != nil {
				return err
			}
			return errors.New("CREATE target exists without original acknowledged OID; retain unknown outcome")
		}
	}
	if strings.HasPrefix(action, "delete_") && isNoRows(err) {
		var retained bool
		if err = db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE oid=$1)", expected).Scan(&retained); err != nil {
			return err
		}
		if retained {
			return errors.New("original deletion object was renamed; no name-based deletion permitted")
		}
		return verifyCatalogOwnerBindings(ctx, db, p)
	}
	if err != nil || oid != expected || marker != "neon-control/branch/"+p.BranchID && !(strings.HasPrefix(action, "create_") && marker == "" && !after) {
		return renameFault(409, "catalog_identity_conflict", "Original PostgreSQL OID, name or marker changed")
	}
	if kind == "role" {
		var super bool
		if err = db.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE oid=$1`, oid).Scan(&super); err != nil || super {
			return errors.New("system role cannot be mutated")
		}
	}
	if kind == "database" {
		var owner uint32
		if err = db.QueryRow(ctx, `SELECT datdba FROM pg_database WHERE oid=$1`, oid).Scan(&owner); err != nil {
			return err
		}
		expectedOwner := proof.Object.OwnerOID
		if strings.HasPrefix(action, "create_") && proof.Owner != nil {
			expectedOwner = proof.Owner.OID
		}
		if action == "update_database_owner" && proof.Owner != nil && owner == proof.Owner.OID {
			expectedOwner = owner
		}
		if owner != expectedOwner {
			return errors.New("original database owner OID changed")
		}
	}
	return verifyCatalogOwnerBindings(ctx, db, p)
}
func verifyCatalogOwnerBindings(ctx context.Context, db catalogRenameReader, p catalogPayload) error {
	for _, v := range []struct {
		name string
		ref  *catalogObjectRef
	}{{p.Owner, p.NativeIdentity.Owner}, {p.PreviousOwner, p.NativeIdentity.PreviousOwner}} {
		if v.name == "" {
			continue
		}
		if v.ref == nil || v.ref.OID == 0 {
			return catalogIdentityRequired()
		}
		var oid uint32
		var marker string
		var super bool
		if err := db.QueryRow(ctx, `SELECT oid,COALESCE(shobj_description(oid,'pg_authid'),''),rolsuper FROM pg_roles WHERE rolname=$1`, v.name).Scan(&oid, &marker, &super); err != nil || oid != v.ref.OID || super || marker != "neon-control/branch/"+p.BranchID {
			return renameFault(409, "catalog_identity_conflict", "Original database owner role OID changed")
		}
	}
	return nil
}

func verifyCatalogObjectRef(ctx context.Context, tx pgx.Tx, p catalogPayload, kind, name string, ref catalogObjectRef) error {
	var actual catalogObjectRef
	var scope catalogNativeScope
	var state string
	err := tx.QueryRow(ctx, `SELECT id,identity_id,object_oid,intent_version,credential_ref,credential_uid,owner_oid,tenant_id,timeline_id,state FROM catalog_object_bindings WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND kind=$4 AND name=$5`, ref.ID, p.ProjectID, p.BranchID, kind, name).Scan(&actual.ID, &actual.IdentityID, &actual.OID, &actual.Version, &actual.CredentialRef, &actual.CredentialUID, &actual.OwnerOID, &scope.TenantID, &scope.TimelineID, &state)
	if err != nil || actual != ref || state != "ready" || scope != p.NativeIdentity.Scope {
		return errors.New("original catalog generation rejected")
	}
	return nil
}
func verifyCatalogMutationBindingsTx(ctx context.Context, tx pgx.Tx, p catalogPayload, action string) error {
	if p.NativeIdentity == nil {
		return nil
	} // Only old immutable result recovery uses the legacy payload.
	proof := p.NativeIdentity
	var scope catalogNativeScope
	err := tx.QueryRow(ctx, `SELECT p.tenant_id,b.timeline_id FROM projects p JOIN branches b ON b.project_id=p.id WHERE p.id=$1 AND b.id=$2`, p.ProjectID, p.BranchID).Scan(&scope.TenantID, &scope.TimelineID)
	if err != nil || !scope.valid() || scope != proof.Scope {
		return errCatalogInventoryScope
	}
	if !strings.HasPrefix(action, "create_") {
		if proof.Object.Version != p.ExpectedVersion {
			return errors.New("catalog binding version changed")
		}
		if err = verifyCatalogObjectRef(ctx, tx, p, catalogObjectKind(action), p.Name, proof.Object); err != nil {
			return err
		}
	}
	for _, v := range []struct {
		name string
		ref  *catalogObjectRef
	}{{p.Owner, proof.Owner}, {p.PreviousOwner, proof.PreviousOwner}} {
		if v.name == "" {
			continue
		}
		if v.ref == nil {
			return catalogIdentityRequired()
		}
		if err = verifyCatalogObjectRef(ctx, tx, p, "role", v.name, *v.ref); err != nil {
			return err
		}
		var version int64
		var state, cref string
		if err = tx.QueryRow(ctx, `SELECT version,state,COALESCE(credential_ref,'') FROM branch_roles WHERE branch_id=$1 AND name=$2`, p.BranchID, v.name).Scan(&version, &state, &cref); err != nil || version != v.ref.Version || state != "ready" || cref != v.ref.CredentialRef {
			return errors.New("owner credential generation changed")
		}
	}
	return nil
}

func (s *server) catalogCreateObservation(ctx context.Context, p catalogPayload, action string) (uint32, error) {
	var oid uint32
	var digest, project, branch string
	err := s.db.QueryRow(ctx, `SELECT object_oid,payload_sha256,project_id,branch_id FROM catalog_create_observations WHERE operation_id=$1`, p.operationID).Scan(&oid, &digest, &project, &branch)
	if isNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if digest != catalogMutationDigest(action, p) || project != p.ProjectID || branch != p.BranchID {
		return 0, errors.New("original CREATE observation scope rejected")
	}
	return oid, nil
}
func (s *server) recordCatalogCreateObservation(ctx context.Context, p catalogPayload, action string, oid uint32) error {
	if p.NativeIdentity == nil || !strings.HasPrefix(action, "create_") || oid == 0 {
		return errors.New("original CREATE acknowledgment required")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = verifyCatalogMutationOperationTx(ctx, tx, p.operationID, p.workerID, action, p); err != nil {
		return err
	}
	if err = lockCatalogMutationIntentTx(ctx, tx, p.operationID, action, p); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO catalog_create_observations(operation_id,project_id,branch_id,payload_sha256,object_oid) VALUES($1,$2,$3,$4,$5) ON CONFLICT(operation_id) DO NOTHING`, p.operationID, p.ProjectID, p.BranchID, catalogMutationDigest(action, p), oid)
	if err != nil {
		return err
	}
	var observed uint32
	var digest string
	if err = tx.QueryRow(ctx, `SELECT object_oid,payload_sha256 FROM catalog_create_observations WHERE operation_id=$1`, p.operationID).Scan(&observed, &digest); err != nil {
		return err
	}
	if observed != oid || digest != catalogMutationDigest(action, p) {
		return errors.New("original CREATE observation cannot be overwritten")
	}
	return tx.Commit(ctx)
}

func commitCatalogObjectBindingTx(ctx context.Context, tx pgx.Tx, operation, action string, p catalogPayload, newName string) error {
	if p.NativeIdentity == nil {
		return nil
	}
	if err := verifyCatalogMutationBindingsTx(ctx, tx, p, action); err != nil {
		return err
	}
	ref := p.NativeIdentity.Object
	source := "mutation_result"
	state := "ready"
	if strings.HasPrefix(action, "create_") {
		var digest string
		if err := tx.QueryRow(ctx, `SELECT object_oid,payload_sha256 FROM catalog_create_observations WHERE operation_id=$1 AND project_id=$2 AND branch_id=$3`, operation, p.ProjectID, p.BranchID).Scan(&ref.OID, &digest); err != nil {
			return err
		}
		if digest != catalogMutationDigest(action, p) {
			return errors.New("CREATE result requires original OID observation")
		}
		ref.IdentityID = newID("coi_")
		source = "create_result"
	}
	if strings.HasPrefix(action, "delete_") {
		state = "deleted"
	}
	if strings.HasPrefix(action, "rename_") {
		source = "rename_result"
	}
	ref.ID = newID("cob_")
	ref.Version = p.ExpectedVersion + 1
	if catalogObjectKind(action) == "role" {
		if action != "delete_role" && p.CredentialRef != "" {
			ref.CredentialRef, ref.CredentialUID = p.CredentialRef, p.MutationCredentialUID
		}
	} else if p.NativeIdentity.Owner != nil {
		ref.OwnerOID = p.NativeIdentity.Owner.OID
	}
	if newName == "" {
		newName = p.Name
	}
	return insertCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, catalogObjectKind(action), newName, state, source, operation, p.NativeIdentity.Scope, ref)
}

func commitCatalogRenameBindingTx(ctx context.Context, tx pgx.Tx, operation string, p catalogRenamePayload) error {
	if p.NativeIdentity == nil {
		return nil
	}
	if p.ObjectOID != p.NativeIdentity.Object.OID {
		return errors.New("rename original binding OID rejected")
	}
	payload := p.catalogPayload
	if p.Kind == "role" {
		payload.MutationCredentialUID = p.CredentialUID
	}
	if err := commitCatalogObjectBindingTx(ctx, tx, operation, "rename_"+p.Kind, payload, p.NewName); err != nil {
		return err
	}
	if p.Kind != "role" {
		return nil
	}
	// A role rename advances dependent database metadata versions in the same
	// transaction. Carry only already-bound identities; never invent a DB OID.
	rows, err := tx.Query(ctx, `SELECT c.id,c.identity_id,c.name,c.object_oid,c.intent_version,c.owner_oid,c.state FROM catalog_object_bindings c JOIN branch_databases d ON d.branch_id=c.branch_id AND d.name=c.name AND d.version=c.intent_version WHERE c.project_id=$1 AND c.branch_id=$2 AND c.kind='database' AND d.owner_name=$3`, p.ProjectID, p.BranchID, p.Name)
	if err != nil {
		return err
	}
	type follower struct {
		ref         catalogObjectRef
		name, state string
	}
	var values []follower
	for rows.Next() {
		var v follower
		if err = rows.Scan(&v.ref.ID, &v.ref.IdentityID, &v.name, &v.ref.OID, &v.ref.Version, &v.ref.OwnerOID, &v.state); err != nil {
			break
		}
		values = append(values, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range values {
		if v.ref.OwnerOID != p.ObjectOID {
			return errors.New("bound database follower owner differs from renamed role OID")
		}
		v.ref.ID = newID("cob_")
		v.ref.Version++
		if err = insertCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "database", v.name, v.state, "rename_follower", operation, p.NativeIdentity.Scope, v.ref); err != nil {
			return err
		}
	}
	return nil
}

func applyBoundRolePasswordSQL(ctx context.Context, conn *pgx.Conn, p catalogPayload, action, verifier string, createOID uint32) error {
	if p.NativeIdentity == nil || (action != "create_role" && action != "rotate_role_password") || !functionsql.ValidVerifier(verifier) {
		return errors.New("bound original role password action required")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = verifyCatalogNativeBinding(ctx, tx, p, action, createOID, false); err != nil {
		return err
	}
	escaped, err := conn.PgConn().EscapeString(verifier)
	if err != nil {
		return err
	}
	// Password rotation preserves all unrelated native flags, including NOLOGIN.
	if _, err = tx.Exec(ctx, "ALTER ROLE "+pgQuote(p.Name)+" PASSWORD '"+escaped+"'"); err != nil {
		return sqlExecutionError{"query", err}
	}
	if action == "create_role" {
		if _, err = tx.Exec(ctx, "COMMENT ON ROLE "+pgQuote(p.Name)+" IS "+pgLiteral("neon-control/branch/"+p.BranchID)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "GRANT "+pgQuote(p.Name)+" TO control_probe WITH INHERIT TRUE, SET TRUE"); err != nil {
			return err
		}
	}
	if err = verifyCatalogNativeBinding(ctx, tx, p, action, createOID, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func applyBoundRoleDeletionSQL(ctx context.Context, conn *pgx.Conn, p catalogPayload) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = verifyCatalogNativeBinding(ctx, tx, p, "delete_role", 0, false); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DROP ROLE IF EXISTS "+pgQuote(p.Name)); err != nil {
		return sqlExecutionError{"query", err}
	}
	if err = verifyCatalogNativeBinding(ctx, tx, p, "delete_role", 0, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
