package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const observationProject = "prj_0123456789abcdef"
const observationBranch = "br_0123456789abcdef"
const observationUID = "11111111-1111-4111-8111-111111111111"
const observationNextUID = "22222222-2222-4222-8222-222222222222"

func inheritedBaselineFixture() (catalogBaselineInput, catalogBaselineIntent, createPayload, catalogInheritanceSource, catalogInheritanceObservation) {
	scope := bindingPayload("customer", 1).NativeIdentity.Scope
	input := catalogBaselineInput{Kind: "role", Name: "customer", OID: 41, Acknowledge: true}
	intent := catalogBaselineIntent{Scope: scope, State: "ready", Version: 1, Reference: "parent-original"}
	p := createPayload{ProjectID: observationProject, BranchID: observationBranch, EndpointID: "ep_0123456789abcdef", TenantID: scope.TenantID, TimelineID: scope.TimelineID, CatalogPolicy: catalogPreserveInheritedSQL}
	source := catalogInheritanceSource{Operation: "inherited_original", Project: p.ProjectID, Branch: p.BranchID, Endpoint: p.EndpointID, SourceBranch: "br_fedcba9876543210", Scope: scope, SourceScope: catalogNativeScope{scope.TenantID, strings.Repeat("c", 32)}, PayloadSHA256: catalogInheritancePayloadDigest(p), Items: []catalogInheritanceItem{{Kind: "role", Name: input.Name, Version: 7, CredentialRef: intent.Reference, CredentialUID: observationUID}}}
	observed := catalogInheritanceObservation{Operation: source.Operation, Project: p.ProjectID, Branch: p.BranchID, Endpoint: p.EndpointID, Scope: scope, SourceSHA256: catalogInheritanceSourceDigest(source), Objects: []catalogInheritanceObject{{Kind: "role", Name: input.Name, OID: input.OID, Status: "legacy_unattested"}}}
	return input, intent, p, source, observed
}

func TestCatalogBaselineInheritanceRequiresOriginalPublishedUIDAndOID(t *testing.T) {
	input, intent, p, source, observed := inheritedBaselineFixture()
	if e := verifyCatalogBaselineInheritance(p.ProjectID, p.BranchID, input, intent, observationUID, p, source, observed); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*catalogBaselineInput, *catalogBaselineIntent, *createPayload, *catalogInheritanceSource, *catalogInheritanceObservation){
		func(i *catalogBaselineInput, _ *catalogBaselineIntent, _ *createPayload, _ *catalogInheritanceSource, _ *catalogInheritanceObservation) {
			i.OID++
		},
		func(_ *catalogBaselineInput, i *catalogBaselineIntent, _ *createPayload, _ *catalogInheritanceSource, _ *catalogInheritanceObservation) {
			i.Version++
		},
		func(_ *catalogBaselineInput, i *catalogBaselineIntent, _ *createPayload, _ *catalogInheritanceSource, _ *catalogInheritanceObservation) {
			i.Reference = "successor"
		},
		func(_ *catalogBaselineInput, _ *catalogBaselineIntent, p *createPayload, _ *catalogInheritanceSource, _ *catalogInheritanceObservation) {
			p.EndpointType = "read_only"
		},
		func(_ *catalogBaselineInput, _ *catalogBaselineIntent, _ *createPayload, s *catalogInheritanceSource, _ *catalogInheritanceObservation) {
			s.SourceBranch = s.Branch
		},
		func(_ *catalogBaselineInput, _ *catalogBaselineIntent, _ *createPayload, s *catalogInheritanceSource, _ *catalogInheritanceObservation) {
			s.Items[0].CredentialUID = observationNextUID
		},
		func(_ *catalogBaselineInput, _ *catalogBaselineIntent, _ *createPayload, s *catalogInheritanceSource, _ *catalogInheritanceObservation) {
			s.Items[0].External = true
		},
		func(_ *catalogBaselineInput, _ *catalogBaselineIntent, _ *createPayload, s *catalogInheritanceSource, _ *catalogInheritanceObservation) {
			s.Items[0].Binding = &catalogObjectRef{OID: 41}
		},
		func(_ *catalogBaselineInput, _ *catalogBaselineIntent, _ *createPayload, _ *catalogInheritanceSource, o *catalogInheritanceObservation) {
			o.SourceSHA256 = "changed"
		},
		func(_ *catalogBaselineInput, _ *catalogBaselineIntent, _ *createPayload, _ *catalogInheritanceSource, o *catalogInheritanceObservation) {
			o.Objects[0].Status = "external_identity"
		},
	} {
		i, v, q, s, o := inheritedBaselineFixture()
		change(&i, &v, &q, &s, &o)
		if e := verifyCatalogBaselineInheritance(p.ProjectID, p.BranchID, i, v, observationUID, q, s, o); e == nil {
			t.Fatal("changed original inheritance accepted")
		}
	}
}

func TestCatalogBaselineInheritanceMetadataOnlySecretCannotReplaceOriginalWitness(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	input, intent, p, source, observed := inheritedBaselineFixture()
	uid, endpoint := observationUID, "ep_fedcba9876543210"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/secrets/parent-original") {
			t.Error("unexpected credential request")
		}
		json.NewEncoder(w).Encode(record{"immutable": true, "metadata": record{"uid": uid, "labels": record{"neon-control/project-id": p.ProjectID, "neon-control/endpoint-id": endpoint}}})
	}))
	defer peer.Close()
	s.kube = &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	read := func() error {
		_, e := s.catalogBaselineCredentialUID(ctx, p.ProjectID, p.BranchID, p.EndpointID, input, intent)
		return e
	}
	if e := read(); e == nil {
		t.Fatal("parent-owned credential accepted without publication")
	}
	raw, _ := json.Marshal(p)
	proof, _ := json.Marshal(source)
	obs, _ := json.Marshal(observed)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'branch',$3,'create_branch','succeeded','binding_admin',$1,$4)`, source.Operation, p.ProjectID, p.BranchID, raw)
	exec(`INSERT INTO catalog_inheritance_sources(operation_id,project_id,branch_id,proof) VALUES($1,$2,$3,$4)`, source.Operation, p.ProjectID, p.BranchID, proof)
	exec(`INSERT INTO catalog_inheritance_observations(endpoint_id,operation_id,project_id,branch_id,proof) VALUES($1,$2,$3,$4,$5)`, p.EndpointID, source.Operation, p.ProjectID, p.BranchID, obs)
	exec(`INSERT INTO catalog_inheritance_publications(endpoint_id) VALUES($1)`, p.EndpointID)
	if e := read(); e == nil {
		t.Fatal("publication without original SQL accepted")
	}
	exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,0,'sql_probe_via_proxy','succeeded')`, source.Operation)
	if e := read(); e != nil {
		t.Fatal("published parent credential rejected", e)
	}
	uid, endpoint = observationNextUID, p.EndpointID
	if e := read(); e == nil {
		t.Fatal("successor with copied current endpoint labels accepted")
	}
	var count int
	if e := s.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_identity_baselines)+(SELECT count(*) FROM catalog_object_bindings)`).Scan(&count); e != nil || count != 0 {
		t.Fatal("credential read wrote catalog identity", e)
	}
}

func observationSnapshot() catalogNativeSnapshot {
	_, snapshot := inventoryFixture()
	snapshot.Scope = bindingPayload("customer", 1).NativeIdentity.Scope
	snapshot.Roles[0].Marker = "neon-control/branch/" + observationBranch
	snapshot.Databases[0].Marker = snapshot.Roles[0].Marker
	snapshot.Roles[0].Flags = catalogRoleFlags{Login: true, Inherit: true, CreateRole: true, CreateDB: true, Replication: true, BypassRLS: true, ConnectionLimit: -1}
	return snapshot
}

func TestCatalogCurrentObservationNativeScopeOIDOwnerAndUnknownOptions(t *testing.T) {
	snapshot := observationSnapshot()
	intent := catalogBaselineIntent{Scope: snapshot.Scope, State: "ready", Version: 2, Reference: "next-credential"}
	b := catalogStoredBaseline{ID: "cbi_111111111111111111111111", Intent: catalogBaselineIntent{Scope: snapshot.Scope, Version: 1, Reference: "original"}, Expected: catalogIdentityExpectation{Scope: snapshot.Scope, Kind: "role", Name: "customer", OID: 41}, CredentialUID: observationUID}
	snapshot.Roles[0].Flags.ConnectionLimit = 7
	if _, e := prepareCurrentObservation(b, observationBranch, "customer", intent, observationNextUID, nil, snapshot); e != nil {
		t.Fatal("unknown historical options blocked current OID observation", e)
	}
	for _, change := range []func(*catalogNativeSnapshot){
		func(s *catalogNativeSnapshot) { s.Scope.TimelineID = strings.Repeat("c", 32) },
		func(s *catalogNativeSnapshot) { s.Roles[0].OID = 43; s.Databases[0].OwnerOID = 43 },
		func(s *catalogNativeSnapshot) { s.Roles[0].Name = "external_rename" },
		func(s *catalogNativeSnapshot) { s.Roles[0].Marker = "copied" },
		func(s *catalogNativeSnapshot) { s.Roles[0].Flags.Superuser = true },
	} {
		v := observationSnapshot()
		change(&v)
		if _, e := prepareCurrentObservation(b, observationBranch, "customer", intent, observationNextUID, nil, v); e == nil {
			t.Fatal("conflicting current observation accepted")
		}
	}
	b.Expected.Kind, b.Expected.Name, b.Expected.OID = "database", "customer_db", 51
	intent.Reference = "customer"
	owner := catalogObjectRef{ID: "owner", IdentityID: "identity", OID: 41, Version: 2, CredentialRef: "next-credential", CredentialUID: observationNextUID}
	if _, e := prepareCurrentObservation(b, observationBranch, "customer_db", intent, "", &owner, snapshot); e != nil {
		t.Fatal(e)
	}
	owner.OID = 42
	if _, e := prepareCurrentObservation(b, observationBranch, "customer_db", intent, "", &owner, snapshot); e == nil {
		t.Fatal("unrelated current Owner accepted")
	}
}

func TestCatalogCurrentObservationAtomicBridgeReplayAndOriginalCredentialReceipt(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	snapshot := observationSnapshot()
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,'customer','ready','original',1)`, observationBranch)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state,version) VALUES($1,'customer_db','customer','ready',1)`, observationBranch)
	baseline := func(kind, name string, oid uint32) catalogStoredBaseline {
		t.Helper()
		intent, e := readCatalogBaselineIntent(ctx, s.db, observationProject, observationBranch, kind, name)
		if e != nil {
			t.Fatal(e)
		}
		input := catalogBaselineInput{Kind: kind, Name: name, OID: oid, Acknowledge: true}
		proof, e := prepareCatalogBaseline(input, intent, snapshot, observationBranch)
		if e != nil {
			t.Fatal(e)
		}
		if kind == "role" {
			proof.CredentialUID = observationUID
		}
		tx, e := s.db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		receipt, e := commitCatalogBaselineTx(ctx, tx, observationProject, observationBranch, "binding_admin", "initial-"+kind, "initial-"+kind, "initial-hash-"+kind, input, proof)
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		value, e := readStoredCatalogBaseline(ctx, s.db, observationProject, observationBranch, receipt["binding_id"].(string))
		if e != nil {
			t.Fatal(e)
		}
		return value
	}
	roleBaseline, dbBaseline := baseline("role", "customer", 41), baseline("database", "customer_db", 51)
	exec(`UPDATE branch_roles SET version=2,credential_ref='next-credential' WHERE branch_id=$1 AND name='customer'`, observationBranch)
	intent, e := readCatalogBaselineIntent(ctx, s.db, observationProject, observationBranch, "role", "customer")
	if e != nil {
		t.Fatal(e)
	}
	if e = verifyCurrentObservationCredential(ctx, s.db, observationProject, observationBranch, "customer", intent, roleBaseline, observationNextUID); e == nil {
		t.Fatal("unwitnessed newer credential accepted")
	}
	p := bindingPayload("customer", 1)
	p.CredentialRef = "next-credential"
	p.MutationCredentialUID = observationNextUID
	raw, _ := json.Marshal(p)
	res, _ := json.Marshal(record{"name": "customer", "version_token": "2"})
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('old_rotate',$1,'branch_catalog',$2,'rotate_role_password','succeeded','binding_admin','old-rotate',$3)`, observationProject, observationBranch, raw)
	exec(`INSERT INTO catalog_mutation_results(operation_id,project_id,branch_id,action,payload_sha256,result) VALUES('old_rotate',$1,$2,'rotate_role_password',$3,$4)`, observationProject, observationBranch, catalogMutationDigest("rotate_role_password", p), res)
	if e = verifyCurrentObservationCredential(ctx, s.db, observationProject, observationBranch, "customer", intent, roleBaseline, observationNextUID); e != nil {
		t.Fatal(e)
	}
	if e = verifyCurrentObservationCredential(ctx, s.db, observationProject, observationBranch, "customer", intent, roleBaseline, observationUID); e == nil {
		t.Fatal("same-name successor credential UID accepted")
	}
	proof, e := prepareCurrentObservation(roleBaseline, observationBranch, "customer", intent, observationNextUID, nil, snapshot)
	if e != nil {
		t.Fatal(e)
	}
	commit := func(actor, key, hash string, value catalogCurrentObservationProof) (record, error) {
		tx, e := s.db.Begin(ctx)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback(ctx)
		out, e := commitCurrentObservationTx(ctx, tx, observationProject, observationBranch, actor, "observe-"+key, key, hash, value)
		if e == nil {
			e = tx.Commit(ctx)
		}
		return out, e
	}
	fault := func(e error, status int) {
		t.Helper()
		var v *catalogRenameFault
		if !errors.As(e, &v) || v.Status != status {
			t.Fatalf("expected %d, got %v", status, e)
		}
	}
	exec(`UPDATE users SET disabled=true WHERE id='binding_admin'`)
	_, e = commit("binding_admin", "bridge", "hash", proof)
	fault(e, 403)
	exec(`UPDATE users SET disabled=false WHERE id='binding_admin'`)
	stale := proof
	stale.Version++
	_, e = commit("binding_admin", "stale", "hash", stale)
	fault(e, 412)
	exec(`CREATE FUNCTION reject_current_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_current_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_current_audit()`)
	if _, e = commit("binding_admin", "bridge", "hash", proof); e == nil {
		t.Fatal("audit failure ignored")
	}
	var count int
	if e = s.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_object_bindings)+(SELECT count(*) FROM catalog_current_observations)`).Scan(&count); e != nil || count != 0 {
		t.Fatal("partial current identity survived rollback", e)
	}
	exec(`DROP TRIGGER reject_current_audit ON audit_events`)
	first, e := commit("binding_admin", "bridge", "hash", proof)
	if e != nil {
		t.Fatal(e)
	}
	if first["version_token"] != "2" || first["options"] != "not_recorded" {
		t.Fatal("incorrect bridge response")
	}
	_, e = commit("binding_admin", "other", "hash", proof)
	fault(e, 409)
	_, e = commit("binding_admin", "bridge", "different", proof)
	fault(e, 409)
	owner := proof.Object
	owner.ID = first["binding_id"].(string)
	owner.IdentityID = first["identity_id"].(string)
	dbIntent, e := readCatalogBaselineIntent(ctx, s.db, observationProject, observationBranch, "database", "customer_db")
	if e != nil {
		t.Fatal(e)
	}
	database, e := prepareCurrentObservation(dbBaseline, observationBranch, "customer_db", dbIntent, "", &owner, snapshot)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = commit("binding_admin", "database", "db-hash", database); e != nil {
		t.Fatal(e)
	}
	exec(`UPDATE branch_roles SET version=3,credential_ref='later' WHERE branch_id=$1 AND name='customer'`, observationBranch)
	replay, e := commit("binding_admin", "bridge", "hash", proof)
	if e != nil || replay["binding_id"] != first["binding_id"] || replay["version_token"] != "2" {
		t.Fatal("original request replay used current generation", e)
	}
	var baselines, observations, bindings, audits int
	if e = s.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_identity_baselines WHERE intent_version=1),(SELECT count(*) FROM catalog_current_observations),(SELECT count(*) FROM catalog_object_bindings),(SELECT count(*) FROM audit_events WHERE action='observe_current_catalog_identity')`).Scan(&baselines, &observations, &bindings, &audits); e != nil || baselines != 2 || observations != 2 || bindings != 2 || audits != 2 {
		t.Fatal("original baseline or unique receipt changed", e)
	}
	if _, e = s.db.Exec(ctx, `UPDATE catalog_current_observations SET proof='{}'`); e == nil {
		t.Fatal("immutable observation rewritten")
	}
	// A historical 053 result proves its UID only, never a fabricated native OID.
	var result map[string]any
	json.Unmarshal(res, &result)
	if _, ok := result["object_oid"]; ok {
		t.Fatal("fixture invented an old OID witness")
	}
}
