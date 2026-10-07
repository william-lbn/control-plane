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
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real PostgreSQL intent, middleware and leases; only the storage HTTP peer is
// simulated here. The opt-in Linux UI test separately uses the real Pageserver.
func TestHistoricalBranchIntegration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA") + "_restore"
	if !regexp.MustCompile(`^v2_auth_[a-z0-9_]+$`).MatchString(schema) || len(schema) > 63 {
		t.Fatal("fresh isolated schema required")
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_restore','restore','Restore'),('org_outside','outside','Outside')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('org_restore'),('org_outside')`)
	for _, actor := range []string{"editor_restore", "viewer_restore", "outside_restore"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_restore','editor_restore','editor'),('org_restore','viewer_restore','viewer'),('org_outside','outside_restore','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_restore','org_restore','Restore',$1,'rke2-lab',16,'ready','managed',now(),now())`, strings.Repeat("a", 32))
	parent := strings.Repeat("b", 32)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES('br_restore','prj_restore','main',$1,'ready',now())`, parent)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES('br_restore','role_created_later','ready','later-secret')`)
	exec(`INSERT INTO branch_databases(branch_id,name,owner_name,state) VALUES('br_restore','database_created_later','cloud_admin','ready')`)
	targetTime := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	var mutex sync.Mutex
	leaseCalls := 0
	kind := "present"
	latest := "0/4000"
	minimum := "0/1000"
	children := map[string]map[string]any{}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if strings.HasSuffix(r.URL.Path, "/lsn_lease") {
			leaseCalls++
			var body record
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["lsn"] != "0/2000" && body["lsn"] != "0/3000" {
				t.Error("unexpected pinned point", body)
			}
			jsonResponse(w, 200, record{"valid_until": time.Now().Add(10 * time.Minute).UTC()})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/get_lsn_by_timestamp") {
			if r.URL.Query().Get("timestamp") != targetTime.Format(time.RFC3339) {
				t.Error("timestamp normalization/query encoding")
			}
			jsonResponse(w, 200, record{"kind": kind, "lsn": "0/2000"})
			return
		}
		if strings.Contains(r.URL.Path, "storage-controller") && r.Method == "POST" {
			var body record
			_ = json.NewDecoder(r.Body).Decode(&body)
			children[stringVal(body["new_timeline_id"])] = record{"ancestor_timeline_id": body["ancestor_timeline_id"], "ancestor_lsn": body["ancestor_start_lsn"]}
			jsonResponse(w, 201, record{})
			return
		}
		if strings.HasSuffix(r.URL.Path, parent) {
			jsonResponse(w, 200, record{"min_readable_lsn": minimum, "initdb_lsn": "0/1000", "last_record_lsn": latest})
			return
		}
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if child := children[id]; child != nil {
			jsonResponse(w, 200, child)
			return
		}
		jsonResponse(w, 404, record{"message": "timeline not found"})
	}))
	defer peer.Close()
	s := &server{db: db, kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}, idempotencyKey: []byte(strings.Repeat("k", 32)), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	t.Setenv("NEON_V2_PITR_ENABLED", "true")
	h := s.routes()
	request := func(actor, method, path string, body record, key string, want int) record {
		t.Helper()
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s got %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		var v record
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	path := "/api/v1/projects/prj_restore/branches"
	input := record{"name": "history", "parent_branch_id": "br_restore", "parent_timestamp": targetTime.In(time.FixedZone("test-offset", 2*3600)).Format(time.RFC3339), "create_endpoint": false}
	request("viewer_restore", "POST", path, input, "restore-viewer", 403)
	request("outside_restore", "GET", path+"/br_restore/restore-window", nil, "", 404)
	window := request("viewer_restore", "GET", path+"/br_restore/restore-window", nil, "", 200)
	if window["min_readable_lsn"] != "0/1000" {
		t.Fatal(window)
	}
	got := request("editor_restore", "POST", path, input, "restore-same-key", 202)
	branch := stringVal(nested(got, "resource", "id"))
	operation := stringVal(nested(got, "operation", "id"))
	mutex.Lock()
	latest = "0/8000"
	mutex.Unlock()
	replay := request("editor_restore", "POST", path, input, "restore-same-key", 202)
	if nested(replay, "resource", "id") != branch || nested(replay, "operation", "id") != operation {
		t.Fatal("restore replay changed identity")
	}
	var lsn, source string
	var roles, databases int
	if err = db.QueryRow(ctx, "SELECT parent_lsn,restore_source FROM branches WHERE id=$1", branch).Scan(&lsn, &source); err != nil || lsn != "0/2000" || source != "timestamp" {
		t.Fatal(lsn, source, err)
	}
	_ = db.QueryRow(ctx, "SELECT count(*) FROM branch_roles WHERE branch_id=$1", branch).Scan(&roles)
	_ = db.QueryRow(ctx, "SELECT count(*) FROM branch_databases WHERE branch_id=$1", branch).Scan(&databases)
	if roles != 0 || databases != 0 || leaseCalls != 1 {
		t.Fatal("replayed lease or current catalog projected into history", roles, databases, leaseCalls)
	}
	var payload []byte
	if err = db.QueryRow(ctx, "SELECT payload FROM operations WHERE id=$1", operation).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p createPayload
	if err = json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE operations SET state='running',lease_owner='restore-worker',lease_expires_at=now()+interval '1 hour' WHERE id=$1`, operation)
	if err = s.reconcileCreate(ctx, operation, "restore-worker", "create_branch", p); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	minimum = "0/7000"
	mutex.Unlock()
	if err = s.reconcileCreate(ctx, operation, "restore-worker", "create_branch", p); err != nil {
		t.Fatal("created child lost replayability after source GC advanced", err)
	}
	if leaseCalls != 2 {
		t.Fatal("existing child replay must not renew expired source point", leaseCalls)
	}
	conflict := record{"name": "history", "parent_branch_id": "br_restore", "parent_lsn": "0/3000", "create_endpoint": false}
	request("editor_restore", "POST", path, conflict, "restore-same-key", 409)
	request("editor_restore", "POST", path, conflict, "restore-outside-key", 422)
	mutex.Lock()
	minimum = "0/1000"
	kind = "past"
	mutex.Unlock()
	input["name"] = "outside-history"
	request("editor_restore", "POST", path, input, "restore-past-key", 422)
	mutex.Lock()
	kind = "future"
	mutex.Unlock()
	request("editor_restore", "POST", path, input, "restore-future-key", 409)
	mutex.Lock()
	kind = "nodata"
	mutex.Unlock()
	request("editor_restore", "POST", path, input, "restore-empty-key", 422)
	t.Setenv("NEON_V2_PITR_ENABLED", "false")
	request("editor_restore", "POST", path, input, "restore-disabled-key", 503)
}
