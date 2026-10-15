package control

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSavedQueryExplicitSharingIsolationAndRevoke(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, e := pgxpool.ParseConfig(dsn)
	if dsn == "" || e != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "v2_saved_" + randomToken(6)
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	for _, actor := range []string{"alice", "bob", "viewer", "outsider"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_saved','saved','Saved'),('org_other','other','Other')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_saved','alice','admin'),('org_saved','bob','editor'),('org_saved','viewer','viewer'),('org_other','outsider','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_saved','org_saved','Saved','lab',16,'ready','managed',now(),now()),('prj_other','org_other','Other','lab',16,'ready','managed',now(),now())`)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{0x42}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{2}, 32)}}}
	call := func(actor, method, path, key, version string, body any, want int) (map[string]any, *httptest.ResponseRecorder) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", version)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s status %d expected %d; body omitted", method, w.Code, want)
		}
		var v map[string]any
		json.Unmarshal(w.Body.Bytes(), &v)
		return v, w
	}

	base := "/api/v1/projects/prj_saved/saved-queries"
	sql := "SELECT 'shared-private-plaintext';"
	spec := savedQuerySpec{Name: "Explicit shared draft", SQL: sql}
	v, _ := call("alice", "POST", base, "new-private-key", "", spec, 201)
	id := v["query"].(map[string]any)["id"].(string)
	route := base + "/" + id
	call("bob", "GET", route, "", "", nil, 404)
	var original []byte
	if e = db.QueryRow(ctx, `SELECT sealed_sql FROM saved_queries WHERE id=$1`, id).Scan(&original); e != nil {
		t.Fatal(e)
	}
	share := map[string]any{"shared": true}
	v, w := call("alice", "POST", route+"/sharing", "explicit-share-key", `"1"`, share, 200)
	if v["query"].(map[string]any)["is_shared"] != true || strings.Contains(w.Body.String(), "shared-private-plaintext") {
		t.Fatal("bad sharing representation")
	}
	_, w = call("alice", "POST", route+"/sharing", "explicit-share-key", `"1"`, share, 200)
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("durable share replay missing")
	}
	call("alice", "POST", route+"/sharing", "explicit-share-key", `"1"`, map[string]any{"shared": false}, 409)
	v, w = call("bob", "GET", base+"?view=shared", "", "", nil, 200)
	if len(v["items"].([]any)) != 1 || strings.Contains(w.Body.String(), "shared-private-plaintext") {
		t.Fatal("shared listing exposed SQL or lost draft")
	}
	v, _ = call("bob", "GET", route, "", "", nil, 200)
	if v["sql"] != sql || v["can_edit"] != false {
		t.Fatal("shared read did not authenticate owner's encryption AAD")
	}
	call("bob", "POST", route+"/sharing", "foreign-share-key", `"2"`, share, 404)
	call("bob", "PATCH", route, "foreign-update-key", `"2"`, spec, 404)
	call("bob", "DELETE", route, "foreign-delete-key", `"2"`, nil, 404)
	call("viewer", "GET", route, "", "", nil, 403)
	call("outsider", "GET", route, "", "", nil, 404)
	call("alice", "POST", route+"/sharing", "stale-unshare-key", `"1"`, map[string]any{"shared": false}, 412)
	v, _ = call("bob", "GET", base, "", "", nil, 200)
	if len(v["items"].([]any)) != 0 {
		t.Fatal("shared drafts entered personal list")
	}
	call("alice", "POST", route+"/sharing", "unshare-correct-key", `"2"`, map[string]any{"shared": false}, 200)
	call("bob", "GET", route, "", "", nil, 404)
	v, _ = call("bob", "GET", base+"?view=shared", "", "", nil, 200)
	if len(v["items"].([]any)) != 0 {
		t.Fatal("unshare failed")
	}
	v, _ = call("alice", "GET", route, "", "", nil, 200)
	if v["sql"] != sql {
		t.Fatal("sharing changed original SQL")
	}
	var after []byte
	var content, generation int64
	if e = db.QueryRow(ctx, `SELECT sealed_sql,content_generation,generation FROM saved_queries WHERE id=$1`, id).Scan(&after, &content, &generation); e != nil || !bytes.Equal(after, original) || content != 1 || generation != 3 {
		t.Fatal("sharing rotated SQL ciphertext or lost optimistic version", e)
	}
	call("alice", "POST", route+"/sharing", "missing-shared-key", `"3"`, map[string]any{}, 422)
	call("alice", "POST", route+"/sharing", "missing-version-key", "", share, 428)
	call("alice", "GET", base+"?view=external", "", "", nil, 422)
}
