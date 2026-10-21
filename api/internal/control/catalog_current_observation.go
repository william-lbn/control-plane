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

type catalogCurrentObservationInput struct {
	Name        string `json:"name"`
	Acknowledge bool   `json:"acknowledge_current_observation"`
}
type catalogCurrentObservationProof struct {
	Baseline  string             `json:"baseline"`
	Kind      string             `json:"kind"`
	Name      string             `json:"name"`
	Scope     catalogNativeScope `json:"scope"`
	Version   int64              `json:"version"`
	Reference string             `json:"reference"`
	Object    catalogObjectRef   `json:"object"`
	Owner     *catalogObjectRef  `json:"owner,omitempty"`
}

func currentObservationNameValid(kind, name string) bool {
	return validPGIdentifier(name) && (kind == "role" && !protectedPGRole(name) || kind == "database" && !protectedPGDatabase(name))
}

func (s *server) currentObservationCredentialUID(ctx context.Context, project, branch, name string, intent catalogBaselineIntent, baseline catalogStoredBaseline) (string, error) {
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", intent.Reference), nil)
	if err != nil || secret["immutable"] != true || stringVal(nested(secret, "metadata", "labels", "neon-control/project-id")) != project {
		return "", errors.New("current immutable catalog credential unavailable")
	}
	uid := stringVal(nested(secret, "metadata", "uid"))
	if !functionResourceUID.MatchString(uid) {
		return "", errors.New("current catalog credential UID unavailable")
	}
	if err = verifyCurrentObservationCredential(ctx, s.db, project, branch, name, intent, baseline, uid); err != nil {
		return "", err
	}
	return uid, nil
}

func verifyCurrentObservationCredential(ctx context.Context, db connectionDefaultReader, project, branch, name string, intent catalogBaselineIntent, baseline catalogStoredBaseline, uid string) error {
	if intent.Reference == baseline.Intent.Reference {
		if uid != baseline.CredentialUID {
			return renameFault(409, "catalog_observation_credential_changed", "Original baseline credential UID changed")
		}
		return nil
	}
	// Old ordinary results do not prove OID history. They can prove only this
	// committed credential UID; the OID is observed separately against 049.
	var action, digest string
	var raw []byte
	err := db.QueryRow(ctx, `SELECT o.action,r.payload_sha256,o.payload FROM catalog_mutation_results r JOIN operations o ON o.id=r.operation_id
 WHERE r.project_id=$1 AND r.branch_id=$2 AND r.action='rotate_role_password' AND o.state='succeeded' AND o.action=r.action
 AND r.result->>'name'=$3 AND r.result->>'version_token'=$4 AND o.payload->>'project_id'=$1 AND o.payload->>'branch_id'=$2
 AND o.payload->>'name'=$3 AND o.payload->>'credential_ref'=$5 AND o.payload->>'mutation_credential_uid'=$6
 UNION ALL SELECT o.action,r.payload_sha256,o.payload FROM catalog_rename_results r JOIN operations o ON o.id=r.operation_id
 WHERE r.project_id=$1 AND r.branch_id=$2 AND r.kind='role' AND o.state='succeeded' AND o.action='rename_role' AND r.result->>'name'=$3
 AND r.result->>'version_token'=$4 AND r.result->>'object_oid'=$7 AND o.payload->>'project_id'=$1 AND o.payload->>'branch_id'=$2
 AND o.payload->>'new_name'=$3 AND o.payload->>'credential_ref'=$5 AND o.payload->>'credential_uid'=$6 LIMIT 1`, project, branch, name, strconv.FormatInt(intent.Version, 10), intent.Reference, uid, strconv.FormatUint(uint64(baseline.Expected.OID), 10)).Scan(&action, &digest, &raw)
	if err != nil && !isNoRows(err) {
		return err
	}
	trusted := false
	if err == nil && action == "rotate_role_password" {
		var p catalogPayload
		trusted = json.Unmarshal(raw, &p) == nil && catalogMutationDigest(action, p) == digest
	}
	if err == nil && action == "rename_role" {
		var p catalogRenamePayload
		trusted = json.Unmarshal(raw, &p) == nil && catalogRenameResultDigest(p) == digest
	}
	if !trusted {
		return renameFault(409, "catalog_observation_credential_unattested", "Current credential requires its original committed UID witness")
	}
	return nil
}

func verifyCurrentObservationName(ctx context.Context, db connectionDefaultReader, project, branch, name string, b catalogStoredBaseline) error {
	if name == b.Expected.Name {
		return nil
	}
	var trusted bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_rename_results r JOIN operations o ON o.id=r.operation_id WHERE r.project_id=$1 AND r.branch_id=$2 AND r.kind=$3 AND o.state='succeeded' AND o.action='rename_'||r.kind AND r.result->>'name'=$4 AND r.result->>'object_oid'=$5 AND o.payload->>'project_id'=$1 AND o.payload->>'branch_id'=$2 AND o.payload->>'new_name'=$4)`, project, branch, b.Expected.Kind, name, strconv.FormatUint(uint64(b.Expected.OID), 10)).Scan(&trusted)
	if err != nil {
		return err
	}
	if !trusted {
		return renameFault(409, "catalog_observation_name_unattested", "A changed managed name requires the original successful rename of this OID")
	}
	return nil
}

func prepareCurrentObservation(b catalogStoredBaseline, branch, name string, intent catalogBaselineIntent, uid string, owner *catalogObjectRef, snapshot catalogNativeSnapshot) (catalogCurrentObservationProof, error) {
	p := catalogCurrentObservationProof{Baseline: b.ID, Kind: b.Expected.Kind, Name: name, Scope: intent.Scope, Version: intent.Version, Reference: intent.Reference, Owner: owner, Object: catalogObjectRef{OID: b.Expected.OID, Version: intent.Version}}
	if !catalogBaselineID.MatchString(b.ID) || !currentObservationNameValid(p.Kind, name) || intent.State != "ready" || intent.Version < 1 || intent.Version < b.Intent.Version || b.Expected.Scope != intent.Scope || snapshot.Scope != intent.Scope {
		return p, renameFault(409, "catalog_observation_scope_changed", "Original OID scope and current ready intent required")
	}
	ownerName := ""
	if p.Kind == "role" {
		if !functionResourceUID.MatchString(uid) || intent.Reference == "" {
			return p, errors.New("current role credential UID required")
		}
		p.Object.CredentialRef, p.Object.CredentialUID = intent.Reference, uid
	} else {
		if owner == nil || owner.OID == 0 || owner.ID == "" || owner.Version < 1 {
			return p, catalogIdentityRequired()
		}
		p.Object.OwnerOID = owner.OID
		ownerName = intent.Reference
		ownerValid := false
		for _, role := range snapshot.Roles {
			if role.OID == owner.OID && role.Name == ownerName && !role.Flags.Superuser && role.Marker == "neon-control/branch/"+branch {
				ownerValid = true
			}
		}
		if !ownerValid {
			return p, renameFault(409, "catalog_owner_conflict", "Current original Owner identity and provenance required")
		}
	}
	if compareCatalogCurrentIdentity(intent.Scope, branch, p.Kind, name, ownerName, p.Object, snapshot).Status != "catalog_matches" {
		return p, renameFault(409, "catalog_observation_native_conflict", "Original OID, current name, Owner and provenance must match without SQL repair")
	}
	return p, nil
}

func commitCurrentObservationTx(ctx context.Context, tx pgx.Tx, project, branch, actor, request, key, hash string, p catalogCurrentObservationProof) (record, error) {
	var role, grant string
	err := tx.QueryRow(ctx, `SELECT m.role,COALESCE(g.role,'') FROM projects pr JOIN branches b ON b.project_id=pr.id JOIN organizations o ON o.id=pr.org_id JOIN users u ON u.id=$3 AND NOT u.disabled JOIN organization_members m ON m.org_id=o.id AND m.user_id=u.id LEFT JOIN project_grants g ON g.project_id=pr.id AND g.user_id=u.id WHERE pr.id=$1 AND b.id=$2 AND pr.state='ready' AND pr.source='managed' AND pr.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active' FOR UPDATE OF pr,b`, project, branch, actor).Scan(&role, &grant)
	if err != nil && !isNoRows(err) {
		return nil, err
	}
	if isNoRows(err) || effectivePermission(role, grant) < 3 {
		return nil, renameFault(403, "forbidden", "Current project Admin required")
	}
	if prior, e := readCatalogBaselineReceipt(ctx, tx, actor, key, hash); e != nil || prior != nil {
		return prior, e
	}
	b, err := readStoredCatalogBaseline(ctx, tx, project, branch, p.Baseline)
	if err != nil {
		return nil, err
	}
	intent, err := readCatalogBaselineIntent(ctx, tx, project, branch, p.Kind, p.Name)
	if err != nil {
		return nil, err
	}
	if intent.State != "ready" || intent.Version != p.Version || intent.Reference != p.Reference || intent.Scope != p.Scope || b.Expected.Scope != p.Scope || b.Expected.Kind != p.Kind || b.Expected.OID != p.Object.OID || p.Object.Version != p.Version || !currentObservationNameValid(p.Kind, p.Name) {
		return nil, renameFault(412, "catalog_intent_changed", "Current identity intent changed before observation committed")
	}
	if err = verifyCurrentObservationName(ctx, tx, project, branch, p.Name, b); err != nil {
		return nil, err
	}
	var pending, exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2)),EXISTS(SELECT 1 FROM catalog_object_bindings WHERE project_id=$1 AND branch_id=$2 AND kind=$3 AND (name=$4 OR object_oid=$5))`, project, branch, p.Kind, p.Name, int64(p.Object.OID)).Scan(&pending, &exists)
	if err != nil {
		return nil, err
	}
	if pending {
		return nil, renameFault(409, "branch_operation_in_progress", "Another branch Operation is active")
	}
	if exists {
		return nil, renameFault(409, "catalog_current_identity_exists", "A persistent identity already exists; it cannot be replaced by re-observation")
	}
	if p.Kind == "role" {
		if p.Object.CredentialRef != intent.Reference || !functionResourceUID.MatchString(p.Object.CredentialUID) {
			return nil, errors.New("current credential proof rejected")
		}
		if err = verifyCurrentObservationCredential(ctx, tx, project, branch, p.Name, intent, b, p.Object.CredentialUID); err != nil {
			return nil, err
		}
	} else {
		if p.Owner == nil || p.Owner.OID != p.Object.OwnerOID {
			return nil, errors.New("current Owner proof rejected")
		}
		var ownerVersion int64
		var ownerState, ownerReference string
		if err = tx.QueryRow(ctx, `SELECT version,state,COALESCE(credential_ref,'') FROM branch_roles WHERE branch_id=$1 AND name=$2 FOR UPDATE`, branch, intent.Reference).Scan(&ownerVersion, &ownerState, &ownerReference); err != nil {
			return nil, err
		}
		if ownerState != "ready" || ownerVersion != p.Owner.Version || ownerReference != p.Owner.CredentialRef {
			return nil, renameFault(412, "catalog_owner_changed", "Original Owner generation changed before observation committed")
		}
		if err = verifyCatalogObjectRef(ctx, tx, catalogPayload{ProjectID: project, BranchID: branch, NativeIdentity: &catalogMutationIdentity{Scope: p.Scope}}, "role", intent.Reference, *p.Owner); err != nil {
			return nil, err
		}
	}
	observationID := newID("cco_")
	p.Object.ID, p.Object.IdentityID = newID("cob_"), newID("coi_")
	if err = insertCatalogObjectBinding(ctx, tx, project, branch, p.Kind, p.Name, "ready", "current_observation", observationID, p.Scope, p.Object); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(p)
	if _, err = tx.Exec(ctx, `INSERT INTO catalog_current_observations(id,binding_id,baseline_id,project_id,branch_id,proof) VALUES($1,$2,$3,$4,$5,$6)`, observationID, p.Object.ID, p.Baseline, project, branch, raw); err != nil {
		return nil, err
	}
	response := record{"binding_id": p.Object.ID, "identity_id": p.Object.IdentityID, "source_binding_id": p.Baseline, "kind": p.Kind, "name": p.Name, "object_oid": p.Object.OID, "version_token": strconv.FormatInt(p.Version, 10), "basis": "explicit_current_observation", "credentials": "not_observed", "options": "not_recorded"}
	encoded, _ := json.Marshal(response)
	if _, err = tx.Exec(ctx, `INSERT INTO catalog_identity_requests(actor_id,key_hash,request_hash,branch_id,response) VALUES($1,$2,$3,$4,$5)`, actor, key, hash, branch, encoded); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'observe_current_catalog_identity','branch_catalog',$2,$3,'succeeded')`, actor, branch, request); err != nil {
		return nil, err
	}
	return response, nil
}

func (s *server) catalogObserveCurrent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if accessFrom(r).Level < 3 {
		fail(w, r, 403, "forbidden", "Project Admin required")
		return
	}
	project, branch, binding := r.PathValue("project"), r.PathValue("branch"), r.PathValue("binding")
	var input catalogCurrentObservationInput
	if !catalogBaselineID.MatchString(binding) || readJSON(r, &input) != nil || !input.Acknowledge || !validPGIdentifier(input.Name) {
		fail(w, r, 422, "invalid_current_observation", "Original baseline, current managed name and explicit observation acknowledgment required")
		return
	}
	header := r.Header.Get("If-Match")
	version, err := strconv.ParseInt(strings.Trim(header, `"`), 10, 64)
	if header == "" {
		fail(w, r, 428, "version_required", "Current quoted catalog intent version required")
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
	if prior, e := readCatalogBaselineReceipt(r.Context(), s.db, actor, keyHash(key), hash); e != nil {
		s.catalogIdentityFailure(w, r, e)
		return
	} else if prior != nil {
		jsonResponse(w, 200, prior)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	b, err := readStoredCatalogBaseline(ctx, s.db, project, branch, binding)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	intent, err := readCatalogBaselineIntent(ctx, s.db, project, branch, b.Expected.Kind, input.Name)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	if intent.Version != version {
		fail(w, r, 412, "version_conflict", "Refresh current intent before observation")
		return
	}
	if err = verifyCurrentObservationName(ctx, s.db, project, branch, input.Name, b); err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	uid := ""
	var owner *catalogObjectRef
	if b.Expected.Kind == "role" {
		uid, err = s.currentObservationCredentialUID(ctx, project, branch, input.Name, intent, b)
	} else {
		tx, e := s.db.Begin(ctx)
		if e != nil {
			s.catalogIdentityFailure(w, r, e)
			return
		}
		defer tx.Rollback(ctx)
		// A database observes an already-attested Owner. No Owner head is seeded
		// as an incidental effect of preparing this observation.
		var ref catalogObjectRef
		err = tx.QueryRow(ctx, `SELECT id,identity_id,object_oid,intent_version,credential_ref,credential_uid,owner_oid FROM catalog_object_bindings WHERE project_id=$1 AND branch_id=$2 AND kind='role' AND name=$3 AND state='ready' AND intent_version=(SELECT version FROM branch_roles WHERE branch_id=$2 AND name=$3 AND state='ready') AND tenant_id=$4 AND timeline_id=$5`, project, branch, intent.Reference, intent.Scope.TenantID, intent.Scope.TimelineID).Scan(&ref.ID, &ref.IdentityID, &ref.OID, &ref.Version, &ref.CredentialRef, &ref.CredentialUID, &ref.OwnerOID)
		tx.Rollback(ctx)
		owner = &ref
	}
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	snapshot, _, err := s.observeCatalogInventory(ctx, project, branch)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	p, err := prepareCurrentObservation(b, branch, input.Name, intent, uid, owner, snapshot)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	response, err := commitCurrentObservationTx(ctx, tx, project, branch, actor, requestID(r), keyHash(key), hash, p)
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
	jsonResponse(w, 200, response)
}
