package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Actual PostgreSQL transactions/migrations, authorization and Worker replay.
// Native storage and Kubernetes faults are HTTP peers here, with a separate
// opt-in Chromium suite for real Neon timelines and VM retirement on Linux.
func TestRetainedDeletionIntegration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA") + "_deletion"
	if !regexp.MustCompile(`^v2_auth_[a-z0-9_]+$`).MatchString(schema) || len(schema) > 63 {
		t.Fatal("isolated schema required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
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
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_delete','delete','Delete'),('org_out','out','Out')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('org_delete'),('org_out')`)
	for _, u := range []string{"admin_delete", "editor_delete", "viewer_delete", "outside_delete"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, u)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(u+"-session"), u, digest(u+"-csrf"))
	}
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_delete','admin_delete','admin'),('org_delete','editor_delete','editor'),('org_delete','viewer_delete','viewer'),('org_out','outside_delete','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_delete','org_delete','Delete project','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','rke2-lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,is_default,protected,state,created_at) VALUES('br_root','prj_delete','main','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',NULL,true,true,'ready',now()),('br_parent','prj_delete','parent','cccccccccccccccccccccccccccccccc','br_root',false,false,'ready',now()),('br_leaf','prj_delete','leaf','dddddddddddddddddddddddddddddddd','br_parent',false,false,'ready',now())`)
	exec(`UPDATE projects SET default_branch_id='br_root' WHERE id='prj_delete'`)
	nativeReads := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || !strings.Contains(r.URL.Path, "/timeline/") {
			t.Errorf("unexpected physical operation %s %s", r.Method, r.URL.Path)
		}
		nativeReads++
		jsonResponse(w, 200, record{"state": "Active"})
	}))
	defer peer.Close()
	s := &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), idempotencyKey: []byte(strings.Repeat("d", 32)), kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
	handler := s.routes()
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	request := func(actor, method, path, body, key, version string, want int) record {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", version)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s status %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		var v record
		if json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal("invalid response")
		}
		return v
	}
	base := "/api/v1/projects/prj_delete"
	t.Run("cross_tenant_and_viewer_are_denied", func(t *testing.T) {
		request("outside_delete", "GET", base+"/lifecycle", "", "", "", 404)
		request("viewer_delete", "DELETE", base, `{"confirm_name":"Delete project"}`, "delete-key", "\"1\"", 403)
		request("editor_delete", "PATCH", base+"/branches/br_leaf/protection", `{"confirm_name":"leaf","protected":true}`, "", "\"1\"", 403)
	})
	t.Run("root_and_parent_dependencies_are_enforced", func(t *testing.T) {
		request("editor_delete", "DELETE", base+"/branches/br_root", `{"confirm_name":"main"}`, "delete-root", "\"1\"", 409)
		request("editor_delete", "DELETE", base+"/branches/br_parent", `{"confirm_name":"parent"}`, "delete-parent", "\"1\"", 409)
	})
	t.Run("name_and_version_are_required", func(t *testing.T) {
		request("editor_delete", "DELETE", base+"/branches/br_leaf", `{"confirm_name":"leaf"}`, "delete-leaf", "", 428)
		request("editor_delete", "DELETE", base+"/branches/br_leaf", `{"confirm_name":"wrong"}`, "delete-leaf", "\"1\"", 422)
		request("editor_delete", "DELETE", base+"/branches/br_leaf", `{"confirm_name":"leaf"}`, "delete-leaf", "\"9\"", 412)
	})
	t.Run("branch_protection_is_explicit", func(t *testing.T) {
		request("admin_delete", "PATCH", base+"/branches/br_leaf/protection", `{"confirm_name":"leaf","protected":true}`, "", "\"1\"", 200)
		request("editor_delete", "DELETE", base+"/branches/br_leaf", `{"confirm_name":"leaf"}`, "delete-leaf", "\"2\"", 409)
		request("admin_delete", "PATCH", base+"/branches/br_leaf/protection", `{"confirm_name":"leaf","protected":false}`, "", "\"2\"", 200)
	})
	var leafOp string
	t.Run("leaf_delete_is_durable_and_idempotent", func(t *testing.T) {
		v := request("editor_delete", "DELETE", base+"/branches/br_leaf", `{"confirm_name":"leaf"}`, "delete-leaf", "\"3\"", 202)
		leafOp = v["operation"].(map[string]any)["id"].(string)
		again := request("editor_delete", "DELETE", base+"/branches/br_leaf", `{"confirm_name":"leaf"}`, "delete-leaf", "\"3\"", 202)
		if again["operation"].(map[string]any)["id"] != leafOp {
			t.Fatal("replay created another operation")
		}
		request("editor_delete", "DELETE", base+"/branches/br_leaf", `{"confirm_name":"changed"}`, "delete-leaf", "\"3\"", 409)
		if err = s.workOnce(ctx, "delete-worker"); err != nil {
			t.Fatal(err)
		}
		v = request("editor_delete", "GET", base+"/operations/"+leafOp, "", "", "", 200)
		if v["state"] != "succeeded" {
			t.Fatal(v)
		}
		request("editor_delete", "GET", base+"/branches/br_leaf", "", "", "", 404)
	})
	t.Run("new_or_retried_work_cannot_resurrect_deleted_branch", func(t *testing.T) {
		_, e := db.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('op_forbidden','prj_delete','branch_catalog','br_leaf','create_role','queued','editor_delete','test','{"branch_id":"br_leaf"}')`)
		if e == nil {
			t.Fatal("operation admitted for tombstone")
		}
	})
	t.Run("project_protection_and_protected_branches_are_separate", func(t *testing.T) {
		request("admin_delete", "DELETE", base, `{"confirm_name":"Delete project"}`, "delete-project", "\"1\"", 409)
		request("admin_delete", "PATCH", base+"/protection", `{"confirm_name":"Delete project","protected":false}`, "", "\"1\"", 200)
		request("admin_delete", "DELETE", base, `{"confirm_name":"Delete project"}`, "delete-project", "\"2\"", 409)
		request("admin_delete", "PATCH", base+"/branches/br_root/protection", `{"confirm_name":"main","protected":false}`, "", "\"1\"", 200)
	})
	var projectOp string
	t.Run("project_tombstone_preserves_prior_deleted_branch_and_history", func(t *testing.T) {
		v := request("admin_delete", "DELETE", base, `{"confirm_name":"Delete project"}`, "delete-project", "\"2\"", 202)
		projectOp = v["operation"].(map[string]any)["id"].(string)
		if err = s.workOnce(ctx, "delete-worker"); err != nil {
			t.Fatal(err)
		}
		v = request("admin_delete", "GET", base+"/operations/"+projectOp, "", "", "", 200)
		if v["state"] != "succeeded" {
			t.Fatal(v)
		}
		request("admin_delete", "GET", base, "", "", "", 404)
		request("viewer_delete", "GET", base+"/lifecycle", "", "", "", 404)
		trash := request("admin_delete", "GET", "/api/v1/organizations/org_delete/projects?deleted=true", "", "", "", 200)
		if len(trash["items"].([]any)) != 1 {
			t.Fatal("tombstone absent")
		}
		trash = request("viewer_delete", "GET", "/api/v1/organizations/org_delete/projects?deleted=true", "", "", "", 200)
		if len(trash["items"].([]any)) != 0 {
			t.Fatal("viewer sees deleted project")
		}
	})
	t.Run("seven_day_recovery_expiry_is_enforced", func(t *testing.T) {
		exec(`UPDATE projects SET recover_until=now()-interval '1 second' WHERE id='prj_delete'`)
		request("admin_delete", "POST", base+"/recover", `{"confirm_name":"Delete project"}`, "recover-project", "\"4\"", 409)
		exec(`UPDATE projects SET recover_until=now()+interval '7 days' WHERE id='prj_delete'`)
	})
	t.Run("recovery_restores_only_owned_snapshot_and_cannot_enable_backend_services", func(t *testing.T) {
		v := request("admin_delete", "POST", base+"/recover", `{"confirm_name":"Delete project"}`, "recover-project", "\"4\"", 202)
		id := v["operation"].(map[string]any)["id"].(string)
		if err = s.workOnce(ctx, "delete-worker"); err != nil {
			t.Fatal(err)
		}
		v = request("admin_delete", "GET", base+"/operations/"+id, "", "", "", 200)
		if v["state"] != "succeeded" {
			t.Fatal(v)
		}
		request("admin_delete", "GET", base, "", "", "", 200)
		request("admin_delete", "GET", base+"/branches/br_leaf", "", "", "", 404)
		if nativeReads != 2 {
			t.Fatalf("retained timelines not verified: %d", nativeReads)
		}
		var held, count int
		_ = db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE physical_gc_state='held'),count(*) FROM resource_tombstones`).Scan(&held, &count)
		if held != 2 || count != 2 {
			t.Fatal("history or retention lost")
		}
		// Resume after the metadata commit but before route publication/finish.
		// This is the durable boundary a worker crash or unknown Kube outcome
		// can leave; today's restored marker must permit the same Operation.
		exec(`UPDATE operations SET state='running',lease_owner='recovery-replay',lease_expires_at=now()+interval '1 hour' WHERE id=$1`, id)
		var raw []byte
		if e := db.QueryRow(ctx, "SELECT payload FROM operations WHERE id=$1", id).Scan(&raw); e != nil {
			t.Fatal(e)
		}
		var p deletionPayload
		_ = json.Unmarshal(raw, &p)
		if e := s.reconcileDeletion(ctx, id, "recovery-replay", "recover_project", p); e != nil {
			t.Fatal("recovery after committed metadata failed", e)
		}
		exec(`UPDATE operations SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, id)
	})
	t.Run("stale_delete_worker_cannot_reclose_recovered_project", func(t *testing.T) {
		exec(`UPDATE operations SET state='running',lease_owner='stale-worker',lease_expires_at=now()+interval '1 hour' WHERE id=$1`, projectOp)
		var raw []byte
		_ = db.QueryRow(ctx, "SELECT payload FROM operations WHERE id=$1", projectOp).Scan(&raw)
		var p deletionPayload
		_ = json.Unmarshal(raw, &p)
		if e := s.reconcileDeletion(ctx, projectOp, "stale-worker", "delete_project", p); e == nil {
			t.Fatal("stale deletion accepted after recovery")
		}
	})
}
