package control

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Real disposable metadata/API transactions. No fixture is claimed to prove
// customer-code execution, Neon Proxy SQL, VM readiness or production fencing.
func TestFunctionsNativeAdmissionReplayAuthorizationAndManualLifecycle(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_functions_native_" + randomToken(6)
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
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('owner','owner','unused','owner'),('viewer','viewer','unused','viewer'),('stranger','stranger','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org','native-functions','Functions'),('other','other-functions','Other')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org','owner','owner'),('org','viewer','viewer'),('other','stranger','owner')`)
	for _, u := range []string{"owner", "viewer", "stranger"} {
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(u+"-session"), u, digest(u+"-csrf"))
	}
	const project = "prj_0123456789abcdef"
	const branch = "br_0123456789abcdef"
	const writer = "ep_0123456789abcdef"
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'org','Functions','rke2-lab',16,'ready','managed',now(),now())`, project)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES($1,$2,'main','ready',now())`, branch, project)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',now(),now())`, writer, project, branch)
	s := &server{db: db, storage: &storageBlobs{}, functions: &functionsPlatform{key: []byte(strings.Repeat("k", 32))}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := s.middleware(s.routes())
	base := "/api/v1/projects/" + project + "/branches/" + branch + "/functions"
	request := func(actor, method, path, key, version, body string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		if version != "" {
			r.Header.Set("If-Match", `"`+version+`"`)
		}
		if actor != "" {
			r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
			r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
			r.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d expected %d: %s", method, path, w.Code, want, w.Body.String())
		}
		var out map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	body := `{"slug":"hello","database_name":"neondb","sql_schema":"app"}`
	request("", "POST", base, "native-create-1", "", body, 401)
	request("viewer", "POST", base, "native-create-1", "", body, 403)
	request("stranger", "POST", base, "native-create-1", "", body, 404)
	first := request("owner", "POST", base, "native-create-1", "", body, 202)
	request("", "POST", base+"/hello/resume", "native-resume", "1", `{}`, 401)
	request("viewer", "POST", base+"/hello/resume", "native-resume", "1", `{}`, 403)
	request("stranger", "POST", base+"/hello/resume", "native-resume", "1", `{}`, 404)
	request("owner", "POST", base+"/hello/resume", "native-resume", "", `{}`, 428)
	request("owner", "POST", base+"/hello/resume", "native-resume", "9", `{}`, 412)
	request("owner", "POST", base+"/hello/resume", "native-resume", "1", `{"environment":{}}`, 422)
	request("owner", "POST", base+"/hello/resume", "native-resume", "1", `null`, 422)
	request("owner", "POST", base+"/hello/resume", "native-resume", "1", `{}`, 409)
	// New catalog authorization/stale checks must finish before external SQL.
	ownerPath := "/api/v1/projects/" + project + "/branches/" + branch + "/databases/owned"
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES($1,'owned','app_old','ready')`, branch)
	request("", "PATCH", ownerPath, "owner-change", "1", `{"owner":"app_new"}`, 401)
	request("viewer", "PATCH", ownerPath, "owner-change", "1", `{"owner":"app_new"}`, 403)
	request("stranger", "PATCH", ownerPath, "owner-change", "1", `{"owner":"app_new"}`, 404)
	request("owner", "PATCH", ownerPath, "owner-change", "", `{"owner":"app_new"}`, 428)
	request("owner", "PATCH", ownerPath, "owner-change", "9", `{"owner":"app_new"}`, 412)
	request("owner", "PATCH", ownerPath, "owner-change", "1", `{"owner":"app_old"}`, 422)
	request("owner", "PATCH", ownerPath, "owner-change", "1", `{"owner":"app_new"}`, 409)
	request("owner", "PATCH", ownerPath, "owner-change", "1", `{"owner":"control_probe"}`, 422)
	scriptPath := "/api/v1/projects/" + project + "/endpoints/" + writer + "/query-script"
	request("", "POST", scriptPath, "", "", `{"password":"unused","sql":"SELECT 1"}`, 401)
	request("viewer", "POST", scriptPath, "", "", `{"password":"unused","sql":"SELECT 1"}`, 403)
	request("stranger", "POST", scriptPath, "", "", `{"password":"unused","sql":"SELECT 1"}`, 404)
	request("owner", "POST", scriptPath, "", "", `{"password":"unused","sql":" "}`, 422)
	request("owner", "POST", scriptPath, "", "", `{"password":"unused","sql":"SELECT 1","role":"control_probe"}`, 422)
	request("", "GET", base+"/hello/runtime", "", "", "", 401)
	request("stranger", "GET", base+"/hello/runtime", "", "", "", 404)
	for _, actor := range []string{"owner", "viewer"} {
		observation := request(actor, "GET", base+"/hello/runtime", "", "", "", 200)
		if observation["observed"] != false || observation["observation_source"] != "metadata_only" || observation["background_counters_trusted"] != false {
			t.Fatal("inactive definition fabricated a runtime observation")
		}
	}
	replay := request("owner", "POST", base, "native-create-1", "", body, 202)
	if first["operation"].(map[string]any)["id"] != replay["operation"].(map[string]any)["id"] {
		t.Fatal("replay duplicated operation")
	}
	request("owner", "POST", base, "native-create-1", "", strings.Replace(body, "hello", "different", 1), 409)
	var id string
	var count int
	db.QueryRow(ctx, `SELECT id FROM function_definitions WHERE slug='hello'`).Scan(&id)
	if err = db.QueryRow(ctx, `SELECT count(*) FROM function_definitions`).Scan(&count); err != nil || count != 1 {
		t.Fatal("creation not atomic", err)
	}
	ack := `{"acknowledge_background_work_stop":true}`
	request("owner", "POST", base+"/hello/suspend", "native-stop-bad", "9", ack, 412)
	request("owner", "POST", base+"/hello/suspend", "native-stop-no-ack", "1", `{}`, 422)
	for i, step := range []struct {
		method, path, key, version string
		del                        bool
	}{{"POST", base + "/hello/suspend", "native-stop-1", "1", false}, {"DELETE", base + "/hello", "native-delete-1", "2", true}} {
		accepted := request("owner", step.method, step.path, step.key, step.version, ack, 202)
		op := accepted["operation"].(map[string]any)["id"].(string)
		exec(`UPDATE operations SET state='running',lease_owner='native-worker',lease_expires_at=now()+interval '1 hour' WHERE id=$1`, op)
		target := functionStopTarget{id, project, branch, int64(i + 2), step.del}
		s.processRole = roleWorker
		if e := s.reconcileFunctionStop(ctx, op, "native-worker", target); e != nil {
			t.Fatal("manual no-instance lifecycle failed", e)
		}
		exec(`UPDATE operations SET state='succeeded',finished_at=now() WHERE id=$1`, op)
		request("owner", step.method, step.path, step.key, step.version, ack, 202)
	}
	var state string
	if err = db.QueryRow(ctx, `SELECT state FROM function_definitions WHERE id=$1`, id).Scan(&state); err != nil || state != "deleted" {
		t.Fatal("retirement not committed", err)
	}
	request("owner", "POST", base, "native-create-new", "", body, 202)
	if err = db.QueryRow(ctx, `SELECT count(*) FROM function_definitions WHERE slug='hello'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("retired slug not reusable", err)
	}
	exec(`UPDATE organizations SET state='suspended' WHERE id='org'`)
	request("owner", "POST", base, "native-closed-org", "", strings.Replace(body, "hello", "closed", 1), 404)
}

func TestReservedFunctionsRolesCannotEnterCatalog(t *testing.T) {
	for _, name := range []string{"fn_0123456789abcdef", "FN_0123456789ABCDEF", "fn_user"} {
		if !protectedPGRole(name) {
			t.Fatal("Function namespace entered ordinary catalog")
		}
	}
	if !reservedFunctionRole("fn_0123456789abcdef") {
		t.Fatal("reserved service role rejected")
	}
	for _, name := range []string{"fn_user", "fn_0123456789abcdeF", "fn_0123456789abcdef0", "cloud_admin"} {
		if reservedFunctionRole(name) {
			t.Fatal("ordinary role preserved as Function")
		}
	}
}
