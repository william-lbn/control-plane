package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type catalogBaselineInput struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	OID         uint32 `json:"oid"`
	Acknowledge bool   `json:"acknowledge_initial_observation"`
}

func (v catalogBaselineInput) valid() bool {
	return v.Acknowledge && v.OID > 0 && validPGIdentifier(v.Name) && (v.Kind == "role" && !protectedPGRole(v.Name) || v.Kind == "database" && !protectedPGDatabase(v.Name))
}

type catalogBaselineIntent struct {
	Scope            catalogNativeScope
	Version          int64
	State, Reference string
}
type catalogBaselineProof struct {
	Intent        catalogBaselineIntent
	Expected      catalogIdentityExpectation
	CredentialUID string
}

func readCatalogBaselineIntent(ctx context.Context, db connectionDefaultReader, project, branch, kind, name string) (catalogBaselineIntent, error) {
	out := catalogBaselineIntent{}
	if err := checkCatalogForkExclusion(ctx, db, project, branch, kind, name); err != nil {
		return out, err
	}
	table, column := "branch_databases", "owner_name"
	if kind == "role" {
		table, column = "branch_roles", "credential_ref"
	} else if kind != "database" {
		return out, errors.New("invalid catalog kind")
	}
	err := db.QueryRow(ctx, `SELECT p.tenant_id,b.timeline_id,c.version,c.state,COALESCE(c.`+column+`,'') FROM projects p JOIN branches b ON b.project_id=p.id JOIN `+table+` c ON c.branch_id=b.id WHERE p.id=$1 AND b.id=$2 AND c.name=$3`, project, branch, name).Scan(&out.Scope.TenantID, &out.Scope.TimelineID, &out.Version, &out.State, &out.Reference)
	return out, err
}

// A legacy name/marker cannot prove its history. The Admin acknowledges only
// the first trusted observation NOW. A second request cannot replace this OID.
func prepareCatalogBaseline(input catalogBaselineInput, intent catalogBaselineIntent, snapshot catalogNativeSnapshot, branch string) (catalogBaselineProof, error) {
	proof := catalogBaselineProof{Intent: intent}
	if !input.valid() || intent.State != "ready" || intent.Version < 1 || !intent.Scope.valid() || intent.Scope != snapshot.Scope || validateCatalogNativeSnapshot(snapshot) != nil {
		return proof, renameFault(409, "catalog_identity_conflict", "Ready managed intent and complete native branch observation required")
	}
	expected := catalogIdentityExpectation{Scope: intent.Scope, Kind: input.Kind, Name: input.Name, OID: input.OID}
	marker := "neon-control/branch/" + branch
	if input.Kind == "role" {
		canonical := catalogRoleFlags{Login: true, Inherit: true, CreateRole: true, CreateDB: true, Replication: true, BypassRLS: true, ConnectionLimit: -1}
		for _, v := range snapshot.Roles {
			if v.OID == input.OID && v.Name == input.Name && v.Marker == marker && v.Flags == canonical {
				expected.MarkerSHA256 = catalogMarkerDigest(v.Marker)
				expected.RoleFlags = v.Flags
			}
		}
	} else {
		for _, v := range snapshot.Databases {
			if v.OID == input.OID && v.Name == input.Name && v.Marker == marker && v.Flags == (catalogDatabaseFlags{AllowConnections: true, ConnectionLimit: -1}) {
				expected.MarkerSHA256 = catalogMarkerDigest(v.Marker)
				expected.DatabaseFlags = v.Flags
				expected.OwnerOID = v.OwnerOID
			}
		}
		for _, v := range snapshot.Roles {
			if v.OID == expected.OwnerOID {
				expected.OwnerName = v.Name
			}
		}
		if expected.OwnerName != intent.Reference || protectedPGRole(expected.OwnerName) {
			return proof, renameFault(409, "catalog_owner_conflict", "Original non-system managed database owner required")
		}
	}
	if expected.MarkerSHA256 == "" || compareCatalogNativeIdentity(expected, snapshot).Status != "catalog_matches" {
		return proof, renameFault(409, "catalog_identity_conflict", "Observed OID, marker or options differs from managed intent")
	}
	proof.Expected = expected
	return proof, nil
}

func readCatalogBaselineReceipt(ctx context.Context, db connectionDefaultReader, actor, key, hash string) (record, error) {
	var prior string
	var raw []byte
	err := db.QueryRow(ctx, `SELECT request_hash,response FROM catalog_identity_requests WHERE actor_id=$1 AND key_hash=$2`, actor, key).Scan(&prior, &raw)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if prior != hash {
		return nil, renameFault(409, "idempotency_conflict", "Original key belongs to a different request")
	}
	var value record
	err = json.Unmarshal(raw, &value)
	return value, err
}

func commitCatalogBaselineTx(ctx context.Context, tx pgx.Tx, project, branch, actor, request, key, hash string, input catalogBaselineInput, proof catalogBaselineProof) (record, error) {
	if !input.valid() || proof.Expected.Kind != input.Kind || proof.Expected.Name != input.Name || proof.Expected.OID != input.OID || proof.Expected.Scope != proof.Intent.Scope {
		return nil, errors.New("invalid baseline proof")
	}
	var organizationRole, grant string
	err := tx.QueryRow(ctx, `SELECT m.role,COALESCE(g.role,'') FROM projects p JOIN branches b ON b.project_id=p.id JOIN organizations o ON o.id=p.org_id
 JOIN users u ON u.id=$3 AND NOT u.disabled JOIN organization_members m ON m.org_id=o.id AND m.user_id=u.id LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id
 WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active' FOR UPDATE OF p,b`, project, branch, actor).Scan(&organizationRole, &grant)
	if err != nil && !isNoRows(err) {
		return nil, err
	}
	if isNoRows(err) || effectivePermission(organizationRole, grant) < 3 {
		return nil, renameFault(403, "forbidden", "Current project Admin required")
	}
	if prior, e := readCatalogBaselineReceipt(ctx, tx, actor, key, hash); e != nil || prior != nil {
		return prior, e
	}
	actual, err := readCatalogBaselineIntent(ctx, tx, project, branch, input.Kind, input.Name)
	if err != nil && !isNoRows(err) {
		return nil, err
	}
	if isNoRows(err) || actual != proof.Intent || actual.State != "ready" {
		return nil, renameFault(412, "catalog_intent_changed", "Catalog intent changed before initial observation committed")
	}
	var pending, exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2)),
 EXISTS(SELECT 1 FROM catalog_identity_baselines WHERE branch_id=$2 AND kind=$3 AND (name=$4 OR object_oid=$5)) OR EXISTS(SELECT 1 FROM catalog_object_bindings WHERE project_id=$1 AND branch_id=$2 AND kind=$3 AND (name=$4 OR object_oid=$5))`, project, branch, input.Kind, input.Name, int64(input.OID)).Scan(&pending, &exists)
	if err != nil {
		return nil, err
	}
	if pending {
		return nil, renameFault(409, "branch_operation_in_progress", "Another branch Operation is active")
	}
	if exists {
		return nil, renameFault(409, "catalog_baseline_exists", "Original native identity is already recorded and cannot be overwritten")
	}
	if input.Kind == "role" && (!functionResourceUID.MatchString(proof.CredentialUID) || actual.Reference == "") {
		return nil, errors.New("immutable managed credential UID required")
	}
	if input.Kind == "database" {
		var ownerReady bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=$1 AND name=$2 AND state='ready')`, branch, actual.Reference).Scan(&ownerReady)
		if err != nil {
			return nil, err
		}
		if !ownerReady {
			return nil, renameFault(409, "managed_owner_required", "Ready managed owner required")
		}
	}
	id := newID("cbi_")
	raw, err := json.Marshal(proof.Expected)
	if err != nil {
		return nil, err
	}
	credential := ""
	if input.Kind == "role" {
		credential = actual.Reference
	}
	var created string
	err = tx.QueryRow(ctx, `INSERT INTO catalog_identity_baselines(id,project_id,branch_id,kind,name,object_oid,intent_version,tenant_id,timeline_id,expectation,marker_sha256,credential_ref,credential_uid,captured_by)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING created_at::text`, id, project, branch, input.Kind, input.Name, int64(input.OID), actual.Version, actual.Scope.TenantID, actual.Scope.TimelineID, raw, proof.Expected.MarkerSHA256, credential, proof.CredentialUID, actor).Scan(&created)
	value := record{"binding_id": id, "kind": input.Kind, "expected_name": input.Name, "expected_oid": input.OID, "intent_version": strconv.FormatInt(actual.Version, 10), "basis": "admin_initial_observation", "captured_at": created, "credentials": "not_observed"}
	response, marshalErr := json.Marshal(value)
	if err == nil {
		err = marshalErr
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO catalog_identity_requests(actor_id,key_hash,request_hash,branch_id,response) VALUES($1,$2,$3,$4,$5)`, actor, key, hash, branch, response)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'initialize_catalog_identity','branch_catalog',$2,$3,'succeeded')`, actor, branch, request)
	}
	return value, err
}

func (s *server) observeCatalogInventory(ctx context.Context, project, branch string) (catalogNativeSnapshot, string, error) {
	var scope catalogNativeScope
	err := s.db.QueryRow(ctx, `SELECT p.tenant_id,b.timeline_id FROM projects p JOIN branches b ON b.project_id=p.id WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND b.state='ready' AND p.deleted_at IS NULL AND b.deleted_at IS NULL`, project, branch).Scan(&scope.TenantID, &scope.TimelineID)
	if err != nil {
		return catalogNativeSnapshot{}, "", err
	}
	endpoint, err := s.catalogWriter(ctx, project, branch)
	if err != nil {
		return catalogNativeSnapshot{}, "", err
	}
	release, err := s.acquireProbeGate(ctx, endpoint, true)
	if err != nil {
		return catalogNativeSnapshot{}, endpoint, err
	}
	defer release()
	conn, err := s.catalogConnection(ctx, catalogPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint})
	if err != nil {
		return catalogNativeSnapshot{}, endpoint, err
	}
	defer conn.Close(context.Background())
	snapshot, err := readCatalogNativeSnapshot(ctx, conn, scope)
	return snapshot, endpoint, err
}

func (s *server) catalogIdentities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	project, branch := r.PathValue("project"), r.PathValue("branch")
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	if r.Method == http.MethodGet {
		snapshot, endpoint, err := s.observeCatalogInventory(ctx, project, branch)
		if err != nil {
			s.catalogIdentityFailure(w, r, err)
			return
		}
		value, err := s.catalogIdentityReport(ctx, project, branch, snapshot)
		if err != nil {
			s.catalogIdentityFailure(w, r, err)
			return
		}
		value["endpoint_id"] = endpoint
		jsonResponse(w, 200, value)
		return
	}
	var input catalogBaselineInput
	if readJSON(r, &input) != nil || !input.valid() {
		fail(w, r, 422, "invalid_catalog_baseline", "Acknowledge the first observation of a non-system managed name and current native OID")
		return
	}
	header := r.Header.Get("If-Match")
	version, err := strconv.ParseInt(strings.Trim(header, `"`), 10, 64)
	if header == "" {
		fail(w, r, 428, "version_required", "Original quoted catalog intent version required")
		return
	}
	if err != nil || version < 1 || header != `"`+strconv.FormatInt(version, 10)+`"` {
		fail(w, r, 422, "invalid_version", "Canonical quoted positive version required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	actor := userFrom(r).ID
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, record{"input": input, "version": version})
	if prior, e := readCatalogBaselineReceipt(ctx, s.db, actor, keyHash(key), hash); e != nil {
		s.catalogIdentityFailure(w, r, e)
		return
	} else if prior != nil {
		jsonResponse(w, 200, prior)
		return
	}
	intent, err := readCatalogBaselineIntent(ctx, s.db, project, branch, input.Kind, input.Name)
	if err != nil && !isNoRows(err) {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	if isNoRows(err) || intent.State != "ready" {
		fail(w, r, 409, "managed_resource_required", "Ready API-managed catalog object required")
		return
	}
	if intent.Version != version {
		fail(w, r, 412, "version_conflict", "Catalog changed; refresh before initializing")
		return
	}
	var exists bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_identity_baselines WHERE branch_id=$1 AND kind=$2 AND (name=$3 OR object_oid=$4)) OR EXISTS(SELECT 1 FROM catalog_object_bindings WHERE branch_id=$1 AND kind=$2 AND (name=$3 OR object_oid=$4))`, branch, input.Kind, input.Name, int64(input.OID)).Scan(&exists)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	if exists {
		fail(w, r, 409, "catalog_baseline_exists", "Original identity cannot be overwritten")
		return
	}
	snapshot, endpoint, err := s.observeCatalogInventory(ctx, project, branch)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	proof, err := prepareCatalogBaseline(input, intent, snapshot, branch)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	if input.Kind == "role" {
		secret, e := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", intent.Reference), nil)
		if e != nil || !owned(secret, project, endpoint) || secret["immutable"] != true {
			s.catalogIdentityFailure(w, r, errors.New("original immutable catalog credential unavailable"))
			return
		}
		// Identity only: no customer password or SCRAM verifier is decoded.
		proof.CredentialUID = stringVal(nested(secret, "metadata", "uid"))
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	value, err := commitCatalogBaselineTx(ctx, tx, project, branch, actor, requestID(r), keyHash(key), hash, input, proof)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		if prior, e := readCatalogBaselineReceipt(ctx, s.db, actor, keyHash(key), hash); e == nil && prior != nil {
			jsonResponse(w, 200, prior)
			return
		}
		s.catalogIdentityFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, value)
}

func (s *server) catalogIdentityFailure(w http.ResponseWriter, r *http.Request, err error) {
	var fault *catalogRenameFault
	if errors.As(err, &fault) {
		fail(w, r, fault.Status, fault.Code, fault.Message)
		return
	}
	if errors.Is(err, errCatalogInventoryScope) {
		fail(w, r, 409, "catalog_scope_conflict", "Native tenant/timeline rejected")
		return
	}
	if errors.Is(err, errCatalogInventoryBound) {
		fail(w, r, 409, "catalog_inventory_limit", "Complete bounded inventory required; no partial result returned")
		return
	}
	fail(w, r, 503, "catalog_identity_unavailable", "Could not establish original catalog identity; retain the request key")
}

func (s *server) catalogIdentityReport(ctx context.Context, project, branch string, snapshot catalogNativeSnapshot) (record, error) {
	exclusions, e := readCatalogForkExclusions(ctx, s.db, project, branch)
	if e != nil {
		return nil, e
	}
	excludedNames := map[string]bool{}
	for _, v := range exclusions {
		excludedNames[v.Name] = true
	}
	allExcluded, e := readCatalogInheritanceExclusionNames(ctx, s.db, project, branch)
	if e != nil {
		return nil, e
	}
	for name := range excludedNames {
		allExcluded["role/"+name] = true
	}
	current, e := s.catalogCurrentIdentityReport(ctx, project, branch, snapshot, allExcluded)
	if e != nil {
		return nil, e
	}
	rows, err := s.db.Query(ctx, `SELECT id,kind,name,object_oid,intent_version,expectation,marker_sha256,created_at::text FROM catalog_identity_baselines WHERE project_id=$1 AND branch_id=$2 ORDER BY created_at,id LIMIT 1025`, project, branch)
	if err != nil {
		return nil, err
	}
	type baseline struct {
		id, kind, name, hash, created string
		oid                           uint32
		version                       int64
		expected                      catalogIdentityExpectation
	}
	baselines := []baseline{}
	for rows.Next() {
		var b baseline
		var raw []byte
		if err = rows.Scan(&b.id, &b.kind, &b.name, &b.oid, &b.version, &raw, &b.hash, &b.created); err != nil {
			rows.Close()
			return nil, err
		}
		if len(baselines) == catalogInventoryLimit {
			rows.Close()
			return nil, errCatalogInventoryBound
		}
		if err = json.Unmarshal(raw, &b.expected); err != nil {
			rows.Close()
			return nil, err
		}
		b.expected.MarkerSHA256 = b.hash
		baselines = append(baselines, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	differences := []record{}
	boundNames := map[string]bool{}
	boundOIDs := map[string]bool{}
	for _, b := range baselines {
		// Compare the immutable native record even if a later controlled operation
		// changed intent. That is not automatically labelled external DDL.
		diff := compareCatalogNativeIdentity(b.expected, snapshot)
		raw, _ := json.Marshal(diff)
		value := record{}
		_ = json.Unmarshal(raw, &value)
		value["binding_id"] = b.id
		value["intent_version"] = strconv.FormatInt(b.version, 10)
		value["captured_at"] = b.created
		value["basis"] = "admin_initial_observation"
		actual, e := readCatalogBaselineIntent(ctx, s.db, project, branch, b.kind, b.name)
		if e != nil && !isNoRows(e) {
			return nil, e
		}
		value["catalog_status"] = diff.Status
		if isNoRows(e) || actual.Version != b.version || actual.State != "ready" || actual.Scope != b.expected.Scope {
			value["status"] = "intent_changed"
		}
		differences = append(differences, value)
		boundNames[b.kind+":"+b.name] = true
		boundOIDs[b.kind+":"+strconv.FormatUint(uint64(b.oid), 10)] = true
	}
	native := []record{}
	for _, kind := range []string{"role", "database"} {
		table := "branch_roles"
		if kind == "database" {
			table = "branch_databases"
		}
		intents, e := s.many(ctx, "SELECT name,state,version FROM "+table+" WHERE branch_id=$1 AND state<>'deleted'", branch)
		if e != nil {
			return nil, e
		}
		byName := map[string]record{}
		for _, v := range intents {
			byName[stringVal(v["name"])] = v
		}
		appendObject := func(oid uint32, name string, protected bool, owner string) {
			value := record{"kind": kind, "name": name, "oid": oid, "protected": protected, "managed": false, "can_initialize": false}
			if owner != "" {
				value["owner"] = owner
			}
			if intent := byName[name]; intent != nil {
				value["managed"] = true
				value["intent_state"] = intent["state"]
				value["intent_version"] = fmt.Sprint(intent["version"])
				value["can_initialize"] = !protected && intent["state"] == "ready" && !boundNames[kind+":"+name] && !boundOIDs[kind+":"+strconv.FormatUint(uint64(oid), 10)]
			}
			key := catalogInventoryKey(kind, name)
			if current.KnownNames[key] || current.KnownOIDs[catalogInventoryKey(kind, strconv.FormatUint(uint64(oid), 10))] {
				value["can_initialize"] = false
			}
			if status := current.Status[key]; status != "" {
				value["identity_status"] = status
				if current.CurrentOIDs[key] != oid || status != "catalog_matches" {
					value["managed"] = false
				}
			}
			if allExcluded[kind+"/"+name] {
				value["managed"] = false
				value["can_initialize"] = false
				value["management_excluded"] = true
			}
			native = append(native, value)
		}
		if kind == "role" {
			for _, v := range snapshot.Roles {
				appendObject(v.OID, v.Name, protectedPGRole(v.Name), "")
			}
		} else {
			owners := map[uint32]string{}
			for _, v := range snapshot.Roles {
				owners[v.OID] = v.Name
			}
			for _, v := range snapshot.Databases {
				appendObject(v.OID, v.Name, protectedPGDatabase(v.Name), owners[v.OwnerOID])
			}
		}
	}
	return record{"branch_id": branch, "items": differences, "current_bindings": current.Items, "native_objects": native, "credentials": "not_observed", "complete": true, "limit": catalogInventoryLimit}, nil
}
