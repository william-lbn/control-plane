package control

import (
	"bytes"
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

func TestFunctionRequestJournalRealPostgresAdmissionCompletionIsolationAndPaging(t *testing.T) {
	config, e := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if e != nil || config.ConnConfig.Database != "control_ci" {
		t.Fatal("Disposable PostgreSQL required")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, config.ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "v2_fn_events_" + randomToken(6)
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	db, e := pgxpool.NewWithConfig(ctx, config)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('fn_actor','fn_actor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('fn_org','fn-org','Functions CI'),('fn_out','fn-out','Other')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('fn_org','fn_actor','admin')`)
	exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,'fn_actor',$2,now()+interval '1 hour')`, digest("fn-session"), digest("fn-csrf"))
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('fn_project','fn_org','Functions','rke2-lab',16,'ready','managed',now(),now()),('fn_other','fn_out','Other','rke2-lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('fn_branch','fn_project','main','ready',now()),('fn_other_branch','fn_other','main','ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('fn_writer','fn_project','fn_branch','fn_writer','neonvm','fn_writer','fn_writer','active','cloud_admin','neondb',now(),now())`)
	p := functionInstanceTarget{FunctionID: "fnc_0000000000000001", ProjectID: "fn_project", BranchID: "fn_branch", DeploymentID: "fdp_0000000000000001", Generation: 1, Slug: "hello"}
	instance := "fni_0000000000000001"
	exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'fn_org','fn_project','fn_branch','fn_writer','hello','neondb','app','fn_0000000000000001','provisioning')`, p.FunctionID)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('fn_op','fn_project','function',$1,'deploy_function','queued','fn_actor','fn_ci','{}')`, p.FunctionID)
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,actor_id,operation_id) VALUES($1,$2,'fn_project','fn_branch','nodejs24',repeat('a',64),100,'index.mjs','fixture','env-fixture','fn_actor','fn_op')`, p.DeploymentID, p.FunctionID)
	exec(`INSERT INTO function_instances(id,function_id,deployment_id,project_id,branch_id,generation,vm_name,service_name,bootstrap_secret_ref,state) VALUES($1,$2,$3,'fn_project','fn_branch',1,'fn-0000000000000001','fn-0000000000000001','fn-0000000000000001-bootstrap','provisioning')`, instance, p.FunctionID, p.DeploymentID)
	s := &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), idempotencyKey: bytes.Repeat([]byte{23}, 32)}
	for i, outcome := range []string{"completed", "outcome_unknown", "in_progress"} {
		r := httptest.NewRequest("GET", "/sensitive/path?token=do-not-record", nil)
		r = r.WithContext(context.WithValue(ctx, requestIDKey, newID("req_")))
		event, e := s.beginFunctionEvent(ctx, r, p, instance, "console", "GET")
		if e != nil {
			t.Fatal(e)
		}
		if outcome != "in_progress" {
			s.finishFunctionEvent(event, 200, int64(i+1), outcome)
			s.finishFunctionEvent(event, 500, 999, "completed")
		} else {
			exec(`UPDATE function_request_events SET deadline_at=now()-interval '1 second' WHERE id=$1`, event.ID)
		}
	}
	bad := p
	bad.BranchID = "fn_other_branch"
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(ctx, requestIDKey, newID("req_")))
	if _, e = s.beginFunctionEvent(ctx, r, bad, instance, "public_http", "GET"); e == nil {
		t.Fatal("cross branch journal inserted")
	}
	route := "/api/v1/projects/fn_project/branches/fn_branch/functions/hello/request-logs?limit=1"
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "fn-session"})
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("journal read failed %d", w.Code)
		}
		var body struct {
			Items []record `json:"items"`
			Next  string   `json:"next_after"`
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 {
			t.Fatal("journal pagination failed")
		}
		id := stringVal(body.Items[0]["id"])
		if seen[id] {
			t.Fatal("cursor repeated an entry")
		}
		seen[id] = true
		if strings.Contains(w.Body.String(), "do-not-record") || strings.Contains(w.Body.String(), "sensitive") {
			t.Fatal("sensitive input captured")
		}
		route = "/api/v1/projects/fn_project/branches/fn_branch/functions/hello/request-logs?limit=1&after=" + body.Next
	}
	req := httptest.NewRequest("GET", "/api/v1/projects/fn_other/branches/fn_other_branch/functions/hello/request-logs", nil)
	req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "fn-session"})
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatal("foreign tenant journal readable")
	}
	var bytes int64
	db.QueryRow(ctx, `SELECT response_bytes FROM function_request_events WHERE outcome='completed'`).Scan(&bytes)
	if bytes != 1 {
		t.Fatal("completed journal overwritten")
	}
}
func TestFunctionRequestLogCursorRejectsOtherFunction(t *testing.T) {
	c := functionLogCursor{Started: time.Now(), ID: "fre_0000000000000001", Function: "fnc_0000000000000001"}
	if _, e := decodeFunctionLogCursor(encodeFunctionLogCursor(c), "fnc_0000000000000002"); e == nil {
		t.Fatal("cross function cursor accepted")
	}
}
