package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestCatalogInheritanceInitializationSQLUsesOriginalProbeWitness(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	p := createPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("b", 32), CatalogPolicy: catalogPreserveInheritedSQL}
	operation := "op_555555555555555555555555"
	raw, _ := json.Marshal(p)
	// Exercise source-capture trust independently of VM creation; this fixture
	// has an existing Writer row but initially no original SQL success witness.
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'branch',$3,'create_branch','queued','binding_admin',$1,$4)`, operation, p.ProjectID, p.BranchID, raw)
	tx, e := s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.captureCatalogInheritanceTx(ctx, tx, operation, p, p.BranchID, false); e == nil {
		t.Fatal("unattested prior Writer caused password initialization")
	}
	tx.Rollback(ctx)
	original := p
	original.EndpointID = "ep_0123456789abcdef"
	original.CatalogPolicy = ""
	raw, _ = json.Marshal(original)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('original_initializer',$1,'project',$1,'create_project','succeeded','binding_admin','original-initializer',$2)`, p.ProjectID, raw)
	exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES('original_initializer',0,'sql_probe_via_proxy','succeeded')`)
	tx, e = s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.captureCatalogInheritanceTx(ctx, tx, operation, p, p.BranchID, false); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	source, e := readCatalogInheritanceSource(ctx, s.db, operation, p)
	if e != nil || source.InitializePlatform || source.PlatformWitness != "published_or_original_successful_branch_sql" {
		t.Fatal("original successful SQL did not preserve branch platform identity", e)
	}
}

func TestCatalogInheritanceNativeCommentsRetainPasswordOwnerAndOriginalOID(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, e := pgx.ParseConfig(dsn)
	if dsn == "" || e != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	conn, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	suffix := randomToken(5)
	role, external, database := "inherit_role_"+suffix, "inherit_external_"+suffix, "inherit_db_"+suffix
	scope := catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("c", 32)}
	source := catalogInheritanceSource{Scope: scope, SourceScope: catalogNativeScope{scope.TenantID, strings.Repeat("b", 32)}, Branch: "br_fedcba9876543210", SourceBranch: "br_0123456789abcdef"}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	verifier, e := scramVerifier("ci-original-native-inheritance-password")
	if e != nil {
		t.Fatal(e)
	}
	exec(`SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, scope.TenantID, scope.TimelineID)
	for _, name := range []string{role, external} {
		exec("CREATE ROLE " + pgQuote(name) + " NOLOGIN CONNECTION LIMIT 7 PASSWORD " + pgLiteral(verifier))
		exec("COMMENT ON ROLE " + pgQuote(name) + " IS " + pgLiteral("neon-control/branch/"+source.SourceBranch))
	}
	exec("CREATE DATABASE " + pgQuote(database) + " OWNER " + pgQuote(role) + " CONNECTION LIMIT 17")
	exec("COMMENT ON DATABASE " + pgQuote(database) + " IS " + pgLiteral("neon-control/branch/"+source.SourceBranch))
	defer func() {
		conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgQuote(database))
		for _, name := range []string{role, role + "_retained", external} {
			conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgQuote(name))
		}
	}()
	var roleOID, dbOID, externalOID uint32
	conn.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname=$1`, role).Scan(&roleOID)
	conn.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname=$1`, external).Scan(&externalOID)
	conn.QueryRow(ctx, `SELECT oid FROM pg_database WHERE datname=$1`, database).Scan(&dbOID)
	source.Items = []catalogInheritanceItem{{Kind: "role", Name: role, Version: 1, Binding: &catalogObjectRef{OID: roleOID, Version: 2}}, {Kind: "database", Name: database, Owner: role, Version: 1, Binding: &catalogObjectRef{OID: dbOID, OwnerOID: roleOID, Version: 2}}, {Kind: "role", Name: external, Version: 1, External: true, ExpectedOID: externalOID - 1, ObservedOID: externalOID}}
	snapshot, e := readCatalogNativeSnapshot(ctx, conn, scope)
	if e != nil {
		t.Fatal(e)
	}
	objects, e := observeCatalogInheritance(source, snapshot)
	if e != nil {
		t.Fatal(e)
	}
	if e = classifyCatalogInheritancePrivileges(ctx, conn, source, objects); e != nil {
		t.Fatal(e)
	}
	if e = claimCatalogInheritanceSQL(ctx, conn, source, objects); e != nil {
		t.Fatal(e)
	}
	if e = claimCatalogInheritanceSQL(ctx, conn, source, objects); e != nil {
		t.Fatal("original comment replay failed", e)
	}
	var password, marker string
	var login bool
	var limit int
	if e = conn.QueryRow(ctx, `SELECT rolpassword,rolcanlogin,rolconnlimit,shobj_description(oid,'pg_authid') FROM pg_authid WHERE oid=$1`, roleOID).Scan(&password, &login, &limit, &marker); e != nil || password != verifier || login || limit != 7 || marker != "neon-control/branch/"+source.Branch {
		t.Fatal("inheritance comment changed role credentials/options", e)
	}
	var owner uint32
	if e = conn.QueryRow(ctx, `SELECT datdba,datconnlimit,shobj_description(oid,'pg_database') FROM pg_database WHERE oid=$1`, dbOID).Scan(&owner, &limit, &marker); e != nil || owner != roleOID || limit != 17 || marker != "neon-control/branch/"+source.Branch {
		t.Fatal("inheritance comment changed native Owner/options", e)
	}
	conn.QueryRow(ctx, `SELECT shobj_description(oid,'pg_authid') FROM pg_roles WHERE oid=$1`, externalOID).Scan(&marker)
	if marker != "neon-control/branch/"+source.SourceBranch {
		t.Fatal("excluded SQL object was tagged")
	}
	exec("ALTER ROLE " + pgQuote(role) + " RENAME TO " + pgQuote(role+"_retained"))
	exec("CREATE ROLE " + pgQuote(role) + " NOLOGIN PASSWORD " + pgLiteral(verifier))
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS " + pgLiteral("neon-control/branch/"+source.Branch))
	if e = claimCatalogInheritanceSQL(ctx, conn, source, objects); e == nil {
		t.Fatal("retry adopted a same-name replacement despite the copied marker/password")
	}
}

func TestCatalogInheritanceAdmissionSnapshotSurvivesParentChangesAndNoCompute(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	parent := "br_0123456789abcdef"
	child := "br_fedcba9876543210"
	operation := "op_333333333333333333333333"
	p := createPayload{ProjectID: "prj_0123456789abcdef", BranchID: child, TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("c", 32), ParentTimelineID: strings.Repeat("b", 32), ParentLSN: "0/1234", CatalogPolicy: catalogPreserveInheritedSQL}
	ref := catalogObjectRef{ID: "admission_parent_role", IdentityID: "admission_parent_identity", OID: 1000, Version: 2, CredentialRef: "admission-parent-role", CredentialUID: "11111111-1111-4111-8111-111111111111"}
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,'admission_role','ready',$2,2)`, parent, ref.CredentialRef)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state,version) VALUES($1,'postgres','cloud_admin','ready',1)`, parent)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,state,created_at) VALUES($1,$2,'data-only',$3,$4,'creating',now())`, child, p.ProjectID, p.TimelineID, parent)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,'admission_role','ready',$2,1)`, child, ref.CredentialRef)
	raw, _ := json.Marshal(p)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'branch',$3,'create_branch','queued','binding_admin',$1,$4)`, operation, p.ProjectID, child, raw)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]any{"immutable": true, "metadata": map[string]any{"uid": ref.CredentialUID, "labels": credentialLabels(p.ProjectID, "ep_0123456789abcdef")}})
	}))
	defer peer.Close()
	s.kube = &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	tx, e := s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = insertCatalogObjectBinding(ctx, tx, p.ProjectID, parent, "role", "admission_role", "ready", "initial_observation", "admission-initial", catalogNativeScope{p.TenantID, p.ParentTimelineID}, ref); e != nil {
		t.Fatal(e)
	}
	if e = s.captureCatalogInheritanceTx(ctx, tx, operation, p, parent, false); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	source, e := readCatalogInheritanceSource(ctx, s.db, operation, p)
	if e != nil || len(source.Items) != 1 || source.Items[0].Binding == nil || *source.Items[0].Binding != ref || !source.InitializePlatform {
		t.Fatal("original admission snapshot lost", e)
	}
	exec(`UPDATE branch_roles SET version=99,credential_ref='later-parent-credential' WHERE branch_id=$1 AND name='admission_role'`, parent)
	exec(`UPDATE branches SET state='ready' WHERE id=$1`, child)
	exec(`UPDATE operations SET state='succeeded' WHERE id=$1`, operation)
	endpoint := "ep_fedcba9876543210"
	next := p
	next.EndpointID = endpoint
	next.ParentTimelineID = ""
	next.ParentLSN = ""
	raw, _ = json.Marshal(next)
	nextOperation := "op_444444444444444444444444"
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'provisioning','cloud_admin','postgres','read_write',now(),now())`, endpoint, p.ProjectID, child)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'endpoint',$3,'create_endpoint','queued','binding_admin',$1,$4)`, nextOperation, p.ProjectID, endpoint, raw)
	tx, e = s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.captureCatalogInheritanceTx(ctx, tx, nextOperation, next, child, false); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	frozen, e := readCatalogInheritanceSource(ctx, s.db, nextOperation, next)
	if e != nil || frozen.SourceBranch != parent || frozen.SourceScope != source.SourceScope || frozen.ParentLSN != source.ParentLSN || len(frozen.Items) != 1 || *frozen.Items[0].Binding != ref || !frozen.InitializePlatform {
		t.Fatal("first Writer borrowed today's parent generation", e)
	}
}

func TestCatalogInheritanceAtomicHeadExclusionAuditAndFrozenSource(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	parent := "br_0123456789abcdef"
	child := "br_fedcba9876543210"
	endpoint := "ep_fedcba9876543210"
	operation := "op_111111111111111111111111"
	worker := "inheritance_worker"
	p := createPayload{ProjectID: "prj_0123456789abcdef", BranchID: child, EndpointID: endpoint, EndpointType: "read_write", TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("c", 32), ParentTimelineID: strings.Repeat("b", 32), ParentLSN: "0/1234", CatalogPolicy: catalogPreserveInheritedSQL}
	sourceScope := catalogNativeScope{p.TenantID, p.ParentTimelineID}
	known := catalogObjectRef{ID: "parent_role_binding", IdentityID: "parent_role_identity", OID: 1000, Version: 2, CredentialRef: "original-parent-role", CredentialUID: "11111111-1111-4111-8111-111111111111"}
	dbref := catalogObjectRef{ID: "parent_db_binding", IdentityID: "parent_db_identity", OID: 2000, Version: 2, OwnerOID: 1000}
	replaced := catalogObjectRef{ID: "parent_replaced_binding", IdentityID: "parent_replaced_identity", OID: 3000, Version: 2, CredentialRef: "original-replaced-role", CredentialUID: "22222222-2222-4222-8222-222222222222"}
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,'owned_role','ready',$2,2),($1,'replaced_role','ready',$3,2),($1,'legacy_role','ready','legacy-credential',1)`, parent, known.CredentialRef, replaced.CredentialRef)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state,version) VALUES($1,'owned_database','owned_role','ready',2)`, parent)
	tx, e := s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range []struct {
		kind, name string
		ref        catalogObjectRef
	}{{"role", "owned_role", known}, {"database", "owned_database", dbref}, {"role", "replaced_role", replaced}} {
		if e = insertCatalogObjectBinding(ctx, tx, p.ProjectID, parent, v.kind, v.name, "ready", "initial_observation", "initial-witness", sourceScope, v.ref); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,state,created_at) VALUES($1,$2,'child',$3,$4,'creating',now())`, child, p.ProjectID, p.TimelineID, parent)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'provisioning','cloud_admin','postgres','read_write',now(),now())`, endpoint, p.ProjectID, child)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) SELECT $1,name,'ready',credential_ref FROM branch_roles WHERE branch_id=$2`, child, parent)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) SELECT $1,name,owner_name,'ready' FROM branch_databases WHERE branch_id=$2`, child, parent)
	raw, _ := json.Marshal(p)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'branch',$3,'create_branch','running','binding_admin',$1,$4,$5,now()+interval '5 minutes')`, operation, p.ProjectID, child, raw, worker)
	source := catalogInheritanceSource{Operation: operation, PayloadSHA256: catalogInheritancePayloadDigest(p), Project: p.ProjectID, Branch: child, Endpoint: endpoint, SourceBranch: parent, Scope: catalogNativeScope{p.TenantID, p.TimelineID}, SourceScope: sourceScope, InitializePlatform: true, Items: []catalogInheritanceItem{{Kind: "role", Name: "owned_role", Version: 2, CredentialRef: known.CredentialRef, CredentialUID: known.CredentialUID, Binding: &known}, {Kind: "database", Name: "owned_database", Owner: "owned_role", Version: 2, Binding: &dbref}, {Kind: "role", Name: "replaced_role", Version: 2, CredentialRef: replaced.CredentialRef, CredentialUID: replaced.CredentialUID, Binding: &replaced}, {Kind: "role", Name: "legacy_role", Version: 1, CredentialRef: "legacy-credential", CredentialUID: "44444444-4444-4444-8444-444444444444"}}}
	tx, e = s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = storeCatalogInheritanceSourceTx(ctx, tx, source); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	v := catalogInheritanceObservation{Operation: operation, SourceSHA256: catalogInheritanceSourceDigest(source), Project: p.ProjectID, Branch: child, Endpoint: endpoint, Scope: source.Scope, ConfigUID: "11111111-1111-4111-8111-111111111111", CredentialUID: "22222222-2222-4222-8222-222222222222", RoutesUID: "33333333-3333-4333-8333-333333333333", Objects: []catalogInheritanceObject{{Kind: "role", Name: "owned_role", OID: 1000, Status: "inherited_identity"}, {Kind: "database", Name: "owned_database", OID: 2000, OwnerOID: 1000, Status: "inherited_identity"}, {Kind: "role", Name: "replaced_role", OID: 3001, Status: "external_identity", Reason: "source_oid_replaced"}, {Kind: "role", Name: "legacy_role", OID: 4000, Status: "legacy_unattested"}}}
	commit := func(value catalogInheritanceObservation, lease string) error {
		tx, e := s.db.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if e = storeCatalogInheritanceObservationTx(ctx, tx, operation, lease, p, source, value); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if e = commit(v, "foreign-worker"); e == nil {
		t.Fatal("foreign observation worker accepted")
	}
	forged := v
	forged.SourceSHA256 = strings.Repeat("f", 64)
	if e = commit(forged, worker); e == nil {
		t.Fatal("changed source snapshot accepted")
	}
	exec(`CREATE FUNCTION reject_inheritance_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_inheritance_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_inheritance_audit()`)
	if e = commit(v, worker); e == nil {
		t.Fatal("audit failure committed inherited heads")
	}
	var count int
	s.db.QueryRow(ctx, `SELECT count(*) FROM catalog_object_bindings WHERE branch_id=$1`, child).Scan(&count)
	if count != 0 {
		t.Fatal("head survived failed inheritance audit")
	}
	exec(`DROP TRIGGER reject_inheritance_audit ON audit_events;DROP FUNCTION reject_inheritance_audit()`)
	if e = commit(v, worker); e != nil {
		t.Fatal(e)
	}
	if e = commit(v, worker); e != nil {
		t.Fatal("original observation not replayable", e)
	}
	tx, e = s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	inherited, e := loadCatalogObjectBinding(ctx, tx, p.ProjectID, child, "role", "owned_role", source.Scope)
	if e != nil || inherited.OID != known.OID || inherited.IdentityID == known.IdentityID || inherited.Version != 1 || inherited.CredentialUID != known.CredentialUID {
		t.Fatal("copied original identity lineage differs", e)
	}
	if e = checkCatalogForkExclusion(ctx, tx, p.ProjectID, child, "role", "replaced_role"); e == nil {
		t.Fatal("external replacement managed by child")
	}
	if _, e = loadCatalogObjectBinding(ctx, tx, p.ProjectID, child, "role", "legacy_role", source.Scope); e == nil {
		t.Fatal("legacy source name invented an original OID")
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"catalog_inheritance_sources", "catalog_inheritance_observations", "catalog_inheritance_exclusions"} {
		if _, e = s.db.Exec(ctx, "DELETE FROM "+table); e == nil {
			t.Fatal("inheritance evidence was deletable", table)
		}
	}
	s.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='observe_catalog_inheritance' AND request_id=$1`, operation).Scan(&count)
	if count != 1 {
		t.Fatal("observation audit duplicated")
	}
	// Preserve the parent and its credential generation, even after the child
	// acquired its own identity in a different native timeline.
	var oid uint32
	s.db.QueryRow(ctx, `SELECT object_oid FROM catalog_object_bindings WHERE id=$1`, known.ID).Scan(&oid)
	if oid != known.OID {
		t.Fatal("parent binding changed")
	}
}

func TestCatalogInheritanceFixedUIDProtectedConfigAndRoutePublication(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	p := createPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", EndpointType: "read_write", TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("b", 32), CatalogPolicy: catalogPreserveInheritedSQL}
	operation := "op_222222222222222222222222"
	worker := "inheritance_worker"
	raw, _ := json.Marshal(p)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'endpoint',$3,'create_endpoint','running','binding_admin',$1,$4,$5,now()+interval '5 minutes')`, operation, p.ProjectID, p.EndpointID, raw, worker)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,'customer_role','ready','publication-customer',1)`, p.BranchID)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state,version) VALUES($1,'customer_database','customer_role','ready',1)`, p.BranchID)
	source := catalogInheritanceSource{Operation: operation, PayloadSHA256: catalogInheritancePayloadDigest(p), Project: p.ProjectID, Branch: p.BranchID, Endpoint: p.EndpointID, SourceBranch: p.BranchID, Scope: catalogNativeScope{p.TenantID, p.TimelineID}, SourceScope: catalogNativeScope{p.TenantID, p.TimelineID}, Items: []catalogInheritanceItem{{Kind: "role", Name: "customer_role", Version: 1, CredentialRef: "publication-customer", CredentialUID: "66666666-6666-4666-8666-666666666666"}, {Kind: "database", Name: "customer_database", Owner: "customer_role", Version: 1}}}
	tx, e := s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = storeCatalogInheritanceSourceTx(ctx, tx, source); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	v := catalogInheritanceObservation{Operation: operation, SourceSHA256: catalogInheritanceSourceDigest(source), Project: p.ProjectID, Branch: p.BranchID, Endpoint: p.EndpointID, Scope: source.Scope, ConfigUID: "11111111-1111-4111-8111-111111111111", CredentialUID: "22222222-2222-4222-8222-222222222222", RoutesUID: "33333333-3333-4333-8333-333333333333", Objects: []catalogInheritanceObject{{Kind: "role", Name: "customer_role", OID: 5000, Status: "legacy_unattested"}, {Kind: "database", Name: "customer_database", OID: 6000, OwnerOID: 5000, Status: "legacy_unattested"}}}
	tx, e = s.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = storeCatalogInheritanceObservationTx(ctx, tx, operation, worker, p, source, v); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	admin, _ := scramVerifier("ci-inheritance-platform-password")
	probe, _ := scramVerifier("ci-inheritance-probe-password")
	text, e := computeConfig(p.ProjectID, p.TenantID, p.TimelineID, admin, probe, p.EndpointType)
	if e != nil {
		t.Fatal(e)
	}
	var document map[string]any
	json.Unmarshal([]byte(text), &document)
	document["preserved"] = map[string]any{"field": "original"}
	encode := func(value any) string { data, _ := json.Marshal(value); return base64.StdEncoding.EncodeToString(data) }
	config := map[string]any{"metadata": map[string]any{"uid": v.ConfigUID, "resourceVersion": "1", "labels": credentialLabels(p.ProjectID, p.EndpointID)}, "data": map[string]any{"config.json": encode(document), "probePassword": base64.StdEncoding.EncodeToString([]byte("ci-inheritance-probe-password"))}}
	credential := map[string]any{"metadata": map[string]any{"uid": v.CredentialUID, "labels": credentialLabels(p.ProjectID, p.EndpointID)}, "data": map[string]any{"adminVerifier": base64.StdEncoding.EncodeToString([]byte(admin)), "probeVerifier": base64.StdEncoding.EncodeToString([]byte(probe)), "probePassword": base64.StdEncoding.EncodeToString([]byte("ci-inheritance-probe-password"))}}
	routeMap := map[string]map[string]any{selector(p.EndpointID): {"project_id": p.ProjectID, "branch_id": p.BranchID, "roles": map[string]any{"cloud_admin": admin, "control_probe": probe}, "vm_template": map[string]any{"retained": "original"}}, "unrelated": {"retained": true}}
	routes := map[string]any{"metadata": map[string]any{"uid": v.RoutesUID, "resourceVersion": "1"}, "data": map[string]any{"routes.json": encode(routeMap)}}
	configPuts, routePuts := 0, 0
	conflict := true
	lost := true
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "-credentials") {
			jsonResponse(w, 200, credential)
			return
		}
		target := &config
		isRoute := strings.HasSuffix(r.URL.Path, "/"+routesSecret)
		if isRoute {
			target = &routes
		}
		if r.Method == http.MethodPut {
			var value map[string]any
			json.NewDecoder(r.Body).Decode(&value)
			if nested(value, "metadata", "uid") != nested(*target, "metadata", "uid") || nested(value, "metadata", "resourceVersion") != nested(*target, "metadata", "resourceVersion") {
				w.WriteHeader(409)
				return
			}
			if !isRoute {
				configPuts++
				if conflict {
					conflict = false
					config["metadata"].(map[string]any)["resourceVersion"] = "2"
					config["data"].(map[string]any)["concurrent"] = "retained"
					w.WriteHeader(409)
					return
				}
			} else {
				routePuts++
			}
			*target = value
			(*target)["metadata"].(map[string]any)["resourceVersion"] = "3"
			if !isRoute && lost {
				lost = false
				w.WriteHeader(503)
				return
			}
		}
		jsonResponse(w, 200, *target)
	}))
	defer peer.Close()
	s.kube = &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	desired := map[string]any{"roles": []any{map[string]any{"name": "customer_role", "encrypted_password": admin, "options": nil}}, "databases": []any{map[string]any{"name": "customer_database", "owner": "customer_role", "options": nil}}, "delta_operations": []any{}}
	catalog, _ := json.Marshal(desired)
	if e = s.publishCatalogInheritance(ctx, operation, worker, p, v, string(catalog)); e == nil {
		t.Fatal("unknown config acknowledgment did not retain retry")
	}
	if e = s.publishCatalogInheritance(ctx, operation, worker, p, v, string(catalog)); e != nil {
		t.Fatal(e)
	}
	if configPuts != 2 || routePuts != 1 || nested(config, "data", "concurrent") != "retained" {
		t.Fatal("original CAS/retry did not preserve concurrent fields")
	}
	stored, _ := secretText(config, "config.json")
	var actual map[string]any
	json.Unmarshal([]byte(stored), &actual)
	if nested(actual, "spec", "skip_pg_catalog_updates") != true || nested(actual, "preserved", "field") != "original" || len(nested(actual, "spec", "cluster", "roles").([]any)) != 3 {
		t.Fatal("protected catalog view was not retained")
	}
	var decoded map[string]map[string]any
	rawRoute, _ := secretText(routes, "routes.json")
	json.Unmarshal([]byte(rawRoute), &decoded)
	if len(decoded[selector(p.EndpointID)]["roles"].(map[string]any)) != 3 || decoded["unrelated"]["retained"] != true || nested(decoded[selector(p.EndpointID)], "vm_template", "retained") != "original" {
		t.Fatal("route publication touched another endpoint or topology")
	}
	before := configPuts + routePuts
	config["metadata"].(map[string]any)["uid"] = "44444444-4444-4444-8444-444444444444"
	if e = s.publishCatalogInheritance(ctx, operation, worker, p, v, string(catalog)); e == nil || configPuts+routePuts != before {
		t.Fatal("successor config was adopted")
	}
	config["metadata"].(map[string]any)["uid"] = v.ConfigUID
	routes["metadata"].(map[string]any)["uid"] = "55555555-5555-4555-8555-555555555555"
	if e = s.publishCatalogInheritance(ctx, operation, worker, p, v, string(catalog)); e == nil || configPuts+routePuts != before {
		t.Fatal("successor route was adopted")
	}
	if _, e = s.db.Exec(ctx, `DELETE FROM catalog_inheritance_publications`); e == nil {
		t.Fatal("original publication was deletable")
	}
	var count int
	s.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='publish_catalog_inheritance' AND request_id=$1`, operation).Scan(&count)
	if count != 1 {
		t.Fatal("publication audit duplicated")
	}
}
