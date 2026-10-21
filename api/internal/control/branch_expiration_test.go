package control

import (
	"context"
	"encoding/base64"
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

func TestBranchExpirationTimestamp(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	for _, raw := range []string{`null`, `"2026-10-18T01:00:00+01:00"`, `"2026-10-18T02:00:00+01:00"`, `"2026-10-18T02:00:00"`, `"2026-12-18T02:00:00Z"`, `42`, ``} {
		deadline, interval, err := parseBranchExpiration(json.RawMessage(raw), now)
		switch raw {
		case `null`:
			if err != nil || deadline != nil || interval != nil {
				t.Fatal("explicit clear rejected")
			}
		case `"2026-10-18T02:00:00+01:00"`:
			if err != nil || deadline == nil || deadline.Format(time.RFC3339) != "2026-10-18T01:00:00Z" || *interval != 3600 {
				t.Fatal("offset normalization failed", err)
			}
		default:
			if err == nil {
				t.Fatal("invalid expiration accepted", raw)
			}
		}
	}
}

func TestBranchExpirationFunctionalIntegration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA") + "_expiry"
	if !regexp.MustCompile(`^v2_auth_[a-z0-9_]+$`).MatchString(schema) || len(schema) > 63 {
		t.Fatal("isolated schema required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_expiry','expiry','Expiry'),('org_expiry_out','expiry-out','Outside')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('org_expiry'),('org_expiry_out')`)
	for _, actor := range []string{"admin_expiry", "editor_expiry", "viewer_expiry", "outside_expiry"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_expiry','admin_expiry','admin'),('org_expiry','editor_expiry','editor'),('org_expiry','viewer_expiry','viewer'),('org_expiry_out','outside_expiry','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_expiry','org_expiry','Expiry project',repeat('a',32),'rke2-lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,is_default,protected,state,created_at) VALUES('br_expiry_root','prj_expiry','main',repeat('b',32),NULL,true,true,'ready',now()),('br_expiry_leaf','prj_expiry','temporary',repeat('c',32),'br_expiry_root',false,false,'ready',now()),('br_expiry_protected','prj_expiry','protected',repeat('d',32),'br_expiry_root',false,true,'ready',now())`)
	exec(`UPDATE projects SET default_branch_id='br_expiry_root' WHERE id='prj_expiry'`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_expiry_leaf','prj_expiry','br_expiry_leaf','ep-expiry-leaf','neonvm','cp-expiry_leaf','cp-expiry_leaf','active','cloud_admin','postgres',now(),now())`)
	exec(`INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version) VALUES('br_expiry_leaf','postgres','active','suspended','open-source-neon')`)
	endpoint := "ep_expiry_leaf"
	name := kubeName(endpoint)
	routes := record{selector(endpoint): record{"project_id": "prj_expiry", "branch_id": "br_expiry_leaf", "state": "suspended"}, "unrelated": record{"state": "active", "project_id": "other"}}
	servicePresent := true
	physicalDeletes := 0
	kubeCalls := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kubeCalls++
		switch {
		case strings.HasSuffix(r.URL.Path, "/secrets/"+routesSecret):
			if r.Method == "PUT" {
				var value record
				_ = json.NewDecoder(r.Body).Decode(&value)
				text, _ := secretText(value, "routes.json")
				_ = json.Unmarshal([]byte(text), &routes)
			}
			raw, _ := json.Marshal(routes)
			jsonResponse(w, 200, record{"metadata": record{"resourceVersion": "1"}, "data": record{"routes.json": base64.StdEncoding.EncodeToString(raw)}})
		case strings.Contains(r.URL.Path, "/virtualmachines/"):
			if r.Method != "GET" {
				t.Error("absent VM deletion")
			}
			jsonResponse(w, 404, record{})
		case strings.HasSuffix(r.URL.Path, "/pods"):
			jsonResponse(w, 200, record{"items": []any{}})
		case strings.HasSuffix(r.URL.Path, "/services/"+name):
			if !servicePresent {
				jsonResponse(w, 404, record{})
				return
			}
			if r.Method == "DELETE" {
				var options record
				_ = json.NewDecoder(r.Body).Decode(&options)
				if nested(options, "preconditions", "uid") != "original-expiry-service" || nested(options, "preconditions", "resourceVersion") != "1" {
					t.Error("Service identity precondition absent")
				}
				servicePresent = false
				physicalDeletes++
				jsonResponse(w, 200, record{})
				return
			}
			jsonResponse(w, 200, record{"metadata": record{"uid": "original-expiry-service", "resourceVersion": "1", "labels": credentialLabels("prj_expiry", endpoint)}, "spec": record{"type": "ClusterIP", "selector": record{"vm.neon.tech/name": name}}})
		default:
			t.Errorf("unexpected resource request %s %s", r.Method, r.URL.Path)
			jsonResponse(w, 500, record{})
		}
	}))
	defer peer.Close()
	s := &server{db: db, idempotencyKey: []byte(strings.Repeat("e", 32)), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
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
		var value record
		if json.Unmarshal(w.Body.Bytes(), &value) != nil {
			t.Fatal("invalid response")
		}
		return value
	}
	base := "/api/v1/projects/prj_expiry"
	path := base + "/branches/br_expiry_leaf/expiration"
	body := func(deadline string) string { return `{"expires_at":"` + deadline + `","confirm_name":"temporary"}` }
	future := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	request("viewer_expiry", "PATCH", path, body(future), "expiry-set", `"1"`, 403)
	request("outside_expiry", "PATCH", path, body(future), "expiry-set", `"1"`, 404)
	request("editor_expiry", "PATCH", path, body(future), "expiry-set", `"99"`, 412)
	request("editor_expiry", "PATCH", path, `{"expires_at":null,"confirm_name":"wrong"}`, "expiry-set", `"1"`, 422)
	request("editor_expiry", "PATCH", path, `{"confirm_name":"temporary"}`, "expiry-set", `"1"`, 422)
	request("editor_expiry", "PATCH", base+"/branches/br_expiry_root/expiration", `{"expires_at":null,"confirm_name":"main"}`, "expiry-root", `"1"`, 409)
	request("editor_expiry", "PATCH", base+"/branches/br_expiry_protected/expiration", `{"expires_at":null,"confirm_name":"protected"}`, "expiry-protect", `"1"`, 409)
	first := request("editor_expiry", "PATCH", path, body(future), "expiry-set", `"1"`, 202)
	policy := first["operation"].(map[string]any)["id"]
	if first["operation"].(map[string]any)["state"] != "succeeded" || kubeCalls != 0 {
		t.Fatal("policy did not commit atomically without external mutation")
	}
	replay := request("editor_expiry", "PATCH", path, body(future), "expiry-set", `"1"`, 202)
	if replay["operation"].(map[string]any)["id"] != policy {
		t.Fatal("policy replay changed")
	}
	request("admin_expiry", "PATCH", base+"/branches/br_expiry_leaf/protection", `{"confirm_name":"temporary","protected":true}`, "", `"2"`, 409)
	request("editor_expiry", "POST", base+"/branches", `{"name":"child","parent_branch_id":"br_expiry_leaf","create_endpoint":false}`, "expiry-child", `"1"`, 409)
	request("editor_expiry", "POST", base+"/endpoints", `{"branch_id":"br_expiry_leaf","type":"read_only"}`, "expiry-compute", `"1"`, 409)
	request("editor_expiry", "POST", base+"/branches/br_expiry_leaf/data-api", `{}`, "expiry-dataapi", `"1"`, 409)
	request("editor_expiry", "PATCH", path, `{"expires_at":null,"confirm_name":"temporary"}`, "expiry-clear", `"2"`, 202)
	cleared := request("editor_expiry", "GET", base+"/branches/br_expiry_leaf", "", "", "", 200)
	if cleared["expires_at"] != nil || cleared["expiration_status"] != "none" {
		t.Fatal("future policy not cleared")
	}
	due := time.Now().Add(2 * time.Second).UTC().Truncate(time.Second)
	dueBody := body(due.Format(time.RFC3339))
	request("editor_expiry", "PATCH", path, dueBody, "expiry-final", `"3"`, 202)
	time.Sleep(time.Until(due.Add(30 * time.Millisecond)))
	request("editor_expiry", "PATCH", path, `{"expires_at":null,"confirm_name":"temporary"}`, "expiry-too-late", `"4"`, 409)
	request("editor_expiry", "POST", base+"/endpoints/ep_expiry_leaf/query", `{}`, "", `"4"`, 409)
	if err = s.enqueueDueBranchExpirations(ctx); err != nil {
		t.Fatal(err)
	}
	var op string
	if err = db.QueryRow(ctx, `SELECT expiration_operation_id FROM branches WHERE id='br_expiry_leaf'`).Scan(&op); err != nil {
		t.Fatal(err)
	}
	if err = s.workOnce(ctx, "expiry-functional-worker"); err != nil {
		t.Fatal(err)
	}
	finished := request("editor_expiry", "GET", base+"/operations/"+op, "", "", "", 200)
	if finished["state"] != "succeeded" || finished["action"] != "expire_branch" || physicalDeletes != 1 || servicePresent {
		t.Fatalf("expiration not complete: %v", finished)
	}
	if routes[selector(endpoint)].(map[string]any)["state"] != "deleted" || routes["unrelated"].(map[string]any)["state"] != "active" {
		t.Fatal("route scope mismatch")
	}
	var state, status, timeline, rootState, projectState string
	if err = db.QueryRow(ctx, `SELECT b.state,b.expiration_status,b.timeline_id,r.state,p.state FROM branches b JOIN projects p ON p.id=b.project_id JOIN branches r ON r.id=p.default_branch_id WHERE b.id='br_expiry_leaf'`).Scan(&state, &status, &timeline, &rootState, &projectState); err != nil {
		t.Fatal(err)
	}
	if state != "deleted" || status != "retired" || timeline != strings.Repeat("c", 32) || rootState != "ready" || projectState != "ready" {
		t.Fatal("parent or retained timeline changed")
	}
	var held bool
	if err = db.QueryRow(ctx, `SELECT physical_gc_state='held' FROM resource_tombstones WHERE operation_id=$1`, op).Scan(&held); err != nil || !held {
		t.Fatal("held tombstone absent", err)
	}
	request("editor_expiry", "PATCH", path, dueBody, "expiry-final", `"3"`, 202)
	if err = s.enqueueDueBranchExpirations(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM operations WHERE action='expire_branch'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate expiration intent", count, err)
	}
}
