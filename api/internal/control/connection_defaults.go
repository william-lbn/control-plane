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

type connectionDefaultInput struct {
	Mode     string `json:"mode"`
	Role     string `json:"role,omitempty"`
	Database string `json:"database,omitempty"`
}
type connectionDefaultObservation struct {
	RoleOID, DatabaseOID                uint32
	TenantID, TimelineID, CredentialRef string
	RoleVersion, DatabaseVersion        int64
	DatabaseOwner, PlatformDatabase     string
}

func (v connectionDefaultInput) valid() bool {
	return v.Mode == "platform" && v.Role == "" && v.Database == "" ||
		v.Mode == "managed" && validPGIdentifier(v.Role) && !protectedPGRole(v.Role) && validPGIdentifier(v.Database) &&
			(!protectedPGDatabase(v.Database) || v.Database == "postgres")
}

// This projection is for public connection forms/SQL defaults only. Controllers
// must continue using the original endpoint bootstrap and service identities.
func (s *server) publicConnectionDefaults(ctx context.Context, endpoint record) (record, error) {
	var mode, role, database, platformRole, platformDatabase string
	var version int64
	err := s.db.QueryRow(ctx, `SELECT COALESCE(d.mode,'platform'),
 CASE WHEN d.mode='managed' THEN d.role_name ELSE e.role_name END,
 CASE WHEN d.mode='managed' THEN d.database_name ELSE e.database_name END,COALESCE(d.version,1),e.role_name,e.database_name
 FROM endpoints e JOIN branches b ON b.id=e.branch_id AND b.project_id=e.project_id
 LEFT JOIN branch_connection_defaults d ON d.branch_id=b.id
 WHERE e.id=$1 AND e.project_id=$2 AND e.branch_id=$3 AND e.deleted_at IS NULL AND b.deleted_at IS NULL`, endpoint["id"], endpoint["project_id"], endpoint["branch_id"]).Scan(&mode, &role, &database, &version, &platformRole, &platformDatabase)
	if err != nil {
		return nil, err
	}
	return record{"mode": mode, "role": role, "database": database, "version_token": strconv.FormatInt(version, 10), "platform_role": platformRole, "platform_database": platformDatabase}, nil
}

func (s *server) connectionDefaults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	project, branch := r.PathValue("project"), r.PathValue("branch")
	if r.Method == http.MethodGet {
		endpoint, err := s.catalogWriter(r.Context(), project, branch)
		if err != nil {
			if isNoRows(err) {
				err = renameFault(409, "writer_not_ready", "Ready branch writer required")
			}
			s.connectionDefaultsFailure(w, r, err)
			return
		}
		value, err := s.publicConnectionDefaults(r.Context(), record{"id": endpoint, "project_id": project, "branch_id": branch})
		if err != nil {
			s.connectionDefaultsFailure(w, r, err)
			return
		}
		w.Header().Set("ETag", `"`+stringVal(value["version_token"])+`"`)
		jsonResponse(w, 200, value)
		return
	}
	var input connectionDefaultInput
	if readJSON(r, &input) != nil || !input.valid() {
		fail(w, r, 422, "invalid_connection_defaults", "Choose platform defaults or a managed public role and database")
		return
	}
	raw := r.Header.Get("If-Match")
	version, err := strconv.ParseInt(strings.Trim(raw, `"`), 10, 64)
	if raw == "" {
		fail(w, r, 428, "version_required", "Original quoted defaults version required")
		return
	}
	if err != nil || version < 1 || version >= 9223372036854775806 || raw != `"`+strconv.FormatInt(version, 10)+`"` {
		fail(w, r, 422, "invalid_version", "Use one canonical quoted positive version")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	actor := userFrom(r).ID
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, record{"input": input, "version": version})
	// A lost acknowledgement replays before stale-version/native observation.
	if value, err := readConnectionDefaultReceipt(r.Context(), s.db, actor, keyHash(key), hash); err != nil {
		s.connectionDefaultsFailure(w, r, err)
		return
	} else if value != nil {
		jsonResponse(w, 200, value)
		return
	}
	// Replaying a committed request must work even after its original Writer
	// retires. New requests establish current metadata before any cold wake.
	var current int64
	err = s.db.QueryRow(r.Context(), `SELECT COALESCE((SELECT version FROM branch_connection_defaults WHERE branch_id=$1),1)`, branch).Scan(&current)
	if err != nil {
		s.connectionDefaultsFailure(w, r, err)
		return
	}
	if current != version {
		s.connectionDefaultsFailure(w, r, renameFault(412, "version_conflict", "Connection defaults changed; refresh before retrying"))
		return
	}
	endpoint, err := s.catalogWriter(r.Context(), project, branch)
	if err != nil {
		if isNoRows(err) {
			err = renameFault(409, "writer_not_ready", "Ready branch writer required")
		}
		s.connectionDefaultsFailure(w, r, err)
		return
	}
	observation := connectionDefaultObservation{}
	if input.Mode == "managed" {
		observation, err = s.observeConnectionDefault(r.Context(), project, branch, endpoint, input)
		if err != nil {
			s.connectionDefaultsFailure(w, r, err)
			return
		}
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.connectionDefaultsFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	value, err := commitConnectionDefaultTx(r.Context(), tx, project, branch, actor, requestID(r), keyHash(key), hash, version, input, observation)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if previous, replayErr := readConnectionDefaultReceipt(r.Context(), s.db, actor, keyHash(key), hash); replayErr != nil {
			s.connectionDefaultsFailure(w, r, replayErr)
			return
		} else if previous != nil {
			jsonResponse(w, 200, previous)
			return
		}
		s.connectionDefaultsFailure(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+stringVal(value["version_token"])+`"`)
	jsonResponse(w, 200, value)
}

func (s *server) connectionDefaultsFailure(w http.ResponseWriter, r *http.Request, err error) {
	var fault *catalogRenameFault
	if errors.As(err, &fault) {
		fail(w, r, fault.Status, fault.Code, fault.Message)
		return
	}
	fail(w, r, 503, "connection_defaults_unavailable", "Could not establish connection defaults; retain the original request key")
}

type connectionDefaultReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readConnectionDefaultReceipt(ctx context.Context, db connectionDefaultReader, actor, key, hash string) (record, error) {
	var old string
	var raw []byte
	err := db.QueryRow(ctx, `SELECT request_hash,response FROM connection_default_requests WHERE actor_id=$1 AND key_hash=$2`, actor, key).Scan(&old, &raw)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if old != hash {
		return nil, renameFault(409, "idempotency_conflict", "Original request key was used for a different request")
	}
	var value record
	if err = json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func (s *server) observeConnectionDefault(ctx context.Context, project, branch, endpoint string, input connectionDefaultInput) (connectionDefaultObservation, error) {
	result := connectionDefaultObservation{}
	var owner string
	var managedDB bool
	err := s.db.QueryRow(ctx, `SELECT r.credential_ref,p.tenant_id,b.timeline_id,r.version,
 EXISTS(SELECT 1 FROM branch_databases d WHERE d.branch_id=b.id AND d.name=$4 AND d.state='ready'),
 COALESCE((SELECT owner_name FROM branch_databases d WHERE d.branch_id=b.id AND d.name=$4 AND d.state='ready'),''),
 COALESCE((SELECT version FROM branch_databases d WHERE d.branch_id=b.id AND d.name=$4 AND d.state='ready'),0),
 e.database_name
 FROM projects p JOIN branches b ON b.project_id=p.id JOIN branch_roles r ON r.branch_id=b.id
 JOIN endpoints e ON e.id=$5 AND e.branch_id=b.id AND e.project_id=p.id AND e.deleted_at IS NULL
 WHERE p.id=$1 AND b.id=$2 AND r.name=$3 AND r.state='ready'`, project, branch, input.Role, input.Database, endpoint).Scan(&result.CredentialRef, &result.TenantID, &result.TimelineID, &result.RoleVersion, &managedDB, &owner, &result.DatabaseVersion, &result.PlatformDatabase)
	result.DatabaseOwner = owner
	if isNoRows(err) {
		return result, renameFault(409, "managed_role_required", "Ready API-managed role required")
	}
	if err != nil {
		return result, err
	}
	if !managedDB && input.Database != result.PlatformDatabase {
		return result, renameFault(409, "managed_database_required", "Managed database or original platform default database required")
	}
	ctx, cancel := context.WithTimeout(ctx, sqlTotalTimeout)
	defer cancel()
	release, err := s.acquireProbeGate(ctx, endpoint, true)
	if err != nil {
		return result, err
	}
	defer release()
	conn, err := s.catalogConnection(ctx, catalogPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint})
	if err != nil {
		return result, err
	}
	defer conn.Close(context.Background())
	return observeConnectionDefaultSQL(ctx, conn, input, result, managedDB, owner, branch)
}

func observeConnectionDefaultSQL(ctx context.Context, conn *pgx.Conn, input connectionDefaultInput, result connectionDefaultObservation, managedDB bool, owner, branch string) (connectionDefaultObservation, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout='10s'`); err != nil {
		return result, err
	}
	var tenant, timeline, roleMarker, dbMarker, dbOwner string
	var login, superuser, connect, template bool
	err = tx.QueryRow(ctx, `SELECT current_setting('neon.tenant_id',true),current_setting('neon.timeline_id',true)`).Scan(&tenant, &timeline)
	if err != nil || tenant != result.TenantID || timeline != result.TimelineID || len(tenant) != 32 || len(timeline) != 32 {
		return result, renameFault(409, "catalog_scope_conflict", "Native tenant/timeline differs from the selected branch")
	}
	err = tx.QueryRow(ctx, `SELECT oid,rolcanlogin,rolsuper,COALESCE(shobj_description(oid,'pg_authid'),'') FROM pg_roles WHERE rolname=$1`, input.Role).Scan(&result.RoleOID, &login, &superuser, &roleMarker)
	if err != nil && !isNoRows(err) {
		return result, err
	}
	if err != nil || !login || superuser || roleMarker != "neon-control/branch/"+branch {
		return result, renameFault(409, "catalog_identity_conflict", "Managed role native identity differs from intent")
	}
	err = tx.QueryRow(ctx, `SELECT oid,datallowconn,datistemplate,pg_get_userbyid(datdba),COALESCE(shobj_description(oid,'pg_database'),'') FROM pg_database WHERE datname=$1`, input.Database).Scan(&result.DatabaseOID, &connect, &template, &dbOwner, &dbMarker)
	if err != nil && !isNoRows(err) {
		return result, err
	}
	if err != nil || !connect || template || (managedDB && (dbOwner != owner || dbMarker != "neon-control/branch/"+branch)) {
		return result, renameFault(409, "catalog_identity_conflict", "Database native identity differs from intent")
	}
	// An untracked arbitrary SQL database is not adopted by selecting defaults.
	// The sole exception is the original platform default database, checked again
	// against endpoint metadata in the atomic commit below.
	if !managedDB && input.Database != result.PlatformDatabase {
		return result, renameFault(409, "managed_database_required", "Managed database or original platform default database required")
	}
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT has_database_privilege($1::oid,$2::oid,'CONNECT')`, result.RoleOID, result.DatabaseOID).Scan(&allowed); err != nil {
		return result, err
	}
	if !allowed {
		return result, renameFault(409, "database_connect_required", "Selected role requires CONNECT on this database")
	}
	return result, tx.Commit(ctx)
}

func commitConnectionDefaultTx(ctx context.Context, tx pgx.Tx, project, branch, actor, request, key, hash string, version int64, input connectionDefaultInput, observed connectionDefaultObservation) (record, error) {
	if !input.valid() || version < 1 {
		return nil, errors.New("invalid defaults intent")
	}
	var orgRole, grant string
	err := tx.QueryRow(ctx, `SELECT m.role,COALESCE(g.role,'') FROM projects p JOIN branches b ON b.project_id=p.id
 JOIN organizations o ON o.id=p.org_id JOIN users u ON u.id=$3 AND NOT u.disabled
 JOIN organization_members m ON m.org_id=o.id AND m.user_id=u.id LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id
 WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL
 AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active' FOR UPDATE OF p,b`, project, branch, actor).Scan(&orgRole, &grant)
	if err != nil && !isNoRows(err) {
		return nil, err
	}
	if isNoRows(err) || effectivePermission(orgRole, grant) < 3 {
		return nil, renameFault(403, "forbidden", "Current project Admin required")
	}
	if value, err := readConnectionDefaultReceipt(ctx, tx, actor, key, hash); err != nil || value != nil {
		return value, err
	}
	var current int64
	var pending bool
	var platformRole, platformDatabase string
	err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM branch_connection_defaults WHERE branch_id=$1),1),
 EXISTS(SELECT 1 FROM operations WHERE project_id=$2 AND state IN ('queued','running','retry_wait') AND (resource_id=$1 OR payload->>'branch_id'=$1)),
 role_name,database_name FROM endpoints WHERE project_id=$2 AND branch_id=$1 AND endpoint_type='read_write' AND state='active' AND deleted_at IS NULL`, branch, project).Scan(&current, &pending, &platformRole, &platformDatabase)
	if isNoRows(err) {
		return nil, renameFault(409, "writer_not_ready", "Ready branch writer required")
	}
	if err != nil {
		return nil, err
	}
	if current != version {
		return nil, renameFault(412, "version_conflict", "Connection defaults changed; refresh before retrying")
	}
	if pending {
		return nil, renameFault(409, "branch_operation_in_progress", "Another branch Operation is active")
	}
	role, database := platformRole, platformDatabase
	if input.Mode == "managed" {
		var eligible bool
		err = tx.QueryRow(ctx, `SELECT p.tenant_id=$4 AND b.timeline_id=$5
 AND EXISTS(SELECT 1 FROM branch_roles r WHERE r.branch_id=b.id AND r.name=$3 AND r.state='ready' AND r.credential_ref=$6 AND r.version=$9)
 AND (($7=$8 AND $10::bigint=0) OR EXISTS(SELECT 1 FROM branch_databases d WHERE d.branch_id=b.id AND d.name=$7 AND d.state='ready' AND d.version=$10 AND d.owner_name=$11))
 FROM projects p JOIN branches b ON b.project_id=p.id WHERE p.id=$1 AND b.id=$2`, project, branch, input.Role, observed.TenantID, observed.TimelineID, observed.CredentialRef, input.Database, platformDatabase, observed.RoleVersion, observed.DatabaseVersion, observed.DatabaseOwner).Scan(&eligible)
		if err != nil {
			return nil, err
		}
		if !eligible || observed.RoleOID == 0 || observed.DatabaseOID == 0 {
			return nil, renameFault(409, "catalog_intent_changed", "Original role, database or branch identity changed")
		}
		role, database = input.Role, input.Database
	}
	_, err = tx.Exec(ctx, `INSERT INTO branch_connection_defaults(branch_id,mode,role_name,database_name,role_oid,database_oid,tenant_id,timeline_id,version,updated_by)
 VALUES($1,$2,CASE WHEN $2='managed' THEN $3 END,CASE WHEN $2='managed' THEN $4 END,
 CASE WHEN $2='managed' THEN $5::bigint END,CASE WHEN $2='managed' THEN $6::bigint END,
 CASE WHEN $2='managed' THEN $7 END,CASE WHEN $2='managed' THEN $8 END,$9,$10)
 ON CONFLICT(branch_id) DO UPDATE SET mode=EXCLUDED.mode,role_name=EXCLUDED.role_name,database_name=EXCLUDED.database_name,
 role_oid=EXCLUDED.role_oid,database_oid=EXCLUDED.database_oid,tenant_id=EXCLUDED.tenant_id,timeline_id=EXCLUDED.timeline_id,
 version=EXCLUDED.version,updated_by=EXCLUDED.updated_by,updated_at=clock_timestamp()`, branch, input.Mode, input.Role, input.Database, int64(observed.RoleOID), int64(observed.DatabaseOID), observed.TenantID, observed.TimelineID, version+1, actor)
	value := record{"mode": input.Mode, "role": role, "database": database, "version_token": strconv.FormatInt(version+1, 10), "platform_role": platformRole, "platform_database": platformDatabase}
	response, marshalErr := json.Marshal(value)
	if err == nil {
		err = marshalErr
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO connection_default_requests(actor_id,key_hash,request_hash,branch_id,response) VALUES($1,$2,$3,$4,$5)`, actor, key, hash, branch, response)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'update_connection_defaults','branch',$2,$3,'succeeded')`, actor, branch, request)
	}
	return value, err
}

func checkConnectionDefaultDependency(ctx context.Context, db connectionDefaultReader, branch, kind, name string) error {
	column := "database_name"
	if kind == "role" {
		column = "role_name"
	}
	var held bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_connection_defaults WHERE branch_id=$1 AND mode='managed' AND `+column+`=$2)`, branch, name).Scan(&held); err != nil {
		return err
	}
	if held {
		return renameFault(409, "connection_default_dependency", "Select different customer connection defaults before deleting or renaming this object")
	}
	return nil
}
