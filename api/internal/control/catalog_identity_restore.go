package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

var catalogBaselineID = regexp.MustCompile(`^cbi_[a-f0-9]{24}$`)

type catalogStoredBaseline struct {
	ID            string
	Intent        catalogBaselineIntent
	Expected      catalogIdentityExpectation
	CredentialUID string
}

func readStoredCatalogBaseline(ctx context.Context, db connectionDefaultReader, project, branch, id string) (catalogStoredBaseline, error) {
	b := catalogStoredBaseline{ID: id}
	var kind, name, hash string
	var oid uint32
	var raw []byte
	err := db.QueryRow(ctx, `SELECT kind,name,object_oid,intent_version,tenant_id,timeline_id,expectation,marker_sha256,credential_ref,credential_uid FROM catalog_identity_baselines WHERE id=$1 AND project_id=$2 AND branch_id=$3`, id, project, branch).Scan(&kind, &name, &oid, &b.Intent.Version, &b.Intent.Scope.TenantID, &b.Intent.Scope.TimelineID, &raw, &hash, &b.Intent.Reference, &b.CredentialUID)
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(raw, &b.Expected); err != nil {
		return b, err
	}
	b.Expected.MarkerSHA256 = hash
	b.Intent.State = "ready"
	if b.Expected.Kind != kind || b.Expected.Name != name || b.Expected.OID != oid || b.Expected.Scope != b.Intent.Scope || !b.Intent.Scope.valid() {
		return b, errors.New("immutable catalog baseline rejected")
	}
	if kind == "database" {
		b.Intent.Reference = b.Expected.OwnerName
	}
	return b, nil
}

type catalogIdentityRestoreInput struct {
	Acknowledge     bool `json:"acknowledge_restore"`
	ResetCredential bool `json:"acknowledge_role_credential_reset"`
}
type catalogIdentityRestorePayload struct {
	catalogRenamePayload
	BindingID       string             `json:"binding_id"`
	Scope           catalogNativeScope `json:"native_scope"`
	ResetCredential bool               `json:"reset_original_credential"`
}

func (p catalogIdentityRestorePayload) validRestore() bool {
	return catalogBaselineID.MatchString(p.BindingID) && p.Scope.valid() && p.valid() && p.bindingsValid() && (p.Kind == "role" && p.ResetCredential || p.Kind == "database" && !p.ResetCredential)
}
func prepareCatalogIdentityRestore(b catalogStoredBaseline, snapshot catalogNativeSnapshot, input catalogIdentityRestoreInput) (catalogRenamePayload, error) {
	p := catalogRenamePayload{Kind: b.Expected.Kind, NewName: b.Expected.Name, ObjectOID: b.Expected.OID, catalogPayload: catalogPayload{ExpectedVersion: b.Intent.Version}}
	if !input.Acknowledge || input.ResetCredential != (p.Kind == "role") {
		return p, renameFault(422, "restore_acknowledgement_required", "Acknowledge original name restoration and, for a role, resetting its original managed credential")
	}
	diff := compareCatalogNativeIdentity(b.Expected, snapshot)
	if diff.Status != "renamed" || len(diff.Changes) != 1 || diff.Changes[0] != "name" {
		return p, renameFault(409, "catalog_restore_conflict", "Only an original OID with its name changed and all other observed fields unchanged can be restored")
	}
	p.Name = diff.ObservedName
	if p.Kind == "role" {
		p.CredentialRef = b.Intent.Reference
	} else {
		p.Owner = b.Expected.OwnerName
	}
	if !validPGIdentifier(p.Name) || (p.Kind == "role" && protectedPGRole(p.Name)) || (p.Kind == "database" && protectedPGDatabase(p.Name)) {
		return p, renameFault(409, "catalog_restore_name_rejected", "Observed original name is outside the supported non-system managed restore scope")
	}
	return p, nil
}
func catalogRestoreMatchesBaseline(p catalogIdentityRestorePayload, b catalogStoredBaseline) bool {
	return p.validRestore() && p.BindingID == b.ID && p.Kind == b.Expected.Kind && p.NewName == b.Expected.Name && p.ObjectOID == b.Expected.OID && p.ExpectedVersion == b.Intent.Version && p.Scope == b.Expected.Scope && (p.Kind == "database" && p.Owner == b.Expected.OwnerName || p.Kind == "role" && p.CredentialRef == b.Intent.Reference && p.CredentialUID == b.CredentialUID)
}
func verifyCatalogRestoreIntent(ctx context.Context, db connectionDefaultReader, p catalogIdentityRestorePayload) (catalogStoredBaseline, error) {
	b, err := readStoredCatalogBaseline(ctx, db, p.ProjectID, p.BranchID, p.BindingID)
	if err != nil {
		return b, err
	}
	if !catalogRestoreMatchesBaseline(p, b) {
		return b, renameFault(409, "catalog_restore_identity_changed", "Original baseline, OID, credential UID or scope changed")
	}
	if err = verifyCatalogRestoreParents(ctx, db, p, false); err != nil {
		return b, err
	}
	actual, err := readCatalogBaselineIntent(ctx, db, p.ProjectID, p.BranchID, p.Kind, p.NewName)
	if err != nil && !isNoRows(err) {
		return b, err
	}
	if isNoRows(err) || actual != b.Intent {
		return b, renameFault(412, "catalog_intent_changed", "Original managed intent changed before restoration")
	}
	return b, nil
}

func verifyCatalogRestoreParents(ctx context.Context, db connectionDefaultReader, p catalogIdentityRestorePayload, lock bool) error {
	query := `SELECT p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active'
 AND e.state='active' AND e.deleted_at IS NULL AND e.endpoint_type='read_write'
 FROM projects p JOIN branches b ON b.project_id=p.id JOIN organizations o ON o.id=p.org_id
 JOIN endpoints e ON e.project_id=p.id AND e.branch_id=b.id WHERE p.id=$1 AND b.id=$2 AND e.id=$3`
	if lock {
		query += ` FOR UPDATE OF p,b`
	}
	var eligible bool
	err := db.QueryRow(ctx, query, p.ProjectID, p.BranchID, p.EndpointID).Scan(&eligible)
	if err != nil && !isNoRows(err) {
		return err
	}
	if isNoRows(err) || !eligible {
		return renameFault(409, "catalog_restore_parent_changed", "Original ready project, branch and Writer required")
	}
	return nil
}

func (s *server) catalogIdentityRestore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	project, branch, binding := r.PathValue("project"), r.PathValue("branch"), r.PathValue("binding")
	var input catalogIdentityRestoreInput
	if !catalogBaselineID.MatchString(binding) || readJSON(r, &input) != nil || !input.Acknowledge {
		fail(w, r, 422, "invalid_catalog_restore", "Original identity and explicit restoration acknowledgment required")
		return
	}
	header := r.Header.Get("If-Match")
	version, err := strconv.ParseInt(strings.Trim(header, `"`), 10, 64)
	if header == "" {
		fail(w, r, 428, "version_required", "Original quoted managed intent version required")
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
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, record{"input": input, "version": version})
	if s.replayCatalog(w, r, key, hash) {
		return
	}
	b, err := readStoredCatalogBaseline(r.Context(), s.db, project, branch, binding)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Original catalog identity not found")
		return
	}
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	if version != b.Intent.Version {
		fail(w, r, 412, "version_conflict", "Original identity baseline version required")
		return
	}
	if input.ResetCredential != (b.Expected.Kind == "role") {
		fail(w, r, 422, "restore_acknowledgement_required", "Explicit original role credential reset acknowledgment required only for a role")
		return
	}
	actual, err := readCatalogBaselineIntent(r.Context(), s.db, project, branch, b.Expected.Kind, b.Expected.Name)
	if err != nil && !isNoRows(err) {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	if isNoRows(err) || actual != b.Intent {
		fail(w, r, 412, "catalog_intent_changed", "Managed intent no longer matches the original baseline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	snapshot, endpoint, err := s.observeCatalogInventory(ctx, project, branch)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	rename, err := prepareCatalogIdentityRestore(b, snapshot, input)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	rename.ProjectID = project
	rename.BranchID = branch
	rename.EndpointID = endpoint
	if err = s.captureCatalogRenameBindings(ctx, &rename); err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	p := catalogIdentityRestorePayload{catalogRenamePayload: rename, BindingID: binding, Scope: b.Expected.Scope, ResetCredential: input.ResetCredential}
	if !catalogRestoreMatchesBaseline(p, b) {
		s.catalogIdentityFailure(w, r, renameFault(409, "catalog_restore_identity_changed", "Original immutable credential identity rejected"))
		return
	}
	verifier, err := s.catalogRenameVerifier(ctx, rename)
	if err == nil {
		err = s.verifyCatalogRestoreBackings(ctx, p, verifier)
	}
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
	operation, err := admitCatalogIdentityRestoreTx(ctx, tx, p, userFrom(r).ID, requestID(r), keyHash(key), hash)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		if s.replayCatalog(w, r, key, hash) {
			return
		}
		s.catalogIdentityFailure(w, r, err)
		return
	}
	s.acceptCatalog(w, r, operation)
}

func admitCatalogIdentityRestoreTx(ctx context.Context, tx pgx.Tx, p catalogIdentityRestorePayload, actor, request, key, hash string) (string, error) {
	if !p.validRestore() {
		return "", errors.New("invalid original identity restore payload")
	}
	var organizationRole, grant string
	err := tx.QueryRow(ctx, `SELECT m.role,COALESCE(g.role,'') FROM projects p JOIN branches b ON b.project_id=p.id JOIN organizations o ON o.id=p.org_id
 JOIN users u ON u.id=$3 AND NOT u.disabled JOIN organization_members m ON m.org_id=o.id AND m.user_id=u.id LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id
 WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active' FOR UPDATE OF p,b`, p.ProjectID, p.BranchID, actor).Scan(&organizationRole, &grant)
	if err != nil && !isNoRows(err) {
		return "", err
	}
	if isNoRows(err) || effectivePermission(organizationRole, grant) < 3 {
		return "", renameFault(403, "forbidden", "Current project Admin required")
	}
	if _, err = verifyCatalogRestoreIntent(ctx, tx, p); err != nil {
		return "", err
	}
	var pending bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, p.ProjectID, p.BranchID).Scan(&pending)
	if err != nil {
		return "", err
	}
	if pending {
		return "", renameFault(409, "branch_operation_in_progress", "Another branch Operation is active")
	}
	if err = verifyCatalogRenameEndpointSet(ctx, tx, p.catalogRenamePayload); err != nil {
		return "", err
	}
	operation := newID("op_")
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'branch_catalog',$3,'restore_catalog_identity','queued',$4,$5,$6)`, operation, p.ProjectID, p.BranchID, actor, request, raw)
	for ordinal, name := range []string{"verify_original_immutable_backings", "restore_original_native_oid", "verify_retained_cold_backings", "confirm_original_identity_result"} {
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, operation, ordinal, name)
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, actor, key, hash, operation)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'restore_catalog_identity','branch_catalog',$2,$3,'accepted')`, actor, p.BranchID, request)
	}
	return operation, err
}

// Restoring a native name requires the retained cold specs and Proxy already
// to express the original desired name. Never silently rewrite drifted stores.
func (s *server) verifyCatalogRestoreBackings(ctx context.Context, p catalogIdentityRestorePayload, verifier string) error {
	if !p.validRestore() {
		return errors.New("invalid restore backings")
	}
	for id, uid := range p.ConfigUIDs {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(id)+"-config"), nil)
		if err != nil || !owned(secret, p.ProjectID, id) || stringVal(nested(secret, "metadata", "uid")) != uid {
			return errors.New("original restore config UID rejected")
		}
		raw, err := secretText(secret, "config.json")
		if err != nil {
			return err
		}
		var config map[string]any
		if json.Unmarshal([]byte(raw), &config) != nil {
			return errors.New("restore config invalid")
		}
		before, _ := json.Marshal(config)
		if err = catalogRenameDelta(config, p.catalogRenamePayload, verifier); err != nil {
			return err
		}
		after, _ := json.Marshal(config)
		if string(before) != string(after) {
			return errors.New("retained config differs from original restore intent")
		}
	}
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
	if err != nil || stringVal(nested(secret, "metadata", "uid")) != p.RoutesUID {
		return errors.New("original restore routes UID rejected")
	}
	raw, err := secretText(secret, "routes.json")
	if err != nil {
		return err
	}
	var routes map[string]map[string]any
	if json.Unmarshal([]byte(raw), &routes) != nil {
		return errors.New("restore routes invalid")
	}
	before, _ := json.Marshal(routes)
	if err = renameCatalogRouteMap(routes, p.catalogRenamePayload, verifier); err != nil {
		return err
	}
	after, _ := json.Marshal(routes)
	if string(before) != string(after) {
		return errors.New("retained routes differ from original restore intent")
	}
	return nil
}

func applyCatalogIdentityRestoreSQL(ctx context.Context, conn *pgx.Conn, p catalogIdentityRestorePayload, expected catalogIdentityExpectation, verifier string) error {
	if !p.validRestore() || p.Scope != expected.Scope || p.Kind != expected.Kind || p.NewName != expected.Name || p.ObjectOID != expected.OID {
		return errors.New("restore native identity scope rejected")
	}
	snapshot, err := readCatalogNativeSnapshot(ctx, conn, p.Scope)
	if err != nil {
		return err
	}
	diff := compareCatalogNativeIdentity(expected, snapshot)
	if diff.Status != "catalog_matches" && (diff.Status != "renamed" || diff.ObservedName != p.Name || len(diff.Changes) != 1 || diff.Changes[0] != "name") {
		return errors.New("original native identity cannot be restored under current conflicts")
	}
	if err = applyCatalogRenameSQL(ctx, conn, p.catalogRenamePayload, verifier); err != nil {
		return err
	}
	snapshot, err = readCatalogNativeSnapshot(ctx, conn, p.Scope)
	if err != nil {
		return err
	}
	if compareCatalogNativeIdentity(expected, snapshot).Status != "catalog_matches" {
		return errors.New("original native restore postcondition rejected")
	}
	return nil
}

func catalogIdentityRestoreRecorded(ctx context.Context, db connectionDefaultReader, operation string, p catalogIdentityRestorePayload) (bool, error) {
	var binding string
	err := db.QueryRow(ctx, `SELECT binding_id FROM catalog_identity_restore_results WHERE operation_id=$1`, operation).Scan(&binding)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if binding != p.BindingID {
		return false, errors.New("original restore result identity rejected")
	}
	return true, nil
}
func commitCatalogIdentityRestoreResultTx(ctx context.Context, tx pgx.Tx, operation, worker string, p catalogIdentityRestorePayload) error {
	if err := verifyCatalogRestoreParents(ctx, tx, p, true); err != nil {
		return err
	}
	if err := assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	if done, err := catalogIdentityRestoreRecorded(ctx, tx, operation, p); err != nil || done {
		return err
	}
	if _, err := verifyCatalogRestoreIntent(ctx, tx, p); err != nil {
		return err
	}
	if err := verifyCatalogRenameEndpointSet(ctx, tx, p.catalogRenamePayload); err != nil {
		return err
	}
	raw, _ := json.Marshal(record{"binding_id": p.BindingID, "kind": p.Kind, "restored_name": p.NewName, "original_oid": p.ObjectOID, "intent_version": strconv.FormatInt(p.ExpectedVersion, 10), "original_role_credential_reset": p.ResetCredential})
	_, err := tx.Exec(ctx, `INSERT INTO catalog_identity_restore_results(operation_id,binding_id,result) VALUES($1,$2,$3)`, operation, p.BindingID, raw)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) SELECT actor_id,'restore_catalog_identity','branch_catalog',resource_id,request_id,'succeeded' FROM operations WHERE id=$1`, operation)
	}
	return err
}
func (s *server) recoverRecordedCatalogRestore(ctx context.Context, operation, worker string, p catalogIdentityRestorePayload) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	done, err := catalogIdentityRestoreRecorded(ctx, tx, operation, p)
	if err != nil {
		return err
	}
	if !done {
		return errors.New("original restore result missing during recovery")
	}
	_, err = tx.Exec(ctx, `UPDATE operation_steps SET state='succeeded',detail=NULL,updated_at=now() WHERE operation_id=$1`, operation)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *server) reconcileCatalogIdentityRestore(ctx context.Context, operation, worker string, p catalogIdentityRestorePayload) error {
	if !p.validRestore() {
		return errors.New("invalid restore Operation payload")
	}
	// A committed original result must not apply old credentials again after a
	// later external DDL. Recover the original success, not a new repair request.
	if done, err := catalogIdentityRestoreRecorded(ctx, s.db, operation, p); err != nil {
		return err
	} else if done {
		return s.recoverRecordedCatalogRestore(ctx, operation, worker, p)
	}
	b, err := verifyCatalogRestoreIntent(ctx, s.db, p)
	if err != nil {
		return err
	}
	verifier, err := s.catalogRenameVerifier(ctx, p.catalogRenamePayload)
	if err != nil {
		return err
	}
	apply := func(ctx context.Context) error {
		if _, err := verifyCatalogRestoreIntent(ctx, s.db, p); err != nil {
			return err
		}
		release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
		if err != nil {
			return err
		}
		defer release()
		conn, err := s.catalogConnection(ctx, p.catalogPayload)
		if err != nil {
			return err
		}
		defer conn.Close(context.Background())
		return applyCatalogIdentityRestoreSQL(ctx, conn, p, b.Expected, verifier)
	}
	steps := []func(context.Context) error{
		func(ctx context.Context) error { return s.verifyCatalogRestoreBackings(ctx, p, verifier) }, apply,
		func(ctx context.Context) error { return s.verifyCatalogRestoreBackings(ctx, p, verifier) },
		func(ctx context.Context) error {
			if err := apply(ctx); err != nil {
				return err
			}
			if err := s.verifyCatalogRestoreBackings(ctx, p, verifier); err != nil {
				return err
			}
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			err = commitCatalogIdentityRestoreResultTx(ctx, tx, operation, worker, p)
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		},
	}
	for ordinal, step := range steps {
		if err = s.createStep(ctx, operation, worker, ordinal, "catalog_identity_restore", step); err != nil {
			return err
		}
	}
	return nil
}
