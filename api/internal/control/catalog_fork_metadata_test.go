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

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCatalogForkOriginalLeaseImmutableHistoryAndAuditRollback(t *testing.T) {
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
	schema := "v2_fork_exclusion_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgQuote(schema)); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgQuote(schema)+" CASCADE")
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
	const project = "prj_0123456789abcdef"
	const parent = "br_1123456789abcdef"
	const child = "br_2123456789abcdef"
	const endpoint = "ep_2123456789abcdef"
	const op = "op_111111111111111111111111"
	const worker = "fork-original-worker"
	const actor = "fork_admin"
	p := createPayload{ProjectID: project, BranchID: child, EndpointID: endpoint, TenantID: strings.Repeat("a", 32), ParentTimelineID: strings.Repeat("b", 32), TimelineID: strings.Repeat("c", 32), ParentLSN: "0/1000000"}
	v := catalogForkExclusion{Name: "replaced_role", BaselineID: "cbi_111111111111111111111111", SourceBranchID: parent, ExpectedOID: 12345, ObservedOID: 12346, CredentialRef: "original-fork-role-secret", CredentialUID: "11111111-1111-4111-8111-111111111111"}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('fork_org','fork-org','Fork')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('fork_org',$1,'admin')`, actor)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES($1,'fork_org','Fork','lab',16,'ready','managed',$2,now(),now())`, project, p.TenantID)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'parent',$3,'ready',now())`, parent, project, p.ParentTimelineID)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,state,created_at) VALUES($1,$2,'child',$3,$4,'creating',now())`, child, project, p.TimelineID, parent)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'error','cloud_admin','postgres','read_write',now(),now())`, endpoint, project, child)
	for _, branch := range []string{parent, child} {
		exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES($1,$2,'ready',$3)`, branch, v.Name, v.CredentialRef)
	}
	exec(`INSERT INTO catalog_identity_baselines(id,project_id,branch_id,kind,name,object_oid,intent_version,tenant_id,timeline_id,expectation,marker_sha256,credential_ref,credential_uid,captured_by) VALUES($1,$2,$3,'role',$4,$5,1,$6,$7,'{}',$8,$9,$10,$11)`, v.BaselineID, project, parent, v.Name, int64(v.ExpectedOID), p.TenantID, p.ParentTimelineID, strings.Repeat("d", 64), v.CredentialRef, v.CredentialUID, actor)
	raw, _ := json.Marshal(p)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'branch',$3,'create_branch','running',$4,'fork-original-request',$5,$6,clock_timestamp()+interval '5 minutes')`, op, project, child, actor, raw, worker)
	exec(`UPDATE branches SET state='error' WHERE id=$1`, child)
	exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state,updated_at) VALUES($1,0,'create_neonvm','succeeded',(SELECT created_at+interval '1 second' FROM operations WHERE id=$1)),($1,1,'sql_probe_via_proxy','failed',now())`, op)
	recordInspection := func(value createPayload, lease string, items []catalogForkExclusion) error {
		t.Helper()
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = recordCatalogForkInspectionTx(ctx, tx, op, lease, value, parent, "known_identity_check", items); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if err = recordInspection(p, "other-worker", []catalogForkExclusion{v}); err == nil {
		t.Fatal("foreign lease recorded source history")
	}
	wrong := p
	wrong.TenantID = strings.Repeat("e", 32)
	if err = recordInspection(wrong, worker, []catalogForkExclusion{v}); err == nil {
		t.Fatal("cross-tenant source observation accepted")
	}
	wrongItem := v
	wrongItem.ExpectedOID++
	if err = recordInspection(p, worker, []catalogForkExclusion{wrongItem}); err == nil {
		t.Fatal("forged original baseline proof accepted")
	}
	exec(`CREATE FUNCTION reject_fork_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned fork audit failure'; END $$; CREATE TRIGGER reject_fork_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_fork_audit()`)
	if err = recordInspection(p, worker, []catalogForkExclusion{v}); err == nil {
		t.Fatal("audit failure committed inspection")
	}
	var count int
	err = db.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_fork_inspections)+(SELECT count(*) FROM catalog_fork_exclusions)`).Scan(&count)
	if err != nil || count != 0 {
		t.Fatal("partial failed inspection", err)
	}
	exec(`DROP TRIGGER reject_fork_audit ON audit_events`)
	exec(`UPDATE projects SET state='deleting' WHERE id=$1`, project)
	if err = recordInspection(p, worker, []catalogForkExclusion{v}); err == nil {
		t.Fatal("retiring project acquired a new fork exclusion")
	}
	exec(`UPDATE projects SET state='ready' WHERE id=$1`, project)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES($1,'retained_dependent_db',$2,'ready')`, parent, v.Name)
	if err = recordInspection(p, worker, []catalogForkExclusion{v}); err == nil {
		t.Fatal("critical database dependency excluded")
	}
	exec(`DELETE FROM branch_databases WHERE branch_id=$1 AND name='retained_dependent_db'`, parent)
	if err = recordInspection(p, worker, []catalogForkExclusion{v}); err != nil {
		t.Fatal(err)
	}
	changed := v
	changed.ObservedOID++
	if err = recordInspection(p, worker, []catalogForkExclusion{changed}); err != nil {
		t.Fatal("original receipt did not replay", err)
	}
	excluded, err := readCatalogForkExclusions(ctx, db, project, child)
	if err != nil || len(excluded) != 1 || excluded[0].ObservedOID != v.ObservedOID || excluded[0].ExpectedOID != v.ExpectedOID {
		t.Fatal("immutable original exclusion changed", err)
	}
	if e := checkCatalogForkExclusion(ctx, db, project, child, "role", v.Name); e == nil {
		t.Fatal("excluded role still considered managed")
	}
	if _, e := readCatalogBaselineIntent(ctx, db, project, child, "role", v.Name); e == nil {
		t.Fatal("initial observation can re-adopt an excluded role")
	}
	viewServer := &server{db: db}
	view, err := viewServer.catalogIdentityReport(ctx, project, child, catalogNativeSnapshot{Scope: catalogNativeScope{TenantID: p.TenantID, TimelineID: p.TimelineID}, Roles: []catalogNativeRole{{OID: v.ObservedOID, Name: v.Name}}, Databases: []catalogNativeDatabase{}})
	if err != nil {
		t.Fatal(err)
	}
	objects := view["native_objects"].([]record)
	if len(objects) != 1 || objects[0]["managed"] != false || objects[0]["can_initialize"] != false || objects[0]["management_excluded"] != true {
		t.Fatal("excluded role exposed as a new managed identity")
	}
	if _, e := db.Exec(ctx, `UPDATE catalog_fork_exclusions SET observed_oid=observed_oid+1 WHERE branch_id=$1`, child); e == nil {
		t.Fatal("original observation overwritten")
	}
	if _, e := db.Exec(ctx, `UPDATE catalog_fork_inspections SET source_timeline_id=$2 WHERE branch_id=$1`, child, strings.Repeat("f", 32)); e == nil {
		t.Fatal("original source scope overwritten")
	}
	verifier, err := scramVerifier("owned-original-fork-credential")
	if err != nil {
		t.Fatal(err)
	}
	var created time.Time
	if err = db.QueryRow(ctx, `SELECT created_at FROM operations WHERE id=$1`, op).Scan(&created); err != nil {
		t.Fatal(err)
	}
	configUID, routesUID := "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	credentialUID := v.CredentialUID
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("metadata freeze unexpectedly mutated Kubernetes")
			w.WriteHeader(405)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch name {
		case v.CredentialRef:
			jsonResponse(w, 200, record{"metadata": record{"uid": credentialUID, "labels": credentialLabels(project, "ep_1123456789abcdef")}, "immutable": true, "data": record{"verifier": base64.StdEncoding.EncodeToString([]byte(verifier))}})
		case kubeName(endpoint) + "-config":
			jsonResponse(w, 200, record{"metadata": record{"uid": configUID, "labels": credentialLabels(project, endpoint), "creationTimestamp": created.Add(10 * time.Millisecond).UTC().Format(time.RFC3339Nano)}})
		case routesSecret:
			jsonResponse(w, 200, record{"metadata": record{"uid": routesUID}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	s := &server{db: db, kube: &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}}
	binding, err := s.freezeCatalogForkBacking(ctx, op, worker, p, excluded)
	if err != nil {
		t.Fatal("original backing freeze failed", err)
	}
	if binding.ConfigUID != configUID || binding.RoutesUID != routesUID || binding.Digests[v.Name] != catalogMarkerDigest(verifier) {
		t.Fatal("backing credential intent binding lost")
	}
	credentialUID = "44444444-4444-4444-8444-444444444444"
	if _, e := s.freezeCatalogForkBacking(ctx, op, worker, p, excluded); e == nil {
		t.Fatal("replacement immutable credential adopted after original freeze")
	}
	credentialUID = v.CredentialUID
	if _, e := db.Exec(ctx, `UPDATE catalog_fork_exclusion_backings SET config_uid=$2 WHERE endpoint_id=$1`, endpoint, "55555555-5555-4555-8555-555555555555"); e == nil {
		t.Fatal("original config UID rebound")
	}
	finish := func(lease string) error {
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = commitCatalogForkBackingReadyTx(ctx, tx, op, lease, p, binding); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if err = finish("other-worker"); err == nil {
		t.Fatal("foreign lease committed filtered backing result")
	}
	exec(`CREATE TRIGGER reject_fork_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_fork_audit()`)
	if err = finish(worker); err == nil {
		t.Fatal("audit failure committed backing result")
	}
	exec(`DROP TRIGGER reject_fork_audit ON audit_events`)
	current, err := readCatalogForkBacking(ctx, db, endpoint)
	if err != nil || current.State != "updating" {
		t.Fatal("failed audit committed partial backing result", err)
	}
	if err = finish(worker); err != nil {
		t.Fatal(err)
	}
	if err = finish(worker); err != nil {
		t.Fatal("original backing result replay failed", err)
	}
	err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action='filter_inherited_role_projection' AND outcome='succeeded'`, child).Scan(&count)
	if err != nil || count != 1 {
		t.Fatal("duplicate/missing original backing audit", err)
	}
	if _, e := db.Exec(ctx, `UPDATE catalog_fork_exclusion_backings SET state='updating' WHERE endpoint_id=$1`, endpoint); e == nil {
		t.Fatal("completed original backing result reverted")
	}
}
