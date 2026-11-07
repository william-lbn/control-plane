package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type storageGCPGFixture struct {
	s              *server
	j              *storageGCJournal
	ctx            context.Context
	project, actor string
	backing        *storageGCFakeBacking
	t              *testing.T
}

func newStorageGCPGFixture(t *testing.T) *storageGCPGFixture {
	t.Helper()
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set; disposable control_ci required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "v2_storage_gc_" + randomToken(5)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := &storageGCPGFixture{ctx: ctx, project: "prj_0123456789abcdef", actor: "gc_operator", t: t}
	f.s = &server{db: db, idempotencyKey: bytes.Repeat([]byte{0x42}, 32), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.backing = &storageGCFakeBacking{present: true}
	f.j = &storageGCJournal{db: db, storeID: strings.Repeat("a", 64), deleter: &storageGCDeleter{backing: f.backing, operators: map[string]bool{f.actor: true}, projects: map[string]bool{f.project: true}, minimumAge: time.Minute}}
	f.s.storage = &storageBlobs{gcJournal: f.j}
	f.exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','viewer')`, f.actor)
	f.exec(`INSERT INTO organizations(id,slug,name) VALUES('org_gc_fixture','gc-fixture','Disposable GC fixture')`)
	f.exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES($1,'org_gc_fixture','Disposable GC fixture','lab',16,'ready','managed',clock_timestamp(),clock_timestamp())`, f.project)
	return f
}
func (f *storageGCPGFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.s.db.Exec(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *storageGCPGFixture) call(handler http.HandlerFunc, method, key, run string, input any, actor user, project string) *httptest.ResponseRecorder {
	f.t.Helper()
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest(method, "/gc", bytes.NewReader(raw))
	r = r.WithContext(context.WithValue(f.ctx, userKey, actor))
	r.SetPathValue("project", project)
	r.SetPathValue("gcRun", run)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}
func (f *storageGCPGFixture) allocation(suffix, state string, ageSeconds int64) (string, string) {
	id := "sga_" + strings.Repeat(suffix, 24)
	key := f.project + "/blob_" + strings.Repeat(suffix, 24)
	f.exec(`INSERT INTO storage_gc_allocations(store_id,blob_key,allocation_id,project_id,size,state,created_at,resolved_at) VALUES($1,$2,$3,$4,4,$5,clock_timestamp()-interval '2 hours',CASE WHEN $5='uploading' THEN NULL ELSE clock_timestamp()-($6::bigint*interval '1 second') END)`, f.j.storeID, key, id, f.project, state, ageSeconds)
	return key, id
}

func TestStorageGCPostgresJournalIsProspectiveAndTerminal(t *testing.T) {
	f := newStorageGCPGFixture(t)
	key := f.project + "/part_" + strings.Repeat("1", 24)
	id, err := f.j.begin(f.ctx, key, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.j.begin(f.ctx, key, 4); err == nil {
		t.Fatal("same immutable key admitted twice")
	}
	if err = f.j.finish(f.ctx, key, id, true); err != nil {
		t.Fatal(err)
	}
	if err = f.j.finish(f.ctx, key, id, false); err == nil {
		t.Fatal("potentially published bytes demoted to orphan")
	}
	for _, query := range []string{`UPDATE storage_gc_allocations SET state='never_published'`, `UPDATE storage_gc_allocations SET blob_key=blob_key||'0'`, `DELETE FROM storage_gc_allocations`} {
		if _, err = f.s.db.Exec(f.ctx, query); err == nil {
			t.Fatal("allocation evidence altered", query)
		}
	}
	if _, err = f.j.begin(f.ctx, "functions/"+f.project+"/code.zip", 4); err == nil {
		t.Fatal("Functions artifact namespace adopted")
	}
	if _, err = f.j.begin(f.ctx, "prj_ffffffffffffffff/blob_"+strings.Repeat("2", 24), 4); err == nil {
		t.Fatal("nonexistent project admitted")
	}
}

func TestStorageGCPostgresPlanExecuteAndHeldReferences(t *testing.T) {
	f := newStorageGCPGFixture(t)
	key, id := f.allocation("1", "never_published", 3600)
	f.allocation("2", "may_reference", 3600) // Includes copied/shared/history/retained/MPU references.
	f.allocation("3", "uploading", 3600)     // Crash/unknown remote result never expires into an orphan.
	f.allocation("4", "never_published", 0)
	f.backing.info = minio.ObjectInfo{Size: 4, ETag: "synthetic-etag", UserMetadata: map[string]string{storageGCMetadata: id}}
	u := user{ID: f.actor}
	w := f.call(f.s.storageGCPlan, "POST", "gc-original-plan-key", "", record{"limit": 100}, u, f.project)
	if w.Code != 200 {
		t.Fatalf("plan failed: %d %s", w.Code, w.Body.String())
	}
	var plan struct {
		ID    string   `json:"id"`
		Items []record `json:"items"`
	}
	if json.Unmarshal(w.Body.Bytes(), &plan) != nil || len(plan.Items) != 1 || plan.Items[0]["allocation_id"] != id {
		t.Fatal("held/fresh allocations entered plan", w.Body.String())
	}
	if strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("backing identity leaked")
	}
	replay := f.call(f.s.storageGCPlan, "POST", "gc-original-plan-key", "", record{"limit": 100}, u, f.project)
	if replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("original plan not replayed")
	}
	conflict := f.call(f.s.storageGCPlan, "POST", "gc-original-plan-key", "", record{"limit": 1}, u, f.project)
	if conflict.Code != 409 {
		t.Fatal("changed keyed plan accepted", conflict.Code)
	}
	wrong := f.call(f.s.storageGCExecute, "POST", "", plan.ID, record{"confirm_run": "different"}, u, f.project)
	if wrong.Code != 422 || f.backing.deletes != 0 {
		t.Fatal("unreviewed plan executed")
	}
	w = f.call(f.s.storageGCExecute, "POST", "", plan.ID, record{"confirm_run": plan.ID}, u, f.project)
	if w.Code != 200 || f.backing.deletes != 1 || !strings.Contains(w.Body.String(), `"state":"deleted"`) {
		t.Fatalf("execute: %d %s deletes=%d", w.Code, w.Body.String(), f.backing.deletes)
	}
	w = f.call(f.s.storageGCExecute, "POST", "", plan.ID, record{"confirm_run": plan.ID}, u, f.project)
	if w.Code != 200 || f.backing.deletes != 1 {
		t.Fatal("execution replay repeated DELETE")
	}
	for _, query := range []string{`DELETE FROM storage_gc_runs`, `UPDATE storage_gc_runs SET project_id='other'`, `DELETE FROM storage_gc_items`, `UPDATE storage_gc_items SET blob_key=blob_key||'0'`} {
		if _, err := f.s.db.Exec(f.ctx, query); err == nil {
			t.Fatal("GC evidence mutated", query)
		}
	}
	if _, err := f.s.db.Exec(f.ctx, `DELETE FROM storage_gc_attempts`); err == nil {
		t.Fatal("GC attempt evidence erased")
	}
}

func TestStorageGCPostgresAuthorizationAndUnknownResults(t *testing.T) {
	f := newStorageGCPGFixture(t)
	_, id := f.allocation("1", "never_published", 3600)
	f.backing.info = minio.ObjectInfo{Size: 4, ETag: "synthetic-etag", UserMetadata: map[string]string{storageGCMetadata: id}}
	u := user{ID: f.actor}
	for _, actor := range []user{{ID: "org_owner", Role: "owner"}, {ID: f.actor, APIKey: true}} {
		w := f.call(f.s.storageGCPlan, "POST", "gc-denied-plan-key", "", record{"limit": 1}, actor, f.project)
		if w.Code != 403 {
			t.Fatal("unapproved principal admitted", w.Code)
		}
	}
	w := f.call(f.s.storageGCPlan, "POST", "gc-cross-project-key", "", record{"limit": 1}, u, "prj_ffffffffffffffff")
	if w.Code != 403 {
		t.Fatal("unapproved project admitted")
	}
	w = f.call(f.s.storageGCPlan, "POST", "gc-unknown-original-key", "", record{"limit": 1}, u, f.project)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var plan struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &plan)
	f.backing.versioningError = context.DeadlineExceeded
	w = f.call(f.s.storageGCExecute, "POST", "", plan.ID, record{"confirm_run": plan.ID}, u, f.project)
	if w.Code != 409 || f.backing.deletes != 0 {
		t.Fatal("versioning unknown permitted DELETE")
	}
	f.backing.versioningError = nil
	f.backing.removeError = context.DeadlineExceeded
	w = f.call(f.s.storageGCExecute, "POST", "", plan.ID, record{"confirm_run": plan.ID}, u, f.project)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"delete_unknown"`) {
		t.Fatal("lost DELETE inferred reclaimed", w.Code, w.Body.String())
	}
	// Reconcile the original run by HEAD, retaining the unknown first attempt.
	f.backing.removeError = nil
	f.backing.present = false
	w = f.call(f.s.storageGCExecute, "POST", "", plan.ID, record{"confirm_run": plan.ID}, u, f.project)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"absent"`) || f.backing.deletes != 1 {
		t.Fatal("unknown deletion was retried without inspection")
	}
	var historyCount int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM storage_gc_attempts WHERE run_id=$1 AND phase='result'`, plan.ID).Scan(&historyCount); err != nil || historyCount != 2 {
		t.Fatal("unknown attempt history lost", historyCount, err)
	}
	// Cancel after the external side effect. Its precommitted intent survives
	// even when the result transaction can no longer commit.
	parentContext := f.ctx
	interrupted, cancel := context.WithCancel(parentContext)
	f.ctx = interrupted
	f.backing.present = true
	f.backing.onRemove = cancel
	w = f.call(f.s.storageGCExecute, "POST", "", plan.ID, record{"confirm_run": plan.ID}, u, f.project)
	f.ctx = parentContext
	f.backing.onRemove = nil
	cancel()
	if w.Code != 503 {
		t.Fatal("interrupted result was reported as complete", w.Code)
	}
	var intents int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM storage_gc_attempts WHERE run_id=$1 AND phase='intent'`, plan.ID).Scan(&intents); err != nil || intents != 3 {
		t.Fatal("pre-delete durable intent lost after cancellation", intents, err)
	}
	f.exec(`UPDATE users SET disabled=true WHERE id=$1`, f.actor)
	w = f.call(f.s.storageGCRead, "GET", "", plan.ID, nil, u, f.project)
	if w.Code != 403 {
		t.Fatal("disabled operator retained access")
	}
}

func TestStorageGCPostgresJournalProgressesWhenMetadataPoolIsFull(t *testing.T) {
	f := newStorageGCPGFixture(t)
	metadataConfig := f.s.db.Config()
	metadataConfig.MinConns, metadataConfig.MaxConns = 0, 1
	metadata, err := pgxpool.NewWithConfig(f.ctx, metadataConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	writerConfig := f.s.db.Config()
	writerConfig.MinConns, writerConfig.MaxConns = 0, 2
	writer, err := pgxpool.NewWithConfig(f.ctx, writerConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	f.j.db, f.j.writerDB = metadata, writer
	blocker, err := metadata.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	key := f.project + "/part_" + strings.Repeat("f", 24)
	id, err := f.j.begin(ctx, key, 4)
	if err != nil {
		t.Fatal("journal required a second connection from exhausted metadata pool", err)
	}
	if err = f.j.finish(ctx, key, id, false); err != nil {
		t.Fatal("journal completion required metadata pool capacity", err)
	}
}

func TestStorageGCPostgresUploadRecordsBeforeBackingAndBeforePublication(t *testing.T) {
	f := newStorageGCPGFixture(t)
	var allocation string
	status := http.StatusBadRequest
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allocation = r.Header.Get("X-Amz-Meta-" + storageGCMetadata)
		if allocation == "" || r.Header.Get("If-None-Match") != "*" {
			t.Error("prospective conditional allocation metadata absent")
		}
		var state string
		if err := f.s.db.QueryRow(f.ctx, `SELECT state FROM storage_gc_allocations WHERE allocation_id=$1`, allocation).Scan(&state); err != nil || state != "uploading" {
			t.Error("backing write began before durable registration", err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("ETag", `"synthetic-etag"`)
		w.WriteHeader(status)
		if status != 200 {
			_, _ = io.WriteString(w, `<Error><Code>InvalidRequest</Code><Message>Disposable synthetic rejection</Message></Error>`)
		}
	}))
	defer peer.Close()
	client, err := minio.New(strings.TrimPrefix(peer.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("disposable-user", "disposable-secret", ""), Secure: false, Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
	if err != nil {
		t.Fatal(err)
	}
	b := &storageBlobs{client: client, bucket: "neon-product-gc-fixture", gcJournal: f.j}
	for _, code := range []int{400, 200} {
		status = code
		key := f.project + "/blob_" + strings.Repeat(map[int]string{400: "1", 200: "2"}[code], 24)
		err = b.put(f.ctx, key, "text/plain", []byte("test"))
		if (code == 200) != (err == nil) {
			t.Fatal("unexpected PUT result", code, err)
		}
		var state string
		if err = f.s.db.QueryRow(f.ctx, `SELECT state FROM storage_gc_allocations WHERE allocation_id=$1`, allocation).Scan(&state); err != nil {
			t.Fatal(err)
		}
		want := "never_published"
		if code == 200 {
			want = "may_reference"
		}
		if state != want {
			t.Fatal("publication evidence missing at PUT return", state, want)
		}
	}
}
