package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFunctionsReadAuthorizationPaginationAndSecretBoundary(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_functions_read_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
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
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('fn_owner','fn_owner','unused','owner'),('fn_viewer','fn_viewer','unused','viewer'),('fn_stranger','fn_stranger','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('fn_org','fn-read','Read tests'),('fn_out','fn-out-read','Other')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('fn_org','fn_owner','owner'),('fn_org','fn_viewer','viewer'),('fn_out','fn_stranger','owner')`)
	for _, u := range []string{"fn_owner", "fn_viewer", "fn_stranger"} {
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(u+"-session"), u, digest(u+"-csrf"))
	}
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('fn_project','fn_org','Functions','rke2-lab',16,'ready','managed',now(),now()),('fn_other','fn_out','Other','rke2-lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('fn_branch','fn_project','main','ready',now()),('fn_child','fn_project','child','ready',now()),('fn_other_branch','fn_other','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('fn_writer','fn_project','fn_branch','fn_writer','neonvm','fn_writer','fn_writer','active','cloud_admin','neondb',now(),now()),('fn_child_writer','fn_project','fn_child','fn_child_writer','neonvm','fn_child_writer','fn_child_writer','active','cloud_admin','neondb',now(),now())`)
	for i, slug := range []string{"alpha", "beta", "retired"} {
		id := fmt.Sprintf("fnc_%016x", i+1)
		state := "provisioning"
		exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'fn_org','fn_project','fn_branch','fn_writer',$2,'neondb','app',$3,$4)`, id, slug, "fn_"+strings.TrimPrefix(id, "fnc_"), state)
		if slug == "retired" {
			exec(`UPDATE function_definitions SET state='deleted',deleted_at=now() WHERE id=$1`, id)
		}
	}
	for i := 1; i <= 2; i++ {
		op := fmt.Sprintf("fn_read_op%d", i)
		id := fmt.Sprintf("fdp_%016x", i)
		exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,'fn_project','function','fnc_0000000000000001','deploy_function','succeeded','fn_owner','fn_read','{}')`, op)
		exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,environment_names,state,actor_id,operation_id,finished_at) VALUES($1,'fnc_0000000000000001','fn_project','fn_branch','nodejs24',$2,100,'index.mjs','private-artifact-key','private-secret-reference',ARRAY['APP','TOKEN'],'completed','fn_owner',$3,now())`, id, strings.Repeat("a", 64), op)
	}
	s := &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := s.middleware(s.routes())
	request := func(actor, path string) (int, map[string]any, string) {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		if actor != "" {
			r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-artifact-key", "private-secret-reference", "environment_secret_ref", "artifact_key", "manager_key", "database_url"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("private backing data leaked", secret)
			}
		}
		return w.Code, body, w.Header().Get("ETag")
	}
	base := "/api/v1/projects/fn_project/branches/fn_branch/functions"
	t.Run("backend_pg_bigint_etags", func(t *testing.T) {
		for _, table := range []string{"data_api_instances", "managed_auth_instances"} {
			exec(`INSERT INTO ` + table + `(branch_id,project_id,endpoint_id,generation,state,spec,secret_ref) VALUES('fn_branch','fn_project','fn_writer',9007199254740993,'disabled','{}','unused')`)
		}
		exec(`INSERT INTO object_storage_instances(branch_id,project_id,endpoint_id,generation,state,spec) VALUES('fn_branch','fn_project','fn_writer',9007199254740993,'disabled','{}')`)
		for _, service := range []string{"data-api", "auth", "storage"} {
			code, _, etag := request("fn_owner", "/api/v1/projects/fn_project/branches/fn_branch/"+service)
			if code != 200 || etag != `"9007199254740993"` {
				t.Fatal(service, code, etag)
			}
		}
		for _, service := range []string{"data-api", "auth", "storage"} {
			code, _, etag := request("fn_owner", "/api/v1/projects/fn_project/branches/fn_child/"+service)
			if code != 200 || etag != `"0"` {
				t.Fatal("unconfigured service", service, code, etag)
			}
		}
		var cpu int32
		var generation int64
		if err := db.QueryRow(ctx, `SELECT 1000::integer,7::bigint`).Scan(&cpu, &generation); err != nil || number(cpu) != 1000 || number(generation) != 7 {
			t.Fatal("PG numeric metadata converted to zero", err)
		}
	})
	t.Run("viewer_paginated_metadata", func(t *testing.T) {
		code, b, _ := request("fn_viewer", base+"?limit=1")
		if code != 200 || b["next_cursor"] != "alpha" || b["driver_enabled"] != false {
			t.Fatal(code, b)
		}
		items := b["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["slug"] != "alpha" {
			t.Fatal(b)
		}
		code, b, _ = request("fn_viewer", base+"?limit=1&after=alpha")
		if code != 200 || b["next_cursor"] != nil || b["items"].([]any)[0].(map[string]any)["slug"] != "beta" {
			t.Fatal(code, b)
		}
	})
	t.Run("scope_etag_and_no_fake_url", func(t *testing.T) {
		code, b, etag := request("fn_owner", base+"/alpha")
		if code != 200 || etag != `"1"` || b["invocation_url"] != nil || b["runtime_observed"] != false {
			t.Fatal(code, b, etag)
		}
	})
	t.Run("deployment_page_no_secret_values", func(t *testing.T) {
		code, b, _ := request("fn_viewer", base+"/alpha/deployments?limit=1")
		if code != 200 || b["next_cursor"] != "fdp_0000000000000001" {
			t.Fatal(code, b)
		}
		code, b, _ = request("fn_viewer", base+"/alpha/deployments?after=fdp_0000000000000001")
		if code != 200 || len(b["items"].([]any)) != 1 || b["next_cursor"] != nil {
			t.Fatal(code, b)
		}
	})
	for _, item := range []struct {
		name, actor, path string
		want              int
	}{{"anonymous", "", base, 401}, {"cross_tenant", "fn_stranger", base, 404}, {"sibling_branch", "fn_owner", "/api/v1/projects/fn_project/branches/fn_child/functions/alpha", 404}, {"cross_project_branch", "fn_owner", "/api/v1/projects/fn_project/branches/fn_other_branch/functions", 404}, {"retired_hidden", "fn_owner", base + "/retired", 404}, {"invalid_slug", "fn_owner", base + "/with-hyphen", 404}, {"invalid_limit", "fn_owner", base + "?limit=101", 422}, {"invalid_cursor", "fn_owner", base + "?after=x-y", 422}, {"invalid_deployment_cursor", "fn_owner", base + "/alpha/deployments?after=other", 422}} {
		t.Run(item.name, func(t *testing.T) {
			code, _, _ := request(item.actor, item.path)
			if code != item.want {
				t.Fatal(code, item.want)
			}
		})
	}
}
