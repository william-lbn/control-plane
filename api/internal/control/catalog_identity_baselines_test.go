package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCatalogBaselineInitialObservationRequiresKnownIntent(t *testing.T) {
	e, snapshot := inventoryFixture()
	const branch = "br_0123456789abcdef"
	snapshot.Roles[0].Marker = "neon-control/branch/" + branch
	snapshot.Roles[0].Flags = catalogRoleFlags{Login: true, Inherit: true, CreateRole: true, CreateDB: true, Replication: true, BypassRLS: true, ConnectionLimit: -1}
	input := catalogBaselineInput{Kind: "role", Name: e.Name, OID: e.OID, Acknowledge: true}
	intent := catalogBaselineIntent{Scope: snapshot.Scope, Version: 1, State: "ready", Reference: "owned-role"}
	proof, err := prepareCatalogBaseline(input, intent, snapshot, branch)
	if err != nil || proof.Expected.OID != e.OID {
		t.Fatal("valid first observation rejected", err)
	}
	for _, edit := range []func(*catalogBaselineInput, *catalogBaselineIntent, *catalogNativeSnapshot){
		func(i *catalogBaselineInput, v *catalogBaselineIntent, s *catalogNativeSnapshot) {
			i.Acknowledge = false
		},
		func(i *catalogBaselineInput, v *catalogBaselineIntent, s *catalogNativeSnapshot) {
			i.Name = "cloud_admin"
		},
		func(i *catalogBaselineInput, v *catalogBaselineIntent, s *catalogNativeSnapshot) { i.OID++ },
		func(i *catalogBaselineInput, v *catalogBaselineIntent, s *catalogNativeSnapshot) { v.Version = 0 },
		func(i *catalogBaselineInput, v *catalogBaselineIntent, s *catalogNativeSnapshot) {
			v.State = "updating"
		},
		func(i *catalogBaselineInput, v *catalogBaselineIntent, s *catalogNativeSnapshot) {
			s.Roles[0].Flags.Login = false
		},
		func(i *catalogBaselineInput, v *catalogBaselineIntent, s *catalogNativeSnapshot) {
			s.Roles[0].Flags.Superuser = true
		},
		func(i *catalogBaselineInput, v *catalogBaselineIntent, s *catalogNativeSnapshot) {
			s.Roles[0].Marker = "copied wrong branch"
		},
	} {
		i, v := input, intent
		s := snapshot
		s.Roles = append([]catalogNativeRole{}, snapshot.Roles...)
		edit(&i, &v, &s)
		if _, err = prepareCatalogBaseline(i, v, s, branch); err == nil {
			t.Fatal("ambiguous initial observation accepted")
		}
	}
	snapshot.Databases[0].Marker = "neon-control/branch/" + branch
	input = catalogBaselineInput{Kind: "database", Name: "customer_db", OID: 51, Acknowledge: true}
	intent.Reference = "customer"
	if _, err = prepareCatalogBaseline(input, intent, snapshot, branch); err != nil {
		t.Fatal("valid database baseline rejected", err)
	}
	snapshot.Databases[0].OwnerOID = 42
	if _, err = prepareCatalogBaseline(input, intent, snapshot, branch); err == nil {
		t.Fatal("external owner accepted")
	}
}

func TestCatalogBaselineAtomicReplayAuthorizationAndDrift(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_baseline_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	const project = "prj_b0123456789abcde"
	const branch = "br_b0123456789abcde"
	const endpoint = "ep_b0123456789abcde"
	_, snapshot := inventoryFixture()
	snapshot.Roles[0].Marker = "neon-control/branch/" + branch
	snapshot.Databases[0].Marker = snapshot.Roles[0].Marker
	snapshot.Roles[0].Flags = catalogRoleFlags{Login: true, Inherit: true, CreateRole: true, CreateDB: true, Replication: true, BypassRLS: true, ConnectionLimit: -1}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('baseline_admin','baseline_admin','unused','owner'),('baseline_editor','baseline_editor','unused','owner'),('baseline_foreign','baseline_foreign','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('baseline_org','baseline-org','Baseline')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('baseline_org','baseline_admin','admin'),('baseline_org','baseline_editor','editor')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES($1,'baseline_org','Baseline','lab',16,'ready','managed',$2,now(),now())`, project, snapshot.Scope.TenantID)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'main',$3,'ready',now())`, branch, project, snapshot.Scope.TimelineID)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','postgres','read_write',now(),now())`, endpoint, project, branch)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES($1,'customer','ready','original-customer')`, branch)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES($1,'customer_db','customer','ready')`, branch)
	s := &server{db: db}
	input := catalogBaselineInput{Kind: "role", Name: "customer", OID: 41, Acknowledge: true}
	intent, err := readCatalogBaselineIntent(ctx, db, project, branch, input.Kind, input.Name)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := prepareCatalogBaseline(input, intent, snapshot, branch)
	if err != nil {
		t.Fatal(err)
	}
	proof.CredentialUID = "11111111-1111-4111-8111-111111111111"
	expectFault := func(e error, want int) {
		t.Helper()
		var fault *catalogRenameFault
		if !errors.As(e, &fault) || fault.Status != want {
			t.Fatalf("wrong baseline rejection: %v, want %d", e, want)
		}
	}
	path := "/api/v1/projects/" + project + "/branches/" + branch + "/catalog-identities"
	hash := s.createRequestHash("POST:"+path, record{"input": input, "version": int64(1)})
	admit := func(actor, key, h string, p catalogBaselineProof, want int) record {
		t.Helper()
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		v, e := commitCatalogBaselineTx(ctx, tx, project, branch, actor, "baseline-request", keyHash(key), h, input, p)
		if want != 0 {
			expectFault(e, want)
			return nil
		}
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		return v
	}
	admit("baseline_editor", "original", hash, proof, 403)
	admit("baseline_foreign", "original", hash, proof, 403)
	exec(`UPDATE users SET disabled=true WHERE id='baseline_admin'`)
	admit("baseline_admin", "original", hash, proof, 403)
	exec(`UPDATE users SET disabled=false WHERE id='baseline_admin'`)
	stale := proof
	stale.Intent.Version++
	admit("baseline_admin", "original", hash, stale, 412)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('op_baseline_pending',$1,'branch_catalog',$2,'create_role','queued','baseline_admin','req','{}')`, project, branch)
	admit("baseline_admin", "original", hash, proof, 409)
	exec(`UPDATE operations SET state='succeeded' WHERE id='op_baseline_pending'`)
	exec(`CREATE FUNCTION reject_baseline_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_baseline_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_baseline_audit()`)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = commitCatalogBaselineTx(ctx, tx, project, branch, "baseline_admin", "req", keyHash("original"), hash, input, proof)
	if err == nil {
		t.Fatal("audit failure accepted baseline")
	}
	_ = tx.Rollback(ctx)
	var count int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM catalog_identity_baselines`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial baseline survived audit failure", err)
	}
	exec(`DROP TRIGGER reject_baseline_audit ON audit_events`)
	first := admit("baseline_admin", "original", hash, proof, 0)
	replay := admit("baseline_admin", "original", hash, proof, 0)
	if first["binding_id"] != replay["binding_id"] || first["captured_at"] != replay["captured_at"] {
		t.Fatal("lost acknowledgement changed original record")
	}
	admit("baseline_admin", "original", "different", proof, 409)
	admit("baseline_admin", "new", hash, proof, 409)
	report := func(snap catalogNativeSnapshot) record {
		t.Helper()
		v, e := s.catalogIdentityReport(ctx, project, branch, snap)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	status := func(snap catalogNativeSnapshot, want string) {
		t.Helper()
		v := report(snap)
		items := v["items"].([]record)
		if len(items) != 1 || items[0]["status"] != want || items[0]["credentials"] != "not_observed" {
			t.Fatalf("unexpected drift report: %+v", items)
		}
	}
	status(snapshot, "catalog_matches")
	snapshot.Roles[0].Name = "external_rename"
	status(snapshot, "renamed")
	snapshot.Roles[0].Name = "customer"
	snapshot.Roles[0].OID = 43
	snapshot.Databases[0].OwnerOID = 43
	status(snapshot, "replaced")
	exec(`UPDATE branch_roles SET version=version+1 WHERE branch_id=$1 AND name='customer'`, branch)
	status(snapshot, "intent_changed")
	// Durable baseline is not silently refreshed by a read or an intent change.
	var oid uint32
	var baselineVersion int64
	if err = db.QueryRow(ctx, `SELECT object_oid,intent_version FROM catalog_identity_baselines`).Scan(&oid, &baselineVersion); err != nil || oid != 41 || baselineVersion != 1 {
		t.Fatal("immutable baseline overwritten", err)
	}
	for _, actor := range []string{"baseline_admin", "baseline_editor", "baseline_foreign"} {
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	handler := s.routes()
	raw, _ := json.Marshal(input)
	request := func(actor, method, body, match string, csrf bool, want int) record {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		if csrf {
			req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
			req.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		req.Header.Set("If-Match", match)
		req.Header.Set("Idempotency-Key", "original")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("baseline HTTP %d, want %d: %s", rec.Code, want, rec.Body.String())
		}
		var v record
		if err = json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	request("baseline_foreign", "GET", "", "", true, 404)
	request("baseline_editor", "POST", string(raw), `"1"`, true, 403)
	request("baseline_admin", "POST", string(raw), `"1"`, false, 403)
	request("baseline_admin", "POST", `{"kind":"role","name":"cloud_admin","oid":41,"acknowledge_initial_observation":true}`, `"1"`, true, 422)
	request("baseline_admin", "POST", string(raw), "", true, 428)
	request("baseline_admin", "POST", string(raw), "1", true, 422)
	// Retiring the original Writer does not prevent replay or cold-wake another.
	exec(`UPDATE endpoints SET state='deleted',deleted_at=now() WHERE id=$1`, endpoint)
	if v := request("baseline_admin", "POST", string(raw), `"1"`, true, 200); v["binding_id"] != first["binding_id"] {
		t.Fatal("original HTTP receipt lost")
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM catalog_identity_requests`).Scan(&count); err != nil || count != 1 {
		t.Fatal("receipt duplicated", err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='initialize_catalog_identity'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("audit duplicated", err)
	}
	encoded, _ := json.Marshal(report(snapshot))
	for _, private := range []string{proof.CredentialUID, proof.Intent.Reference, proof.Expected.MarkerSHA256, snapshot.Roles[0].Marker} {
		if private != "" && strings.Contains(string(encoded), private) {
			t.Fatalf("public observation leaked private field %q", private)
		}
	}
	if fmt.Sprint(report(snapshot)["complete"]) != "true" {
		t.Fatal("complete observation flag missing")
	}
}
