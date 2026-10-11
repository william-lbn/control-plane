package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Only a disposable PostgreSQL schema is used. Keep it as evidence; no lab
// metadata, user branch SQL, VM, bootstrap Secret or blob is touched here.
func TestFunctionsMetadataIsolationImmutabilityAndCapacity(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_functions_" + randomToken(6)
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
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	reject := func(query string, args ...any) {
		t.Helper()
		_, err := db.Exec(ctx, query, args...)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || (pgerr.Code != "23514" && pgerr.Code != "23503" && pgerr.Code != "23505") {
			t.Fatal("database boundary did not reject", err)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('fn_actor','fn_actor','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('fn_org','fn-org','Functions CI'),('fn_out','fn-out','Other tenant')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('fn_org','fn_actor','admin')`)
	exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,'fn_actor',$2,now()+interval '1 hour')`, digest("fn-session"), digest("fn-csrf"))
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('fn_project','fn_org','Functions','rke2-lab',16,'ready','managed',now(),now()),('fn_other','fn_out','Other','rke2-lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('fn_branch','fn_project','main','ready',now()),('fn_child','fn_project','child','ready',now()),('fn_other_branch','fn_other','main','ready',now())`)
	for _, row := range [][3]string{{"fn_writer", "fn_project", "fn_branch"}, {"fn_child_writer", "fn_project", "fn_child"}, {"fn_other_writer", "fn_other", "fn_other_branch"}} {
		exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','neondb',now(),now())`, row[0], row[1], row[2])
	}
	definition := `INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,'fn_org','fn_project',$2,$3,$4,'neondb','app','fn_'||substring($1 from 5),'provisioning')`
	first := "fnc_0000000000000001"
	second := "fnc_0000000000000002"
	exec(definition, first, "fn_branch", "fn_writer", "hello")
	exec(definition, second, "fn_child", "fn_child_writer", "hello")
	reject(definition, "fnc_ffffffffffffffff", "fn_branch", "fn_other_writer", "wrong")
	third := "fnc_0000000000000003"
	exec(definition, third, "fn_branch", "fn_writer", "third")
	reject(`UPDATE function_definitions SET org_id='fn_out' WHERE id=$1`, first)
	reject(`UPDATE function_definitions SET slug='changed' WHERE id=$1`, first)
	reject(`UPDATE function_definitions SET state='active' WHERE id=$1`, first)
	deployment := func(id, fn, branch, op string) {
		t.Helper()
		exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,'fn_project','function',$2,'deploy_function','queued','fn_actor','fn_ci',$3::jsonb)`, op, fn, fmt.Sprintf(`{"branch_id":%q}`, branch))
		exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,environment_names,actor_id,operation_id) VALUES($1,$2,'fn_project',$3,'nodejs24',$4,100,'index.mjs',$5,$6,ARRAY['APP','TOKEN'],'fn_actor',$7)`, id, fn, branch, strings.Repeat("a", 64), "functions/"+id, "env-"+strings.TrimPrefix(id, "fdp_"), op)
	}
	a := "fdp_0000000000000001"
	b := "fdp_0000000000000002"
	c := "fdp_0000000000000003"
	deployment(a, first, "fn_branch", "fn_op_a")
	deployment(b, second, "fn_child", "fn_op_b")
	deployment(c, third, "fn_branch", "fn_op_c")
	reject(`UPDATE function_definitions SET target_deployment_id=$2 WHERE id=$1`, first, b)
	exec(`UPDATE function_definitions SET target_deployment_id=$2 WHERE id=$1`, first, a)
	reject(`UPDATE function_deployments SET bundle_digest=$2 WHERE id=$1`, a, strings.Repeat("b", 64))
	reject(`UPDATE function_deployments SET environment_names=ARRAY['TOKEN','APP'] WHERE id=$1`, a)
	exec(`UPDATE function_deployments SET state='building' WHERE id=$1`, a)
	reject(`UPDATE function_deployments SET state='pending' WHERE id=$1`, a)
	exec(`UPDATE function_deployments SET state='completed',finished_at=now() WHERE id=$1`, a)
	reject(`UPDATE function_deployments SET finished_at=now()+interval '1 second' WHERE id=$1`, a)
	exec(`UPDATE function_definitions SET active_deployment_id=$2,state='active' WHERE id=$1`, first, a)
	instance := `INSERT INTO function_instances(id,function_id,deployment_id,project_id,branch_id,generation,vm_name,service_name,bootstrap_secret_ref,state) VALUES($1,$2,$3,'fn_project',$4,1,$5,$5,$5||'-bootstrap','provisioning')`
	exec(instance, "fni_0000000000000001", first, a, "fn_branch", "fn-0000000000000001")
	reject(instance, "fni_0000000000000002", first, b, "fn_branch", "fn-0000000000000002")
	// Two concurrent callers contend for the last global VM slot. Exactly one
	// intent wins; the failed health state must continue to hold its slot.
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 2; i <= 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fn, dep, branch := second, b, "fn_child"
			if i == 3 {
				fn, dep, branch = third, c, "fn_branch"
			}
			_, err := db.Exec(ctx, instance, fmt.Sprintf("fni_%016x", i), fn, dep, branch, fmt.Sprintf("fn-%016x", i))
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || (pgerr.Code != "23514" && pgerr.Code != "23505") {
				t.Fatal("unexpected concurrent admission error", err)
			}
		}
	}
	if wins != 1 {
		t.Fatal("last VM slot did not admit exactly one caller", wins)
	}
	exec(`UPDATE function_instances SET state='failed' WHERE id='fni_0000000000000001'`)
	reject(instance, "fni_0000000000000004", first, a, "fn_branch", "fn-0000000000000004")
	exec(`UPDATE function_instances SET state='retired',retired_at=now() WHERE id='fni_0000000000000001'`)
	reject(`UPDATE function_instances SET state='provisioning',retired_at=NULL WHERE id='fni_0000000000000001'`)
	exec(instance, "fni_0000000000000004", first, a, "fn_branch", "fn-0000000000000004")
	exec(`UPDATE function_instances SET vm_uid='11111111-1111-1111-1111-111111111111' WHERE id='fni_0000000000000004'`)
	reject(`UPDATE function_instances SET vm_uid='22222222-2222-2222-2222-222222222222' WHERE id='fni_0000000000000004'`)
	reject(`UPDATE function_instances SET boot_id=$1,state='ready' WHERE id='fni_0000000000000004'`, strings.Repeat("a", 32))
	for i := 4; i <= 8; i++ {
		exec(definition, fmt.Sprintf("fnc_%016x", i), "fn_branch", "fn_writer", fmt.Sprintf("f%d", i))
	}
	reject(definition, "fnc_0000000000000009", "fn_branch", "fn_writer", "f9")
	exec(`UPDATE function_definitions SET state='deleted',deleted_at=now() WHERE id='fnc_0000000000000008'`)
	exec(definition, "fnc_0000000000000009", "fn_branch", "fn_writer", "f9")
	// No public Functions Driver exists yet. Existing endpoint/parent APIs
	// must refuse retirement beneath both retained definitions and guests.
	exec(`UPDATE operations SET state='succeeded',finished_at=now() WHERE resource_type='function'`)
	exec(`UPDATE projects SET protected=false WHERE id='fn_project'`)
	s := &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), idempotencyKey: bytes.Repeat([]byte{23}, 32)}
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	for _, item := range []struct{ path, body, code string }{
		{"/api/v1/projects/fn_project/endpoints/fn_writer", `{"confirm_selector":"fn_writer"}`, "endpoint_has_services"},
		{"/api/v1/projects/fn_project/branches/fn_child", `{"confirm_name":"child"}`, "functions_retirement_required"},
		{"/api/v1/projects/fn_project", `{"confirm_name":"Functions"}`, "functions_retirement_required"},
	} {
		req := httptest.NewRequest(http.MethodDelete, item.path, strings.NewReader(item.body))
		req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "fn-session"})
		req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "fn-csrf"})
		req.Header.Set("X-CSRF-Token", "fn-csrf")
		req.Header.Set("Idempotency-Key", "fn-parent-"+item.code)
		req.Header.Set("If-Match", `"1"`)
		response := httptest.NewRecorder()
		s.routes().ServeHTTP(response, req)
		if response.Code != 409 || !strings.Contains(response.Body.String(), item.code) {
			t.Fatalf("unsafe parent retirement not blocked: status %d code %s", response.Code, item.code)
		}
	}
}
