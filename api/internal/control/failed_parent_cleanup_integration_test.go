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

func TestFailedParentCleanupIntegration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA") + "_parent_cleanup"
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
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_parent_cleanup','parent-cleanup','Parent cleanup'),('org_parent_out','parent-out','Outside')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('org_parent_cleanup'),('org_parent_out')`)
	for _, actor := range []string{"admin_parent", "editor_parent", "viewer_parent", "outside_parent"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_parent_cleanup','admin_parent','admin'),('org_parent_cleanup','editor_parent','editor'),('org_parent_cleanup','viewer_parent','viewer'),('org_parent_out','outside_parent','admin')`)
	services := map[string]string{}
	unknownDelete := false
	deletes := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/secrets/"+routesSecret):
			if r.Method != "GET" {
				t.Error("unpublished routes must not be rewritten")
			}
			jsonResponse(w, 200, record{"metadata": record{"resourceVersion": "1"}, "data": record{"routes.json": base64.StdEncoding.EncodeToString([]byte(`{}`))}})
		case strings.Contains(r.URL.Path, "/virtualmachines/"):
			if r.Method != "GET" {
				t.Error("absent VM must not be deleted")
			}
			jsonResponse(w, 404, record{})
		case strings.HasSuffix(r.URL.Path, "/pods"):
			jsonResponse(w, 200, record{"items": []any{}})
		case strings.Contains(r.URL.Path, "/services/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			uid := services[name]
			if uid == "" {
				jsonResponse(w, 404, record{})
				return
			}
			if r.Method == "DELETE" {
				var options record
				_ = json.NewDecoder(r.Body).Decode(&options)
				if nested(options, "preconditions", "uid") != uid || nested(options, "preconditions", "resourceVersion") != "1" {
					t.Error("original Service UID/version required")
				}
				deletes++
				if unknownDelete {
					jsonResponse(w, 503, record{})
					return
				}
				delete(services, name)
				jsonResponse(w, 200, record{})
				return
			}
			ep := "ep_" + strings.TrimPrefix(name, "cp-")
			project := "prj_" + strings.TrimPrefix(name, "cp-")
			jsonResponse(w, 200, record{"metadata": record{"uid": uid, "resourceVersion": "1", "labels": credentialLabels(project, ep)}, "spec": record{"type": "ClusterIP", "selector": record{"vm.neon.tech/name": name}}})
		default:
			t.Errorf("unexpected physical request %s %s", r.Method, r.URL.Path)
			jsonResponse(w, 500, record{})
		}
	}))
	defer peer.Close()
	s := &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), idempotencyKey: []byte(strings.Repeat("p", 32)), kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
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
	for _, item := range []struct {
		kind, suffix string
		compute      bool
	}{{"project", "parentproject", true}, {"branch", "parentbranch", true}, {"branch", "parentempty", false}} {
		t.Run(item.suffix, func(t *testing.T) {
			project, branch, endpoint, origin := "prj_"+item.suffix, "br_"+item.suffix, "ep_"+item.suffix, "op_"+item.suffix
			ps := "ready"
			if item.kind == "project" {
				ps = "provisioning"
			}
			tenantID := stableHex(item.suffix, "tenant")
			projectName := "Failed " + item.suffix
			exec(`INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'org_parent_cleanup',$4,$3,'rke2-lab',16,$2,'managed',now(),now())`, project, ps, tenantID, projectName)
			var parent any
			if item.kind == "branch" {
				parent = "br_root_" + item.suffix
				exec(`INSERT INTO branches(id,project_id,name,timeline_id,is_default,protected,state,created_at) VALUES($1,$2,'main',$3,true,true,'ready',now())`, parent, project, stableHex(item.suffix, "root"))
				exec(`UPDATE projects SET default_branch_id=$2 WHERE id=$1`, project, parent)
			}
			timelineID := stableHex(item.suffix, "timeline")
			exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,is_default,protected,state,created_at) VALUES($1,$2,'Failed branch',$5,$3,$4,$4,'creating',now())`, branch, project, parent, item.kind == "project", timelineID)
			if item.kind == "project" {
				exec(`UPDATE projects SET default_branch_id=$2 WHERE id=$1`, project, branch)
			}
			p := createPayload{ProjectID: project, BranchID: branch, TenantID: tenantID, TimelineID: timelineID}
			if item.compute {
				p.EndpointID = endpoint
				exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$4,'neonvm',$5,$5,'provisioning','cloud_admin','postgres','read_write',now(),now())`, endpoint, project, branch, selector(endpoint), kubeName(endpoint))
				services[kubeName(endpoint)] = "original-service-" + item.suffix
			}
			raw, _ := json.Marshal(p)
			resource := branch
			if item.kind == "project" {
				resource = project
			}
			exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,finished_at,retryable) VALUES($1,$2,$3,$4,$5,'failed','admin_parent','original-failure',$6,now(),true)`, origin, project, item.kind, resource, "create_"+item.kind, raw)
			exec(`UPDATE branches SET state='error' WHERE id=$1`, branch)
			if item.kind == "project" {
				exec(`UPDATE projects SET state='error' WHERE id=$1`, project)
			}
			if item.compute {
				exec(`UPDATE endpoints SET state='error' WHERE id=$1`, endpoint)
			}
			exec(`INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version) VALUES($1,'postgres','provisioning','provisioning','open-source-neon')`, branch)
			base := "/api/v1/projects/" + project
			path := base
			body := `{"confirm_name":"` + projectName + `"}`
			if item.kind == "branch" {
				path += "/branches/" + branch
				body = `{"confirm_name":"Failed branch"}`
			}
			key := "cleanup-" + item.suffix
			if item.kind == "project" {
				exec(`UPDATE projects SET version=2 WHERE id=$1`, project)
				request("admin_parent", "DELETE", path, body, key, `"2"`, 409)
				exec(`UPDATE projects SET version=1 WHERE id=$1`, project)
			}
			request("viewer_parent", "DELETE", path, body, key, `"1"`, 403)
			request("outside_parent", "DELETE", path, body, key, `"1"`, 404)
			if item.kind == "project" {
				request("editor_parent", "DELETE", path, body, key, `"1"`, 403)
			}
			request("admin_parent", "DELETE", path, body, key, `"99"`, 412)
			request("admin_parent", "DELETE", path, `{"confirm_name":"wrong"}`, key, `"1"`, 422)
			exec(`UPDATE operations SET payload=jsonb_set(payload,'{timeline_id}','"wrong"') WHERE id=$1`, origin)
			request("admin_parent", "DELETE", path, body, key, `"1"`, 409)
			exec(`UPDATE operations SET payload=$2,state='succeeded' WHERE id=$1`, origin, raw)
			request("admin_parent", "DELETE", path, body, key, `"1"`, 409)
			exec(`UPDATE operations SET state='failed' WHERE id=$1`, origin)
			exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,is_default,protected,state,created_at) VALUES($1,$2,'descendant',$4,$3,false,false,'ready',now())`, "br_child_"+item.suffix, project, branch, stableHex(item.suffix, "child"))
			request("admin_parent", "DELETE", path, body, key, `"1"`, 409)
			exec(`UPDATE branches SET state='deleted',deleted_at=now() WHERE id=$1`, "br_child_"+item.suffix)
			beforeDeletes := deletes
			accepted := request("admin_parent", "DELETE", path, body, key, `"1"`, 202)
			op := stringVal(accepted["operation"].(map[string]any)["id"])
			if accepted["operation"].(map[string]any)["action"] != "cleanup_failed_"+item.kind {
				t.Fatal("old Worker action used")
			}
			replay := request("admin_parent", "DELETE", path, body, key, `"1"`, 202)
			if replay["operation"].(map[string]any)["id"] != op {
				t.Fatal("different replay intent")
			}
			request("admin_parent", "POST", base+"/operations/"+origin+"/retry", "", "", "", 409)
			unknownDelete = item.compute
			if err = s.workOnce(ctx, "parent-cleanup-worker"); err != nil {
				t.Fatal(err)
			}
			if item.compute {
				failed := request("admin_parent", "GET", base+"/operations/"+op, "", "", "", 200)
				if failed["state"] != "failed" || deletes != beforeDeletes+1 {
					t.Fatal("unknown Service delete not held")
				}
				services[kubeName(endpoint)] = "replacement-service"
				unknownDelete = false
				request("admin_parent", "POST", base+"/operations/"+op+"/retry", "", "", "", 202)
				if err = s.workOnce(ctx, "parent-cleanup-worker"); err != nil {
					t.Fatal(err)
				}
				if deletes != beforeDeletes+1 {
					t.Fatal("replacement Service deleted")
				}
				delete(services, kubeName(endpoint))
				request("admin_parent", "POST", base+"/operations/"+op+"/retry", "", "", "", 202)
				if err = s.workOnce(ctx, "parent-cleanup-worker"); err != nil {
					t.Fatal(err)
				}
				var vmUID, serviceUID string
				if err = db.QueryRow(ctx, `SELECT observed_uid FROM failed_endpoint_resource_bindings WHERE operation_id=$1 AND resource_kind='vm'`, op).Scan(&vmUID); err != nil {
					t.Fatal(err)
				}
				if err = db.QueryRow(ctx, `SELECT observed_uid FROM failed_endpoint_resource_bindings WHERE operation_id=$1 AND resource_kind='service'`, op).Scan(&serviceUID); err != nil {
					t.Fatal(err)
				}
				if vmUID != "" || serviceUID != "original-service-"+item.suffix {
					t.Fatal("original witness changed")
				}
				_, err = db.Exec(ctx, `UPDATE failed_endpoint_resource_bindings SET observed_uid='replacement' WHERE operation_id=$1`, op)
				var denied *pgconn.PgError
				if !errors.As(err, &denied) || denied.Code != "23514" {
					t.Fatal("witness mutable")
				}
			}
			finished := request("admin_parent", "GET", base+"/operations/"+op, "", "", "", 200)
			if finished["state"] != "succeeded" {
				t.Fatalf("cleanup not complete: %v", finished)
			}
			replay = request("admin_parent", "DELETE", path, body, key, `"1"`, 202)
			if replay["operation"].(map[string]any)["id"] != op {
				t.Fatal("completed replay changed")
			}
			request("admin_parent", "POST", base+"/operations/"+origin+"/retry", "", "", "", 409)
			var branchState, projectState, originalState, tenant, timeline string
			var recoveryWindow *time.Time
			if err = db.QueryRow(ctx, `SELECT p.state,p.tenant_id,b.state,b.timeline_id,o.state,p.recover_until FROM projects p JOIN branches b ON b.project_id=p.id JOIN operations o ON o.project_id=p.id WHERE p.id=$1 AND b.id=$2 AND o.id=$3`, project, branch, origin).Scan(&projectState, &tenant, &branchState, &timeline, &originalState, &recoveryWindow); err != nil {
				t.Fatal(err)
			}
			expectedProject := "ready"
			if item.kind == "project" {
				expectedProject = "deleted"
			}
			if projectState != expectedProject || branchState != "deleted" || originalState != "failed" || recoveryWindow != nil || tenant != p.TenantID || timeline != p.TimelineID {
				t.Fatal("retained identities or lifecycle mismatch")
			}
		})
	}
}
