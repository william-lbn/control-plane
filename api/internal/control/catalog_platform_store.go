package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *server) observeCatalogPlatform(ctx context.Context, project, branch string) (catalogPlatformSource, catalogNativeSnapshot, error) {
	var source catalogPlatformSource
	source.Project, source.Branch = project, branch
	err := s.db.QueryRow(ctx, `SELECT e.id,e.role_name,e.database_name,p.tenant_id,b.timeline_id FROM endpoints e JOIN projects p ON p.id=e.project_id JOIN branches b ON b.id=e.branch_id AND b.project_id=p.id WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND b.state='ready' AND e.state='active' AND p.deleted_at IS NULL AND b.deleted_at IS NULL AND e.deleted_at IS NULL AND e.endpoint_type='read_write'`, project, branch).Scan(&source.Endpoint, &source.Role, &source.Database, &source.Scope.TenantID, &source.Scope.TimelineID)
	if err != nil {
		return catalogPlatformSource{}, catalogNativeSnapshot{}, err
	}
	p := createPayload{ProjectID: project, BranchID: branch, EndpointID: source.Endpoint, EndpointType: "read_write", TenantID: source.Scope.TenantID, TimelineID: source.Scope.TimelineID}
	backing, password, err := s.kube.readCatalogPlatformBootstrap(ctx, p)
	if err != nil {
		return catalogPlatformSource{}, catalogNativeSnapshot{}, err
	}
	source.CredentialRef = kubeName(source.Endpoint) + "-credentials"
	source.CredentialUID = backing.CredentialUID
	source.ConfigUID = backing.ConfigUID
	release, err := s.acquireProbeGate(ctx, source.Endpoint, true)
	if err != nil {
		return catalogPlatformSource{}, catalogNativeSnapshot{}, err
	}
	defer release()
	// The fixed maintenance database remains usable when the original default
	// database has been externally renamed. Never recreate that default name.
	conn, err := connectSQL(ctx, s.proxyHost, s.proxyPort, "control_probe", password, "template1", selector(source.Endpoint))
	if err != nil {
		return catalogPlatformSource{}, catalogNativeSnapshot{}, err
	}
	defer conn.Close(context.Background())
	snapshot, err := readCatalogNativeSnapshot(ctx, conn, source.Scope)
	if err != nil {
		return catalogPlatformSource{}, snapshot, err
	}
	if !source.valid() {
		return source, snapshot, errCatalogPlatformIdentity
	}
	return source, snapshot, nil
}

func (k *kubeClient) readCatalogPlatformBootstrap(ctx context.Context, p createPayload) (computeCatalogBootstrap, string, error) {
	// Bootstrap returns the original config Secret, then its decoded document.
	// Both original credential/config bindings have already been validated there.
	config, _, backing, err := k.readComputeCatalogBootstrap(ctx, p)
	if err != nil {
		return backing, "", err
	}
	password, err := secretText(config, "probePassword")
	return backing, password, err
}

func readCatalogPlatformBaseline(ctx context.Context, q connectionDefaultReader, source catalogPlatformSource) (string, catalogPlatformObservation, string, error) {
	var id, digest, created string
	var raw []byte
	var value catalogPlatformObservation
	err := q.QueryRow(ctx, `SELECT id,observation,observation_sha256,created_at::text FROM catalog_platform_identity_baselines WHERE project_id=$1 AND branch_id=$2 AND tenant_id=$3 AND timeline_id=$4`, source.Project, source.Branch, source.Scope.TenantID, source.Scope.TimelineID).Scan(&id, &raw, &digest, &created)
	if err != nil {
		return "", value, "", err
	}
	if json.Unmarshal(raw, &value) != nil || value.Source.Project != source.Project || value.Source.Branch != source.Branch || value.Source.Scope != source.Scope {
		return "", value, "", errCatalogPlatformIdentity
	}
	actual, err := value.witness()
	if err != nil || actual != digest {
		return "", value, "", errCatalogPlatformIdentity
	}
	return id, value, created, nil
}

func commitCatalogPlatformBaseline(ctx context.Context, tx pgx.Tx, actor, request, key, hash string, observation catalogPlatformObservation) (record, error) {
	source := observation.Source
	witness, err := observation.witness()
	if err != nil {
		return nil, err
	}
	var orgRole, projectRole, endpoint, role, database, tenant, timeline string
	err = tx.QueryRow(ctx, `SELECT m.role,COALESCE(g.role,''),e.id,e.role_name,e.database_name,p.tenant_id,b.timeline_id FROM projects p JOIN branches b ON b.project_id=p.id JOIN endpoints e ON e.branch_id=b.id AND e.project_id=p.id JOIN organizations o ON o.id=p.org_id JOIN users u ON u.id=$3 AND NOT u.disabled JOIN organization_members m ON m.org_id=o.id AND m.user_id=u.id LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id WHERE p.id=$1 AND b.id=$2 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND e.state='active' AND e.deleted_at IS NULL AND e.endpoint_type='read_write' AND o.state='active' FOR UPDATE OF p,b,e`, source.Project, source.Branch, actor).Scan(&orgRole, &projectRole, &endpoint, &role, &database, &tenant, &timeline)
	if err != nil && !isNoRows(err) {
		return nil, err
	}
	if isNoRows(err) || effectivePermission(orgRole, projectRole) < 3 {
		return nil, renameFault(403, "forbidden", "Current project Admin required")
	}
	if prior, e := readCatalogBaselineReceipt(ctx, tx, actor, key, hash); e != nil || prior != nil {
		return prior, e
	}
	if endpoint != source.Endpoint || role != source.Role || database != source.Database || tenant != source.Scope.TenantID || timeline != source.Scope.TimelineID {
		return nil, renameFault(412, "catalog_platform_source_changed", "Inspect the current original platform source before recording")
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, source.Project, source.Branch).Scan(&pending); err != nil {
		return nil, err
	}
	if pending {
		return nil, renameFault(409, "branch_operation_in_progress", "Retain the active original branch Operation")
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_platform_identity_baselines WHERE branch_id=$1 AND tenant_id=$2 AND timeline_id=$3)`, source.Branch, source.Scope.TenantID, source.Scope.TimelineID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, renameFault(409, "catalog_platform_baseline_exists", "Original platform identity cannot be replaced by another first observation")
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		return nil, err
	}
	id := newID("cps_")
	_, err = tx.Exec(ctx, `INSERT INTO catalog_platform_identity_baselines(id,project_id,branch_id,tenant_id,timeline_id,endpoint_id,credential_ref,credential_uid,config_uid,observation,observation_sha256,captured_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, source.Project, source.Branch, tenant, timeline, endpoint, source.CredentialRef, source.CredentialUID, source.ConfigUID, encoded, witness, actor)
	if err != nil {
		return nil, err
	}
	value := record{"baseline_id": id, "branch_id": source.Branch, "basis": "admin_initial_platform_observation", "object_count": len(observation.Objects), "credentials": "not_observed", "protected_object_mutation": false}
	raw, _ := json.Marshal(value)
	if _, err = tx.Exec(ctx, `INSERT INTO catalog_identity_requests(actor_id,key_hash,request_hash,branch_id,response) VALUES($1,$2,$3,$4,$5)`, actor, key, hash, source.Branch, raw); err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) VALUES($1,'initialize_catalog_platform_identity','branch_catalog',$2,$3,'succeeded')`, actor, source.Branch, request)
	return value, err
}

func (s *server) catalogPlatformIdentities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if accessFrom(r).Level < 2 || r.Method == http.MethodPost && accessFrom(r).Level < 3 {
		fail(w, r, 403, "forbidden", "Project Editor for observation or Admin for explicit initial recording required")
		return
	}
	project, branch := r.PathValue("project"), r.PathValue("branch")
	var input struct {
		Acknowledge bool   `json:"acknowledge_initial_observation"`
		Witness     string `json:"observation_sha256"`
	}
	var key, hash string
	if r.Method == http.MethodPost {
		if readJSON(r, &input) != nil || !input.Acknowledge || !budgetEvidence.MatchString(input.Witness) {
			fail(w, r, 422, "invalid_catalog_platform_observation", "Acknowledge an explicit complete current platform observation")
			return
		}
		var ok bool
		key, ok = creationKey(w, r)
		if !ok {
			return
		}
		key = keyHash(key)
		hash = s.createRequestHash(r.Method+":"+r.URL.Path, input)
		if prior, err := readCatalogBaselineReceipt(r.Context(), s.db, userFrom(r).ID, key, hash); err != nil {
			s.catalogIdentityFailure(w, r, err)
			return
		} else if prior != nil {
			jsonResponse(w, 200, prior)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	source, snapshot, err := s.observeCatalogPlatform(ctx, project, branch)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	if r.Method == http.MethodGet {
		id, baseline, created, readErr := readCatalogPlatformBaseline(ctx, s.db, source)
		if readErr == nil {
			items, e := compareCatalogPlatform(baseline, snapshot)
			if e != nil {
				s.catalogIdentityFailure(w, r, e)
				return
			}
			jsonResponse(w, 200, record{"branch_id": branch, "baseline_id": id, "recorded": true, "captured_at": created, "items": items, "credentials": "not_observed", "basis": "admin_initial_platform_observation", "source_endpoint_changed": baseline.Source.Endpoint != source.Endpoint, "source_backings_changed": baseline.Source.CredentialUID != source.CredentialUID || baseline.Source.ConfigUID != source.ConfigUID, "platform_intent_changed": baseline.Source.Role != source.Role || baseline.Source.Database != source.Database, "protected_object_mutation": false})
			return
		}
		if !isNoRows(readErr) {
			s.catalogIdentityFailure(w, r, readErr)
			return
		}
	}
	observation, err := captureCatalogPlatform(source, snapshot)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	witness, err := observation.witness()
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	if r.Method == http.MethodGet {
		candidates := []record{}
		for _, object := range observation.Objects {
			v := object.Expected
			candidates = append(candidates, record{"kind": v.Kind, "name": v.Name, "oid": v.OID, "owner": v.OwnerName, "owner_oid": v.OwnerOID})
		}
		jsonResponse(w, 200, record{"branch_id": branch, "recorded": false, "observation_sha256": witness, "object_count": len(observation.Objects), "observed_objects": candidates, "items": []catalogIdentityDifference{}, "credentials": "not_observed", "basis": "no_original_platform_observation"})
		return
	}
	if witness != input.Witness {
		fail(w, r, 412, "catalog_platform_observation_changed", "Reinspect the current complete platform identity before recording")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.catalogIdentityFailure(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	value, err := commitCatalogPlatformBaseline(ctx, tx, userFrom(r).ID, requestID(r), key, hash, observation)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		tx.Rollback(ctx)
		if prior, e := readCatalogBaselineReceipt(ctx, s.db, userFrom(r).ID, key, hash); e == nil && prior != nil {
			jsonResponse(w, 200, prior)
			return
		}
		if errors.Is(err, errCatalogPlatformIdentity) {
			fail(w, r, 409, "catalog_platform_identity_unavailable", "Retain the original platform identity; no automatic repair is performed")
			return
		}
		s.catalogIdentityFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, value)
}
