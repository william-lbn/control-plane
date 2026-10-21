package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Transfer only an API-owned non-system database between ready API-managed
// roles on the same branch. Unknown external DDL is never silently adopted.
func (s *server) databaseOwnerMutation(w http.ResponseWriter, r *http.Request) {
	project, branch, name := r.PathValue("project"), r.PathValue("branch"), r.PathValue("database")
	var body struct {
		Owner string `json:"owner"`
	}
	if readJSON(r, &body) != nil || !validPGIdentifier(name) || protectedPGDatabase(name) || !validPGIdentifier(body.Owner) || protectedPGRole(body.Owner) {
		fail(w, r, 422, "invalid_database_owner", "Non-system database and managed owner required")
		return
	}
	version, err := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	if err != nil || version < 1 {
		fail(w, r, 428, "version_required", "Database numeric If-Match version required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, record{"owner": body.Owner, "expected_version": version})
	if s.replayCatalog(w, r, key, hash) {
		return
	}
	if err = checkDefaultOwnerDependency(r.Context(), s.db, project, branch, "database", name); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	if err = checkCatalogForkExclusion(r.Context(), s.db, project, branch, "role", body.Owner); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	endpoint, err := s.catalogWriter(r.Context(), project, branch)
	if isNoRows(err) {
		fail(w, r, 404, "writer_not_found", "Ready branch writer required")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect branch writer")
		return
	}
	// Read metadata before touching SQL, so stale and unsupported requests do not
	// wake a Compute merely to discover that admission is impossible.
	var previous, state string
	var actualVersion int64
	err = s.db.QueryRow(r.Context(), `SELECT owner_name,state,version FROM branch_databases WHERE branch_id=$1 AND name=$2`, branch, name).Scan(&previous, &state, &actualVersion)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect database intent")
		return
	}
	if isNoRows(err) || state != "ready" || protectedPGRole(previous) {
		fail(w, r, 409, "managed_resource_required", "Ready API-owned database with a non-system owner required")
		return
	}
	if actualVersion != version {
		fail(w, r, 412, "version_conflict", "Database changed; refresh before retrying")
		return
	}
	if previous == body.Owner {
		fail(w, r, 422, "owner_unchanged", "Choose a different managed owner")
		return
	}
	var targetReady bool
	err = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=$1 AND name=$2 AND state='ready')`, branch, body.Owner).Scan(&targetReady)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check target owner")
		return
	}
	if !targetReady {
		fail(w, r, 409, "managed_owner_required", "Target owner must be ready and API-managed on this branch")
		return
	}
	p := catalogPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint, Name: name, Owner: body.Owner, PreviousOwner: previous, ExpectedVersion: version}
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	conn, err := s.catalogConnection(ctx, p)
	if err != nil {
		s.sqlFailureResponse(w, r, err)
		return
	}
	var observedOwner, marker string
	err = conn.QueryRow(ctx, `SELECT oid,pg_get_userbyid(datdba),COALESCE(shobj_description(oid,'pg_database'),'') FROM pg_database WHERE datname=$1`, name).Scan(&p.DatabaseOID, &observedOwner, &marker)
	conn.Close(context.Background())
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "catalog_unavailable", "Original database observation unavailable")
		return
	}
	if err != nil || observedOwner != previous || marker != "neon-control/branch/"+branch {
		fail(w, r, 409, "catalog_identity_conflict", "Original database identity or owner differs from managed intent")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not admit owner change")
		return
	}
	defer tx.Rollback(r.Context())
	// Same branch lock as every catalog/Endpoint admission. Recheck both roles and
	// parents after the external read; a branch clone cannot snapshot this update.
	var eligible bool
	err = tx.QueryRow(r.Context(), `SELECT p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active'
 AND EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=b.id AND name=$3 AND state='ready')
 AND EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=b.id AND name=$4 AND state='ready')
 FROM projects p JOIN branches b ON b.project_id=p.id JOIN organizations o ON o.id=p.org_id WHERE p.id=$1 AND b.id=$2 FOR UPDATE OF p,b`, project, branch, previous, body.Owner).Scan(&eligible)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not recheck catalog parents")
		return
	}
	if !eligible {
		fail(w, r, 409, "catalog_intent_changed", "Branch, parents or owner intent changed")
		return
	}
	var pending bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, project, branch).Scan(&pending)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect competing Operations")
		return
	}
	if pending {
		fail(w, r, 409, "branch_operation_in_progress", "Another branch Operation is active")
		return
	}
	if err = s.captureCatalogMutationIdentity(r.Context(), tx, &p, "update_database_owner"); err != nil {
		s.catalogRenameFailure(w, r, err)
		return
	}
	if p.NativeIdentity.Object.OID != p.DatabaseOID {
		fail(w, r, 409, "catalog_identity_conflict", "Database was replaced before admission")
		return
	}
	tag, err := tx.Exec(r.Context(), `UPDATE branch_databases SET state='updating',updated_at=now() WHERE branch_id=$1 AND name=$2 AND state='ready' AND version=$3 AND owner_name=$4`, branch, name, version, previous)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not reserve catalog transition")
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, r, 412, "catalog_intent_changed", "Original database intent changed before admission")
		return
	}
	op := newID("op_")
	payload, _ := json.Marshal(p)
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'branch_catalog',$3,'update_database_owner','queued',$4,$5,$6)`, op, project, branch, userFrom(r).ID, requestID(r), payload)
	for ordinal, step := range []string{"apply_postgres_owner", "persist_all_compute_specs", "verify_proxy_routes", "verify_owner_and_commit"} {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, op, ordinal, step)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, op)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'update_database_owner','branch_catalog',$2,$3,'accepted')`, userFrom(r).ID, branch, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayCatalog(w, r, key, hash) {
			return
		}
		fail(w, r, 409, "operation_conflict", "Could not admit owner change")
		return
	}
	s.acceptCatalog(w, r, op)
}

// PostgreSQL privilege enforcement is retained: the service must be allowed to
// SET both owned roles and create databases. No superuser bypass or temporary
// grants are used. OID/marker and old-or-final owner resolve unknown outcomes.
func applyDatabaseOwnerSQL(ctx context.Context, conn *pgx.Conn, p catalogPayload) error {
	if !validPGIdentifier(p.Name) || protectedPGDatabase(p.Name) || !validPGIdentifier(p.Owner) || protectedPGRole(p.Owner) || !validPGIdentifier(p.PreviousOwner) || protectedPGRole(p.PreviousOwner) || p.Owner == p.PreviousOwner || p.DatabaseOID == 0 || p.ExpectedVersion < 1 {
		return errors.New("invalid database owner intent")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if p.NativeIdentity != nil {
		if err = verifyCatalogNativeBinding(ctx, tx, p, "update_database_owner", 0, false); err != nil {
			return err
		}
	}
	var oid uint32
	var owner, marker string
	err = tx.QueryRow(ctx, `SELECT oid,pg_get_userbyid(datdba),COALESCE(shobj_description(oid,'pg_database'),'') FROM pg_database WHERE datname=$1`, p.Name).Scan(&oid, &owner, &marker)
	if err != nil || oid != p.DatabaseOID || marker != "neon-control/branch/"+p.BranchID || (owner != p.PreviousOwner && owner != p.Owner) {
		return errors.New("original database OID, marker or owner rejected")
	}
	for _, role := range []string{p.PreviousOwner, p.Owner} {
		var roleMarker string
		var superuser bool
		err = tx.QueryRow(ctx, `SELECT COALESCE(shobj_description(oid,'pg_authid'),''),rolsuper FROM pg_roles WHERE rolname=$1`, role).Scan(&roleMarker, &superuser)
		if err != nil || superuser || roleMarker != marker {
			return errors.New("original managed owner role rejected")
		}
	}
	if owner != p.Owner {
		if _, err = tx.Exec(ctx, "ALTER DATABASE "+pgQuote(p.Name)+" OWNER TO "+pgQuote(p.Owner)); err != nil {
			return sqlExecutionError{"query", err}
		}
	}
	if p.NativeIdentity != nil {
		if err = verifyCatalogNativeBinding(ctx, tx, p, "update_database_owner", 0, true); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func commitDatabaseOwnerTx(ctx context.Context, tx pgx.Tx, p catalogPayload) error {
	tag, err := tx.Exec(ctx, `UPDATE branch_databases SET state='ready',owner_name=$3,version=version+1,updated_at=now() WHERE branch_id=$1 AND name=$2 AND state='updating' AND owner_name=$4 AND version=$5`, p.BranchID, p.Name, p.Owner, p.PreviousOwner, p.ExpectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var committed bool
	err = tx.QueryRow(ctx, `SELECT state='ready' AND owner_name=$3 AND version=$4 FROM branch_databases WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name, p.Owner, p.ExpectedVersion+1).Scan(&committed)
	if err != nil || !committed {
		return errors.New("database owner metadata changed before commit")
	}
	return nil
}
