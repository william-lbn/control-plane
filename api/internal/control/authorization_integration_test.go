package control

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests the actual HTTP middleware and PostgreSQL membership/grant/key records.
// No Kubernetes writes, fake authorization repository or production schema.
func TestTenantAuthorizationIntegration(t *testing.T) {
	url, schema := os.Getenv("NEON_V2_TEST_DATABASE_URL"), os.Getenv("NEON_V2_TEST_SCHEMA")
	// A full-suite CI run uses a separate authorization schema so it cannot
	// share fixture rows with migration, fencing or idle-controller tests.
	if isolated := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA"); isolated != "" {
		schema = isolated
	}
	if url == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	if !regexp.MustCompile(`^v2_auth_[a-z0-9_]+$`).MatchString(schema) {
		t.Fatal("dedicated v2_auth_* schema required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal("use a new schema for each attempt:", err)
	}
	cfg, err := pgxpool.ParseConfig(url)
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
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		// All global roles intentionally say owner. They must not bypass tenancy.
		exec(t, `INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, id)
		exec(t, `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(id+"-session"), id, digest(id+"-csrf"))
	}
	exec(t, `INSERT INTO organizations(id,slug,name) VALUES('org_a','a','A'),('org_b','b','B')`)
	exec(t, `INSERT INTO organization_quotas(org_id) VALUES('org_a'),('org_b')`)
	exec(t, `INSERT INTO organization_members(org_id,user_id,role) VALUES('org_a','alice','admin'),('org_a','bob','viewer'),('org_a','carol','collaborator'),('org_b','dave','admin')`)
	exec(t, `INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at)
        VALUES('prj_a','org_a','A','rke2-lab',16,'ready','managed',now(),now()),
        ('prj_a2','org_a','A2','rke2-lab',16,'ready','managed',now(),now()),
        ('prj_b','org_b','B','rke2-lab',16,'ready','managed',now(),now())`)
	exec(t, `INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES('br_a','prj_a','main','timeline-a','ready',now())`)
	exec(t, `INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at)
        VALUES('ep_a','prj_a','br_a','ep-a','neonvm','cp-a','cp-a','active','app','postgres',now(),now())`)
	s := &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), proxyHost: "proxy.invalid", proxyPort: "5432"}
	handler := s.routes()
	request := func(t *testing.T, actor, method, path, body, token string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
			req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
			req.Header.Set("X-CSRF-Token", actor+"-csrf")
		}
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("%s %s returned %d, wanted %d", method, path, res.Code, want)
		}
		var value map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("global owner has no cross organization bypass", func(t *testing.T) {
		request(t, "alice", "GET", "/api/v1/projects/prj_b", "", "", 404)
		request(t, "alice", "GET", "/api/v1/organizations/org_b/projects", "", "", 404)
		request(t, "bob", "POST", "/api/v1/projects/prj_a/endpoints/ep_a/query", `{"sql":"SELECT 1"}`, "", 403)
		request(t, "bob", "GET", "/api/v1/projects/prj_a/endpoints/ep_a/connection-info", "", "", 403)
		got := request(t, "carol", "GET", "/api/v1/organizations/org_a/projects", "", "", 200)
		if len(got["items"].([]any)) != 0 {
			t.Fatal("ungranted collaborator saw projects")
		}
		request(t, "carol", "GET", "/api/v1/projects/prj_a", "", "", 404)
	})
	t.Run("additive project grants and immediate revoke", func(t *testing.T) {
		request(t, "alice", "PUT", "/api/v1/projects/prj_a/permissions/carol", `{"role":"viewer"}`, "", 200)
		got := request(t, "carol", "GET", "/api/v1/organizations/org_a/projects", "", "", 200)
		if len(got["items"].([]any)) != 1 {
			t.Fatal("collaborator grant did not filter list")
		}
		request(t, "carol", "GET", "/api/v1/projects/prj_a/endpoints/ep_a/connection-info", "", "", 403)
		request(t, "alice", "PUT", "/api/v1/projects/prj_a/permissions/bob", `{"role":"editor"}`, "", 200)
		request(t, "bob", "GET", "/api/v1/projects/prj_a/endpoints/ep_a/connection-info", "", "", 200)
		request(t, "alice", "DELETE", "/api/v1/projects/prj_a/permissions/bob", "", "", 200)
		request(t, "bob", "GET", "/api/v1/projects/prj_a/endpoints/ep_a/connection-info", "", "", 403)
		request(t, "alice", "PUT", "/api/v1/projects/prj_a/permissions/dave", `{"role":"admin"}`, "", 404)
		request(t, "bob", "PUT", "/api/v1/projects/prj_a/permissions/bob", `{"role":"admin"}`, "", 403)
	})
	t.Run("keys are hashed scoped capped expiring and revocable", func(t *testing.T) {
		key := request(t, "alice", "POST", "/api/v1/organizations/org_a/api-keys", `{"name":"scoped","project_id":"prj_a","max_role":"viewer","expires_days":1}`, "", 201)
		token, id := key["token"].(string), key["id"].(string)
		var stored string
		if err := db.QueryRow(ctx, `SELECT key_hash FROM api_keys WHERE id=$1`, id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored == token || stored != digest(token) {
			t.Fatal("plaintext token stored or incorrect digest")
		}
		request(t, "", "GET", "/api/v1/projects/prj_a", "", token, 200)
		request(t, "", "GET", "/api/v1/projects/prj_a2", "", token, 404)
		request(t, "", "GET", "/api/v1/projects/prj_b", "", token, 404)
		request(t, "", "GET", "/api/v1/projects/prj_a/endpoints/ep_a/connection-info", "", token, 403)
		request(t, "", "POST", "/api/v1/organizations/org_a/projects", `{}`, token, 403)
		request(t, "", "GET", "/api/v1/organizations/org_a/api-keys", "", token, 404)
		list := request(t, "alice", "GET", "/api/v1/organizations/org_a/api-keys", "", "", 200)
		if strings.Contains(fmt.Sprint(list), token) {
			t.Fatal("token exposed by listing")
		}
		request(t, "alice", "DELETE", "/api/v1/organizations/org_a/api-keys/"+id, "", "", 200)
		request(t, "", "GET", "/api/v1/projects/prj_a", "", token, 401)
		key = request(t, "alice", "POST", "/api/v1/organizations/org_a/api-keys", `{"name":"expired","max_role":"admin","expires_days":1}`, "", 201)
		exec(t, `UPDATE api_keys SET expires_at=now()-interval '1 second' WHERE id=$1`, key["id"])
		request(t, "", "GET", "/api/v1/projects/prj_a", "", key["token"].(string), 401)
	})
	t.Run("last administrator survives concurrent demotions", func(t *testing.T) {
		request(t, "alice", "PATCH", "/api/v1/organizations/org_a/members/bob", `{"role":"admin"}`, "", 200)
		var wg sync.WaitGroup
		var mu sync.Mutex
		success := 0
		for _, actor := range []string{"alice", "bob"} {
			wg.Add(1)
			go func(actor string) {
				defer wg.Done()
				req := httptest.NewRequest("PATCH", "/api/v1/organizations/org_a/members/"+actor, strings.NewReader(`{"role":"viewer"}`))
				req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
				req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
				req.Header.Set("X-CSRF-Token", actor+"-csrf")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code == 200 {
					mu.Lock()
					success++
					mu.Unlock()
				} else if res.Code != 409 && res.Code != 403 {
					t.Errorf("unexpected concurrent demotion status %d", res.Code)
				}
			}(actor)
		}
		wg.Wait()
		var remaining int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE org_id='org_a' AND role IN ('owner','admin')`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 1 || success != 1 {
			t.Fatal("last administrator invariant failed")
		}
	})
	t.Run("bootstrap does not restore revoked access", func(t *testing.T) {
		exec(t, `INSERT INTO users(id,username,password_hash,role) VALUES('usr_local_admin','admin','unused','owner')`)
		if err := s.bootstrapAdmin(ctx); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE user_id='usr_local_admin'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("bootstrap silently restored revoked membership")
		}
	})
}
