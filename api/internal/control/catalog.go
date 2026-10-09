package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Catalog payloads contain references, never passwords or verifiers. The
// credential Secret is immutable per Operation and safe to replay after crash.
type catalogPayload struct {
	ProjectID     string `json:"project_id"`
	BranchID      string `json:"branch_id"`
	EndpointID    string `json:"endpoint_id"`
	Name          string `json:"name"`
	Owner         string `json:"owner,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
}

func validPGIdentifier(name string) bool {
	if name == "" || len(name) > 63 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func protectedPGRole(name string) bool {
	n := strings.ToLower(name)
	return n == "cloud_admin" || n == "postgres" || strings.HasPrefix(n, "pg_") || strings.HasPrefix(n, "neon_") || strings.HasPrefix(n, "control_") || strings.HasPrefix(n, "app_da_")
}
func protectedPGDatabase(name string) bool {
	n := strings.ToLower(name)
	return n == "postgres" || n == "template0" || n == "template1" || n == "neon_admin"
}
func pgQuote(name string) string { return pgx.Identifier{name}.Sanitize() }

func (s *server) catalogWriter(ctx context.Context, project, branch string) (string, error) {
	var endpoint string
	err := s.db.QueryRow(ctx, `SELECT e.id FROM endpoints e JOIN branches b ON b.id=e.branch_id
        WHERE b.id=$1 AND b.project_id=$2 AND b.deleted_at IS NULL AND b.state='ready'
        AND e.project_id=b.project_id AND e.endpoint_type='read_write' AND e.deleted_at IS NULL
        AND e.state='active'`, branch, project).Scan(&endpoint)
	return endpoint, err
}

// All catalog traffic follows the same Proxy endpoint selector and TLS policy
// as user traffic. No direct guest access or public superuser password is used.
func (s *server) catalogConnection(ctx context.Context, p catalogPayload) (*pgx.Conn, error) {
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(p.EndpointID)+"-credentials"), nil)
	if err != nil || !owned(secret, p.ProjectID, p.EndpointID) {
		return nil, errors.New("catalog service credential unavailable")
	}
	password, err := secretText(secret, "probePassword")
	if err != nil {
		return nil, err
	}
	return connectSQL(ctx, s.proxyHost, s.proxyPort, "control_probe", password, "postgres", selector(p.EndpointID))
}

func (s *server) catalogList(w http.ResponseWriter, r *http.Request) {
	project, branch := r.PathValue("project"), r.PathValue("branch")
	endpoint, err := s.catalogWriter(r.Context(), project, branch)
	if err != nil {
		fail(w, r, 404, "writer_not_found", "Ready branch writer required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	conn, err := s.catalogConnection(ctx, catalogPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint})
	if err != nil {
		s.sqlFailureResponse(w, r, err)
		return
	}
	defer conn.Close(context.Background())
	isRole := strings.HasSuffix(r.URL.Path, "/roles")
	query := `SELECT d.datname AS name,pg_get_userbyid(d.datdba) AS owner,pg_database_size(d.oid) AS size_bytes
        FROM pg_database d WHERE NOT d.datistemplate AND d.datallowconn ORDER BY d.datname`
	if isRole {
		query = `SELECT rolname AS name,rolcanlogin AS can_login,rolsuper AS superuser,
        rolcreaterole AS create_role,rolcreatedb AS create_database,rolbypassrls AS bypass_rls,
        rolreplication AS replication FROM pg_roles WHERE rolcanlogin ORDER BY rolname`
	}
	rows, err := conn.Query(ctx, query)
	if err != nil {
		s.sqlFailureResponse(w, r, sqlExecutionError{"query", err})
		return
	}
	items, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		fail(w, r, 503, "catalog_unavailable", "Could not read database catalog")
		return
	}
	table := "branch_databases"
	if isRole {
		table = "branch_roles"
	}
	intents, err := s.many(ctx, "SELECT name,state,version FROM "+table+" WHERE branch_id=$1 AND state<>'deleted'", branch)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read catalog intent")
		return
	}
	byName := map[string]record{}
	for _, v := range intents {
		byName[stringVal(v["name"])] = v
	}
	for _, v := range items {
		n := stringVal(v["name"])
		v["managed"] = byName[n] != nil
		v["state"] = "ready"
		if intent := byName[n]; intent != nil {
			v["state"] = intent["state"]
			v["version"] = intent["version"]
			delete(byName, n)
		}
		if isRole {
			v["protected"] = protectedPGRole(n)
		} else {
			v["protected"] = protectedPGDatabase(n)
		}
	}
	for _, v := range byName {
		v["managed"] = true
		v["protected"] = false
		items = append(items, v)
	}
	if items == nil {
		items = []record{}
	}
	jsonResponse(w, 200, map[string]any{"items": items, "next_cursor": nil, "branch_id": branch, "endpoint_id": endpoint})
}

func (s *server) catalogMutation(w http.ResponseWriter, r *http.Request) {
	project, branch := r.PathValue("project"), r.PathValue("branch")
	endpoint, err := s.catalogWriter(r.Context(), project, branch)
	if err != nil {
		fail(w, r, 404, "writer_not_found", "Ready branch writer required")
		return
	}
	action, isRole := "create_database", false
	name := r.PathValue("database")
	if strings.Contains(r.URL.Path, "/roles") {
		isRole = true
		action = "create_role"
		name = r.PathValue("role")
	}
	if r.Method == http.MethodDelete {
		if isRole {
			action = "delete_role"
		} else {
			action = "delete_database"
		}
	}
	if r.Method == http.MethodPatch {
		action = "rotate_role_password"
	}
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password,omitempty"`
		Owner    string `json:"owner,omitempty"`
	}
	if r.Method != http.MethodDelete {
		if err := readJSON(r, &body); err != nil {
			fail(w, r, 422, "invalid_request", "Invalid catalog request")
			return
		}
	}
	if name == "" {
		name = body.Name
	} else if body.Name != "" && body.Name != name {
		fail(w, r, 422, "invalid_request", "Resource name cannot change")
		return
	}
	if !validPGIdentifier(name) || (isRole && protectedPGRole(name)) || (!isRole && protectedPGDatabase(name)) {
		fail(w, r, 422, "protected_or_invalid_name", "Use a valid non-system PostgreSQL identifier of at most 63 bytes")
		return
	}
	if isRole && r.Method != http.MethodDelete && (len(body.Password) < 12 || len(body.Password) > 256) {
		fail(w, r, 422, "invalid_password", "Password must contain 12-256 bytes")
		return
	}
	if !isRole && r.Method == http.MethodPost && (!validPGIdentifier(body.Owner) || protectedPGRole(body.Owner)) {
		fail(w, r, 422, "invalid_owner", "Choose a non-system database owner role")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, body)
	if s.replayCatalog(w, r, key, hash) {
		return
	}
	if r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
		defer cancel()
		conn, e := s.catalogConnection(ctx, catalogPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint})
		if e != nil {
			s.sqlFailureResponse(w, r, e)
			return
		}
		query := "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)"
		if isRole {
			query = "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)"
		}
		var exists bool
		e = conn.QueryRow(ctx, query, name).Scan(&exists)
		conn.Close(context.Background())
		if e != nil {
			fail(w, r, 503, "catalog_unavailable", "Could not check existing database object")
			return
		}
		if exists {
			fail(w, r, 409, "name_or_resource_conflict", "An existing PostgreSQL object cannot be adopted by CREATE")
			return
		}
	}
	suffix := stableSuffix(userFrom(r).ID, r.Method+":"+r.URL.Path, key)
	p := catalogPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint, Name: name, Owner: body.Owner}
	if isRole && r.Method != http.MethodDelete {
		verifier, err := scramVerifier(body.Password)
		if err != nil {
			fail(w, r, 422, "invalid_password", "Could not derive password verifier")
			return
		}
		p.CredentialRef = "catalog-" + suffix
		secret, err := s.kube.createOwned(r.Context(), "secret", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata":  map[string]any{"name": p.CredentialRef, "labels": credentialLabels(project, endpoint)},
			"immutable": true, "data": map[string]string{"verifier": base64.StdEncoding.EncodeToString([]byte(verifier))}}, project, endpoint)
		if err != nil {
			fail(w, r, 503, "credential_unavailable", "Could not reserve credential verifier")
			return
		}
		stored, e := secretText(secret, "verifier")
		if e != nil || !scramMatches(body.Password, stored) {
			fail(w, r, 409, "credential_conflict", "Credential reservation conflicts with request")
			return
		}
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	// Shared branch-row lock coordinates API admission and endpoint creation.
	var state string
	err = tx.QueryRow(r.Context(), "SELECT state FROM branches WHERE id=$1 AND project_id=$2 FOR UPDATE", branch, project).Scan(&state)
	if err != nil || state != "ready" {
		fail(w, r, 409, "branch_not_ready", "Branch is not ready")
		return
	}
	var pending bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1
        AND state IN ('queued','running','retry_wait') AND (resource_id=$2 AND resource_type='branch_catalog'
            OR action='create_endpoint' AND payload->>'branch_id'=$2))`, project, branch).Scan(&pending)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check catalog operations")
		return
	}
	if pending {
		fail(w, r, 409, "branch_operation_in_progress", "Another branch operation is active")
		return
	}
	table := "branch_databases"
	if isRole {
		table = "branch_roles"
	}
	var oldState string
	e := tx.QueryRow(r.Context(), "SELECT state FROM "+table+" WHERE branch_id=$1 AND name=$2", branch, name).Scan(&oldState)
	if e != nil && !isNoRows(e) {
		fail(w, r, 503, "metadata_unavailable", "Could not check catalog intent")
		return
	}
	if r.Method == http.MethodPost && e == nil && oldState != "deleted" {
		fail(w, r, 409, "name_or_resource_conflict", "Managed resource already exists")
		return
	}
	if r.Method != http.MethodPost && (isNoRows(e) || oldState != "ready") {
		fail(w, r, 409, "managed_resource_required", "A ready API-managed resource is required")
		return
	}
	if !isRole && r.Method == http.MethodDelete {
		var retainedStorage bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM object_storage_instances WHERE branch_id=$1 AND spec->>'database'=$2)`, branch, name).Scan(&retainedStorage); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not check retained storage directory")
			return
		}
		if retainedStorage {
			fail(w, r, 409, "storage_database_retained", "Object manifests are retained in this database; deleting it requires a separately verified storage purge workflow")
			return
		}
		if err = tx.QueryRow(r.Context(), "SELECT owner_name FROM branch_databases WHERE branch_id=$1 AND name=$2", branch, name).Scan(&p.Owner); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read database owner intent")
			return
		}
	}
	if r.Method == http.MethodPost {
		if isRole {
			_, err = tx.Exec(r.Context(), `INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES($1,$2,'creating',$3)
            ON CONFLICT(branch_id,name) DO UPDATE SET state='creating',credential_ref=$3,version=branch_roles.version+1,updated_at=now()`, branch, name, p.CredentialRef)
		} else {
			_, err = tx.Exec(r.Context(), `INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES($1,$2,$3,'creating')
                ON CONFLICT(branch_id,name) DO UPDATE SET state='creating',owner_name=$3,version=branch_databases.version+1,updated_at=now()`, branch, name, body.Owner)
		}
	} else {
		intentState := "deleting"
		if action == "rotate_role_password" {
			intentState = "updating"
		}
		_, err = tx.Exec(r.Context(), "UPDATE "+table+" SET state=$3,updated_at=now() WHERE branch_id=$1 AND name=$2", branch, name, intentState)
	}
	opID := newID("op_")
	payload, _ := json.Marshal(p)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
        VALUES($1,$2,'branch_catalog',$3,$4,'queued',$5,$6,$7)`, opID, project, branch, action, userFrom(r).ID, requestID(r), payload)
	}
	for ordinal, step := range []string{"apply_postgres_catalog", "persist_all_compute_specs", "publish_all_proxy_credentials", "verify_catalog_and_commit"} {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, opID, ordinal, step)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, opID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayCatalog(w, r, key, hash) {
			return
		}
		fail(w, r, 409, "operation_conflict", "Could not admit catalog operation")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, action, "branch_catalog", branch, "accepted", requestID(r))
	s.acceptCatalog(w, r, opID)
}

func (s *server) replayCatalog(w http.ResponseWriter, r *http.Request, key, hash string) bool {
	var stored, id string
	err := s.db.QueryRow(r.Context(), `SELECT request_hash,operation_id FROM idempotency_keys WHERE actor_id=$1 AND key_hash=$2`, userFrom(r).ID, keyHash(key)).Scan(&stored, &id)
	if isNoRows(err) {
		return false
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check idempotency")
		return true
	}
	if stored != hash {
		fail(w, r, 409, "idempotency_conflict", "Key used with different input")
		return true
	}
	s.acceptCatalog(w, r, id)
	return true
}
func (s *server) acceptCatalog(w http.ResponseWriter, r *http.Request, id string) {
	op, err := s.operationRecord(r.Context(), id, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load catalog operation")
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+r.PathValue("project")+"/operations/"+id)
	jsonResponse(w, 202, map[string]any{"operation": op, "branch_id": r.PathValue("branch")})
}

func (s *server) applyCatalogSQL(ctx context.Context, p catalogPayload, action, verifier string) error {
	// A new VM may accept SQL before its management Service has endpoints.
	// Re-observe the live catalog on each transport retry: an unknown native
	// CREATE outcome is never treated as permission to create a second object.
	for attempt := 0; ; attempt++ {
		err := s.applyCatalogSQLOnce(ctx, p, action, verifier)
		var dependency kubeError
		if action != "create_role" || !errors.As(err, &dependency) || (dependency.Status != 502 && dependency.Status != 503) || attempt >= 14 {
			return err
		}
		s.logger.Info("catalog management readiness retry", "endpoint_id", p.EndpointID, "attempt", attempt+1, "http_status", dependency.Status)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *server) applyCatalogSQLOnce(ctx context.Context, p catalogPayload, action, verifier string) error {
	if action == "create_role" {
		// The native privileged management API grants neon_superuser; the
		// service probe is not granted global ADMIN on that protected role.
		conn, err := s.catalogConnection(ctx, p)
		if err != nil {
			return err
		}
		var exists bool
		var admin bool
		err = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", p.Name).Scan(&exists)
		if err == nil && exists {
			err = conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_auth_members m
			JOIN pg_roles role ON role.oid=m.roleid JOIN pg_roles member ON member.oid=m.member
			WHERE role.rolname=$1 AND member.rolname='control_probe' AND m.admin_option)`, p.Name).Scan(&admin)
		}
		conn.Close(context.Background())
		if err != nil {
			return err
		}
		if !exists {
			if err = s.nativeCreateRole(ctx, p, verifier); err != nil {
				return err
			}
		} else if !admin {
			return errors.New("existing role is not owned by catalog service")
		}
	}
	conn, err := s.catalogConnection(ctx, p)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	name := pgQuote(p.Name)
	escaped, err := conn.PgConn().EscapeString(verifier)
	if err != nil {
		return err
	}
	literal := "'" + escaped + "'"
	marker := "neon-control/branch/" + p.BranchID
	quotedMarker := "'" + strings.ReplaceAll(marker, "'", "''") + "'"
	switch action {
	case "create_role", "rotate_role_password":
		// pgx identifier quoting and pgconn literal quoting prevent injection.
		// The literal is an immutable SCRAM verifier, never the user's password.
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(context.Background())
		var comment string
		e = tx.QueryRow(ctx, "SELECT COALESCE(shobj_description(oid,'pg_authid'),'') FROM pg_roles WHERE rolname=$1", p.Name).Scan(&comment)
		if e != nil && !isNoRows(e) {
			return e
		}
		if action == "create_role" && isNoRows(e) {
			var count int
			if err = tx.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolcanlogin").Scan(&count); err != nil {
				return err
			}
			if count >= 500 {
				return errors.New("branch role quota exceeded")
			}
			_, err = tx.Exec(ctx, "CREATE ROLE "+name+" LOGIN INHERIT NOSUPERUSER CREATEROLE CREATEDB BYPASSRLS REPLICATION IN ROLE neon_superuser PASSWORD "+literal)
			if err == nil {
				_, err = tx.Exec(ctx, "COMMENT ON ROLE "+name+" IS "+quotedMarker)
			}
			if err == nil {
				_, err = tx.Exec(ctx, "GRANT "+name+" TO control_probe")
			}
		} else {
			if isNoRows(e) {
				return errors.New("role no longer exists")
			}
			if comment != marker && !(action == "create_role" && comment == "") {
				return errors.New("role ownership marker mismatch")
			}
			if comment == "" {
				if _, err = tx.Exec(ctx, "COMMENT ON ROLE "+name+" IS "+quotedMarker); err != nil {
					return sqlExecutionError{"query", err}
				}
			}
			_, err = tx.Exec(ctx, "ALTER ROLE "+name+" LOGIN PASSWORD "+literal)
			if err == nil && action == "create_role" {
				_, err = tx.Exec(ctx, "GRANT "+name+" TO control_probe WITH INHERIT TRUE, SET TRUE")
			}
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
	case "delete_role":
		var comment string
		e := conn.QueryRow(ctx, "SELECT COALESCE(shobj_description(oid,'pg_authid'),'') FROM pg_roles WHERE rolname=$1", p.Name).Scan(&comment)
		if e != nil && !isNoRows(e) {
			return e
		}
		if e == nil && comment != marker {
			return errors.New("role ownership marker mismatch")
		}
		_, err = conn.Exec(ctx, "DROP ROLE IF EXISTS "+name)
	case "create_database":
		if _, err = conn.Exec(ctx, "SET ROLE "+pgQuote(p.Owner)); err != nil {
			return sqlExecutionError{"session", err}
		}
		var owner, comment string
		e := conn.QueryRow(ctx, "SELECT pg_get_userbyid(datdba),COALESCE(shobj_description(oid,'pg_database'),'') FROM pg_database WHERE datname=$1", p.Name).Scan(&owner, &comment)
		if isNoRows(e) {
			_, err = conn.Exec(ctx, "CREATE DATABASE "+name+" OWNER "+pgQuote(p.Owner))
			if err == nil {
				_, err = conn.Exec(ctx, "COMMENT ON DATABASE "+name+" IS "+quotedMarker)
			}
		} else if e != nil {
			err = e
		} else if owner != p.Owner || comment != marker {
			err = errors.New("database ownership conflicts with intent; manual outcome verification required")
		}
	case "delete_database":
		var comment string
		e := conn.QueryRow(ctx, "SELECT COALESCE(shobj_description(oid,'pg_database'),'') FROM pg_database WHERE datname=$1", p.Name).Scan(&comment)
		if e != nil && !isNoRows(e) {
			return e
		}
		if e == nil && comment != marker {
			return errors.New("database ownership marker mismatch")
		}
		if _, err = conn.Exec(ctx, "SET ROLE "+pgQuote(p.Owner)); err != nil {
			return sqlExecutionError{"session", err}
		}
		_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	default:
		return errors.New("unsupported catalog action")
	}
	if err != nil {
		return sqlExecutionError{"query", err}
	}
	return nil
}

func catalogDelta(config map[string]any, p catalogPayload, action, verifier string) error {
	cluster, ok := nested(config, "spec", "cluster").(map[string]any)
	if !ok {
		return errors.New("invalid catalog compute spec")
	}
	roleAction := strings.Contains(action, "role")
	key := "databases"
	if roleAction {
		key = "roles"
	}
	values, _ := cluster[key].([]any)
	next := []any{}
	for _, v := range values {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("invalid compute catalog member")
		}
		if m["name"] != p.Name {
			next = append(next, m)
		}
	}
	if !strings.HasPrefix(action, "delete_") {
		v := map[string]any{"name": p.Name, "options": nil}
		if roleAction {
			v["encrypted_password"] = verifier
		} else {
			v["owner"] = p.Owner
		}
		next = append(next, v)
	}
	cluster[key] = next
	spec := config["spec"].(map[string]any)
	old, _ := spec["delta_operations"].([]any)
	delta := []any{}
	deltaAction := "delete_db"
	if roleAction {
		deltaAction = "delete_role"
	}
	for _, v := range old {
		m := v.(map[string]any)
		if !(m["name"] == p.Name && m["action"] == deltaAction) {
			delta = append(delta, v)
		}
	}
	if strings.HasPrefix(action, "delete_") {
		delta = append(delta, map[string]any{"action": deltaAction, "name": p.Name, "new_name": nil})
	}
	spec["delta_operations"] = delta
	return nil
}

func (s *server) catalogEndpoints(ctx context.Context, p catalogPayload) ([]record, error) {
	return s.many(ctx, "SELECT id FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND deleted_at IS NULL AND state='active' ORDER BY id", p.ProjectID, p.BranchID)
}
func (s *server) updateCatalogSpecs(ctx context.Context, p catalogPayload, action, verifier string) error {
	endpoints, err := s.catalogEndpoints(ctx, p)
	if err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		id := stringVal(endpoint["id"])
		for attempt := 0; attempt < 6; attempt++ {
			secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(id)+"-config"), nil)
			if err != nil || !owned(secret, p.ProjectID, id) {
				return errors.New("catalog compute config unavailable or unowned")
			}
			raw, err := secretText(secret, "config.json")
			if err != nil {
				return err
			}
			var config map[string]any
			if err = json.Unmarshal([]byte(raw), &config); err != nil {
				return err
			}
			if err = catalogDelta(config, p, action, verifier); err != nil {
				return err
			}
			encoded, _ := json.Marshal(config)
			secret["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(encoded)
			_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", kubeName(id)+"-config"), secret)
			var ke kubeError
			if errors.As(err, &ke) && ke.Status == 409 {
				continue
			}
			if err != nil {
				return err
			}
			break
		}
		// Read after CAS: a retry that exhausted all conflicts must fail.
		stored, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(id)+"-config"), nil)
		if err != nil {
			return err
		}
		raw, err := secretText(stored, "config.json")
		if err != nil {
			return err
		}
		var config map[string]any
		if err = json.Unmarshal([]byte(raw), &config); err != nil {
			return err
		}
		expected, _ := json.Marshal(config)
		if err = catalogDelta(config, p, action, verifier); err != nil {
			return err
		}
		actual, _ := json.Marshal(config)
		if string(expected) != string(actual) {
			return errors.New("catalog config update conflict")
		}
	}
	return nil
}
func (s *server) updateCatalogRoutes(ctx context.Context, p catalogPayload, action, verifier string) error {
	endpoints, err := s.catalogEndpoints(ctx, p)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
		if err != nil {
			return err
		}
		raw, err := secretText(secret, "routes.json")
		if err != nil {
			return err
		}
		var routes map[string]map[string]any
		if err = json.Unmarshal([]byte(raw), &routes); err != nil {
			return err
		}
		for _, endpoint := range endpoints {
			id := stringVal(endpoint["id"])
			route := routes[selector(id)]
			if route == nil || route["project_id"] != p.ProjectID || route["branch_id"] != p.BranchID {
				return errors.New("catalog route ownership mismatch")
			}
			if strings.Contains(action, "role") {
				roles, ok := route["roles"].(map[string]any)
				if !ok {
					return errors.New("catalog route credentials missing")
				}
				if action == "delete_role" {
					delete(roles, p.Name)
				} else {
					roles[p.Name] = verifier
				}
			}
		}
		encoded, _ := json.Marshal(routes)
		secret["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", routesSecret), secret)
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == 409 {
			continue
		}
		return err
	}
	return errors.New("catalog route update conflict")
}
func (s *server) reconcileCatalog(ctx context.Context, operation, worker, action string, p catalogPayload) error {
	if !validPGIdentifier(p.Name) || p.ProjectID == "" || p.BranchID == "" || p.EndpointID == "" {
		return errors.New("invalid catalog payload")
	}
	verifier := ""
	if p.CredentialRef != "" {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", p.CredentialRef), nil)
		if err != nil || !owned(secret, p.ProjectID, p.EndpointID) {
			return errors.New("catalog credential missing or unowned")
		}
		verifier, err = secretText(secret, "verifier")
		if err != nil {
			return err
		}
	}
	steps := []func(context.Context) error{
		func(ctx context.Context) error { return s.applyCatalogSQL(ctx, p, action, verifier) },
		func(ctx context.Context) error { return s.updateCatalogSpecs(ctx, p, action, verifier) },
		func(ctx context.Context) error { return s.updateCatalogRoutes(ctx, p, action, verifier) },
		func(ctx context.Context) error {
			if err := s.applyCatalogSQL(ctx, p, action, verifier); err != nil {
				return err
			}
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
				return err
			}
			state := "ready"
			if strings.HasPrefix(action, "delete_") {
				state = "deleted"
			}
			if strings.Contains(action, "role") {
				_, err = tx.Exec(ctx, `UPDATE branch_roles SET state=$3,credential_ref=NULLIF($4,''),version=version+1,updated_at=now() WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name, state, p.CredentialRef)
			} else {
				_, err = tx.Exec(ctx, `UPDATE branch_databases SET state=$3,version=version+1,updated_at=now() WHERE branch_id=$1 AND name=$2`, p.BranchID, p.Name, state)
			}
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		},
	}
	for ordinal, fn := range steps {
		if err := s.createStep(ctx, operation, worker, ordinal, fmt.Sprintf("catalog_step_%d", ordinal), fn); err != nil {
			return err
		}
	}
	return nil
}

// New writers/replicas and new data branches project the latest branch intent,
// including tombstones. They must not start with the old two-role template.
func (s *server) projectComputeCatalog(ctx context.Context, project, branch string) (string, error) {
	roles, err := s.many(ctx, `SELECT r.name,r.state,r.credential_ref FROM branch_roles r
        JOIN branches b ON b.id=r.branch_id WHERE r.branch_id=$1 AND b.project_id=$2`, branch, project)
	if err != nil {
		return "", err
	}
	databases, err := s.many(ctx, `SELECT d.name,d.owner_name,d.state FROM branch_databases d
        JOIN branches b ON b.id=d.branch_id WHERE d.branch_id=$1 AND b.project_id=$2`, branch, project)
	if err != nil {
		return "", err
	}
	catalog := map[string]any{"roles": []any{}, "databases": []any{}, "delta_operations": []any{}}
	for _, role := range roles {
		name := stringVal(role["name"])
		if role["state"] == "deleted" {
			catalog["delta_operations"] = append(catalog["delta_operations"].([]any), map[string]any{"action": "delete_role", "name": name, "new_name": nil})
			continue
		}
		if role["state"] != "ready" {
			return "", errors.New("catalog role operation pending")
		}
		ref := stringVal(role["credential_ref"])
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", ref), nil)
		if err != nil || nested(secret, "metadata", "labels", "neon-control/project-id") != project {
			return "", errors.New("catalog role credential unavailable or outside project")
		}
		verifier, err := secretText(secret, "verifier")
		if err != nil {
			return "", err
		}
		catalog["roles"] = append(catalog["roles"].([]any), map[string]any{"name": name, "encrypted_password": verifier, "options": nil})
	}
	for _, database := range databases {
		name := stringVal(database["name"])
		if database["state"] == "deleted" {
			catalog["delta_operations"] = append(catalog["delta_operations"].([]any), map[string]any{"action": "delete_db", "name": name, "new_name": nil})
			continue
		}
		if database["state"] != "ready" {
			return "", errors.New("catalog database operation pending")
		}
		catalog["databases"] = append(catalog["databases"].([]any), map[string]any{"name": name, "owner": database["owner_name"], "options": nil})
	}
	b, err := json.Marshal(catalog)
	return string(b), err
}

func (s *server) claimInheritedCatalog(ctx context.Context, p createPayload) error {
	var parent *string
	if err := s.db.QueryRow(ctx, "SELECT parent_branch_id FROM branches WHERE id=$1 AND project_id=$2", p.BranchID, p.ProjectID).Scan(&parent); err != nil {
		return err
	}
	if parent == nil {
		return nil
	}
	conn, err := s.catalogConnection(ctx, catalogPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID})
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	marker := "neon-control/branch/" + p.BranchID
	parentMarker := "neon-control/branch/" + *parent
	for _, kind := range []string{"role", "database"} {
		table, pgTable, pgName := "branch_roles", "pg_roles", "rolname"
		if kind == "database" {
			table = "branch_databases"
			pgTable = "pg_database"
			pgName = "datname"
		}
		names, err := s.many(ctx, "SELECT name FROM "+table+" WHERE branch_id=$1 AND state='ready'", p.BranchID)
		if err != nil {
			return err
		}
		for _, v := range names {
			object := "pg_authid"
			if kind == "database" {
				object = "pg_database"
			}
			var comment string
			if err = conn.QueryRow(ctx, "SELECT COALESCE(shobj_description(oid,'"+object+"'),'') FROM "+pgTable+" WHERE "+pgName+"=$1", v["name"]).Scan(&comment); err != nil {
				return err
			}
			if comment != marker && comment != parentMarker {
				return errors.New("inherited catalog ownership mismatch")
			}
			escaped, e := conn.PgConn().EscapeString(marker)
			if e != nil {
				return e
			}
			if _, err = conn.Exec(ctx, "COMMENT ON "+strings.ToUpper(kind)+" "+pgQuote(stringVal(v["name"]))+" IS '"+escaped+"'"); err != nil {
				return err
			}
		}
	}
	return nil
}
