package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEndpointDeletionIntegration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA") + "_epdelete"
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
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_epdelete','epdelete','Endpoint deletion'),('org_out','out','Other')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('org_epdelete'),('org_out')`)
	for _, u := range []string{"admin_epdelete", "editor_epdelete", "viewer_epdelete", "outside_epdelete"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, u)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(u+"-session"), u, digest(u+"-csrf"))
	}
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_epdelete','admin_epdelete','admin'),('org_epdelete','editor_epdelete','editor'),('org_epdelete','viewer_epdelete','viewer'),('org_out','outside_epdelete','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_epdelete','org_epdelete','Endpoint project','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','rke2-lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,is_default,protected,state,created_at) VALUES('br_epdelete','prj_epdelete','main','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',true,true,'ready',now())`)
	exec(`INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state) VALUES('br_epdelete','postgres','active','active')`)
	for _, id := range []string{"ep_writer", "ep_reader1", "ep_reader2"} {
		kind := "read_only"
		if id == "ep_writer" {
			kind = "read_write"
		}
		exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,'prj_epdelete','br_epdelete',$2,'neonvm',$3,$3,'active','cloud_admin','postgres',$4,now(),now())`, id, selector(id), kubeName(id), kind)
	}
	routes := map[string]record{}
	for _, id := range []string{"ep_writer", "ep_reader1", "ep_reader2"} {
		routes[selector(id)] = record{"project_id": "prj_epdelete", "branch_id": "br_epdelete", "state": "active", "roles": record{"cloud_admin": "synthetic-verifier"}}
	}
	writes := 0
	failRouteWrite := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/secrets/"+routesSecret):
			if r.Method == http.MethodPut {
				writes++
				if failRouteWrite {
					failRouteWrite = false
					jsonResponse(w, 503, record{})
					return
				}
				var secret record
				_ = json.NewDecoder(r.Body).Decode(&secret)
				raw, _ := secretText(secret, "routes.json")
				_ = json.Unmarshal([]byte(raw), &routes)
			}
			b, _ := json.Marshal(routes)
			jsonResponse(w, 200, record{"metadata": record{"resourceVersion": "42"}, "data": record{"routes.json": base64.StdEncoding.EncodeToString(b)}})
		case strings.HasSuffix(r.URL.Path, "/pods"):
			jsonResponse(w, 200, record{"items": []any{}})
		case strings.Contains(r.URL.Path, "/timeline/") && r.Method == http.MethodGet:
			jsonResponse(w, 200, record{"state": "Active"})
		default:
			if r.Method != http.MethodGet {
				t.Errorf("unexpected physical operation: %s", r.Method)
			}
			jsonResponse(w, 404, record{})
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
		var v record
		if json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal("invalid response")
		}
		return v
	}
	base := "/api/v1/projects/prj_epdelete"
	reader := base + "/endpoints/ep_reader1"
	body := `{"confirm_selector":"ep-reader1"}`
	var operation string
	t.Run("authorization_confirmation_version_and_strict_body", func(t *testing.T) {
		request("outside_epdelete", "DELETE", reader, body, "ep-delete-key", `"1"`, 404)
		request("viewer_epdelete", "DELETE", reader, body, "ep-delete-key", `"1"`, 403)
		request("editor_epdelete", "DELETE", reader, body, "ep-delete-key", "", 428)
		request("editor_epdelete", "DELETE", reader, body, "", `"1"`, 422)
		request("editor_epdelete", "DELETE", reader, `{"confirm_selector":"wrong"}`, "ep-delete-key", `"1"`, 422)
		request("editor_epdelete", "DELETE", reader, body, "ep-delete-key", `"99"`, 412)
		request("editor_epdelete", "DELETE", reader, `{"confirm_selector":"ep-reader1","purge":true}`, "ep-delete-key", `"1"`, 422)
		if writes != 0 {
			t.Fatal("rejected request mutated Kubernetes")
		}
	})
	t.Run("enabled_backend_dependency_requires_explicit_disable", func(t *testing.T) {
		exec(`INSERT INTO object_storage_instances(branch_id,project_id,endpoint_id,generation,state,spec) VALUES('br_epdelete','prj_epdelete','ep_writer',1,'active','{}')`)
		v := request("editor_epdelete", "DELETE", base+"/endpoints/ep_writer", `{"confirm_selector":"ep-writer"}`, "ep-writer-key", `"1"`, 409)
		if v["code"] != "endpoint_has_services" {
			t.Fatal(v)
		}
		exec(`UPDATE object_storage_instances SET state='disabled' WHERE branch_id='br_epdelete'`)
	})
	t.Run("accepted_intent_closes_new_work_and_replays_same_operation", func(t *testing.T) {
		v := request("editor_epdelete", "DELETE", reader, body, "ep-delete-key", `"1"`, 202)
		operation = stringVal(v["operation"].(map[string]any)["id"])
		if v["resource"].(map[string]any)["state"] != "deleting" {
			t.Fatal("deletion admission not closed")
		}
		v = request("editor_epdelete", "DELETE", reader, body, "ep-delete-key", `"1"`, 202)
		if stringVal(v["operation"].(map[string]any)["id"]) != operation {
			t.Fatal("lost reply replay created a second operation")
		}
		request("editor_epdelete", "GET", reader+"/connection-info", "", "", "", 410)
		request("editor_epdelete", "DELETE", reader, `{"confirm_selector":"different"}`, "ep-delete-key", `"1"`, 409)
		if _, e := s.queueSuspend(ctx, suspendPayload{ProjectID: "prj_epdelete", EndpointID: "ep_reader1", WorkloadName: kubeName("ep_reader1")}, "system:idle-controller", "stale-sample"); e == nil {
			t.Fatal("stale idle sample admitted during deletion")
		}
		_, e := db.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('op_late','prj_epdelete','endpoint','ep_reader1','update_endpoint','queued','editor_epdelete','test','{}')`)
		var pe *pgconn.PgError
		if !errors.As(e, &pe) || pe.Code != "23514" {
			t.Fatal("durable admission did not reject a stale mutation", e)
		}
	})
	t.Run("unknown_proxy_write_failure_retains_intent_and_retry_identity", func(t *testing.T) {
		failRouteWrite = true
		if e := s.workOnce(ctx, "original-worker"); e != nil {
			t.Fatal(e)
		}
		v := request("editor_epdelete", "GET", base+"/operations/"+operation, "", "", "", 200)
		if v["state"] != "failed" || v["retryable"] != true || writes != 1 {
			t.Fatal("uncertain write outcome was silently retried", v)
		}
		v = request("editor_epdelete", "POST", base+"/operations/"+operation+"/retry", "", "", "", 202)
		if v["id"] != operation {
			t.Fatal("retry identity changed")
		}
		if e := s.workOnce(ctx, "successor-worker"); e != nil {
			t.Fatal(e)
		}
		v = request("editor_epdelete", "GET", base+"/operations/"+operation, "", "", "", 200)
		if v["state"] != "succeeded" || v["attempts"] != float64(2) {
			t.Fatal(v)
		}
	})
	t.Run("only_one_endpoint_retires_and_branch_identity_survives", func(t *testing.T) {
		var n int
		if e := db.QueryRow(ctx, `SELECT count(*) FROM endpoints WHERE project_id='prj_epdelete' AND state='active' AND deleted_at IS NULL`).Scan(&n); e != nil || n != 2 {
			t.Fatal("other computes were affected", e, n)
		}
		if e := db.QueryRow(ctx, `SELECT count(*) FROM branches WHERE id='br_epdelete' AND state='ready' AND deleted_at IS NULL AND protected`).Scan(&n); e != nil || n != 1 {
			t.Fatal("branch identity or protection changed", e)
		}
		if routes[selector("ep_reader1")]["state"] != "deleted" || routes[selector("ep_writer")]["state"] != "active" || routes[selector("ep_reader2")]["state"] != "active" {
			t.Fatal("route scope escaped the deleted endpoint")
		}
		v := request("editor_epdelete", "DELETE", reader, body, "ep-delete-key", `"1"`, 202)
		if v["operation"].(map[string]any)["state"] != "succeeded" {
			t.Fatal("completed replay lost its operation")
		}
		request("editor_epdelete", "GET", reader, "", "", "", 404)
		v = request("viewer_epdelete", "GET", base+"/lifecycle", "", "", "", 200)
		tombstones := v["tombstones"].([]any)
		if len(tombstones) != 1 || tombstones[0].(map[string]any)["resource_type"] != "endpoint" || tombstones[0].(map[string]any)["physical_gc_state"] != "held" {
			t.Fatal("endpoint audit/tombstone missing")
		}
	})
	t.Run("project_recovery_excludes_the_previously_retired_endpoint", func(t *testing.T) {
		exec(`UPDATE branches SET protected=false WHERE id='br_epdelete'`)
		exec(`UPDATE projects SET protected=false WHERE id='prj_epdelete'`)
		v := request("admin_epdelete", "DELETE", base, `{"confirm_name":"Endpoint project"}`, "delete-project-key", `"1"`, 202)
		id := stringVal(v["operation"].(map[string]any)["id"])
		if e := s.workOnce(ctx, "delete-project-worker"); e != nil {
			t.Fatal(e)
		}
		v = request("admin_epdelete", "GET", base+"/operations/"+id, "", "", "", 200)
		if v["state"] != "succeeded" {
			t.Fatal(v)
		}
		v = request("admin_epdelete", "GET", base+"/lifecycle", "", "", "", 200)
		version := v["project"].(map[string]any)["version"]
		b, _ := json.Marshal(version)
		request("admin_epdelete", "POST", base+"/recover", `{"confirm_name":"Endpoint project"}`, "recover-project-key", `"`+string(b)+`"`, 202)
		if e := s.workOnce(ctx, "recover-project-worker"); e != nil {
			t.Fatal(e)
		}
		var live, retired int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM endpoints WHERE deleted_at IS NULL AND project_id='prj_epdelete'`).Scan(&live)
		_ = db.QueryRow(ctx, `SELECT count(*) FROM endpoints WHERE id='ep_reader1' AND deleted_at IS NOT NULL AND deletion_operation_id=$1`, operation).Scan(&retired)
		if live != 2 || retired != 1 || routes[selector("ep_reader1")]["state"] != "deleted" {
			t.Fatal("project recovery resurrected an independently deleted endpoint")
		}
	})
	t.Run("replacement_writer_preserves_retained_role_and_probe_credentials", func(t *testing.T) {
		exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at,deleted_at) VALUES('ep_prior','prj_epdelete','br_epdelete','ep-prior','neonvm','cp-prior','cp-prior','deleted','cloud_admin','postgres','read_write',now(),now(),now())`)
		password := "existing-branch-password"
		adminVerifier, e := scramVerifier(password)
		if e != nil {
			t.Fatal(e)
		}
		probeVerifier, e := scramVerifier("retained-probe-password")
		if e != nil {
			t.Fatal(e)
		}
		encoded := func(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) }
		config, _ := json.Marshal(record{"spec": record{"cluster": record{"roles": []any{record{"name": "cloud_admin", "encrypted_password": adminVerifier}, record{"name": "control_probe", "encrypted_password": probeVerifier}}}}})
		newSecrets := 0
		credentialPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				newSecrets++
				var secret record
				_ = json.NewDecoder(r.Body).Decode(&secret)
				if !owned(secret, "prj_epdelete", "ep_replacement") || nested(secret, "data", "adminVerifier") != encoded(adminVerifier) || nested(secret, "data", "probeVerifier") != encoded(probeVerifier) || nested(secret, "data", "controlSigningSeed") != nil {
					t.Error("replacement credentials rotated branch roles or copied endpoint signing identity")
				}
				jsonResponse(w, 201, secret)
				return
			}
			data := record{"adminVerifier": encoded(adminVerifier), "probePassword": encoded("retained-probe-password"), "probeVerifier": encoded(probeVerifier)}
			if strings.HasSuffix(r.URL.Path, "-config") {
				data = record{"config.json": encoded(string(config))}
			}
			jsonResponse(w, 200, record{"metadata": record{"labels": credentialLabels("prj_epdelete", "ep_prior")}, "data": data})
		}))
		defer credentialPeer.Close()
		service := &server{db: db, kube: &kubeClient{base: credentialPeer.URL, namespace: "neon", http: credentialPeer.Client()}}
		payload := createPayload{ProjectID: "prj_epdelete", BranchID: "br_epdelete", EndpointID: "ep_replacement"}
		if e = service.reserveEndpointCredentials(ctx, payload, "different-branch-password"); !errors.Is(e, errBranchPasswordMismatch) || newSecrets != 0 {
			t.Fatal("compute creation implicitly rotated branch credentials", e)
		}
		if e = service.reserveEndpointCredentials(ctx, payload, password); e != nil || newSecrets != 1 {
			t.Fatal("replacement writer credential reservation failed", e)
		}
	})
}
