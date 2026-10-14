package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestStorageDeleteBatchRealPostgresAtomicVersionsAndHeldReferences(t *testing.T) {
	ctx := context.Background()
	u, e := url.Parse(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if e != nil || u.Path != "/control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	admin, e := pgx.Connect(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	suffix := newID("")
	db := "storage_batch_ci_" + suffix
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(db)); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if _, e = admin.Exec(ctx, "DROP DATABASE "+pgQuote(db)+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	u.Path = "/" + db
	conn, e := pgx.Connect(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	p := objectStoragePayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ObjectStorageSpec{Database: db}}
	tx, e := conn.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = applyObjectStorageSQLTx(ctx, tx, p); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('batch-test','private')`); e != nil {
		t.Fatal(e)
	}
	digest := strings.Repeat("a", 64)
	for _, key := range []string{"keep.txt", "aux/a.txt", "aux/中文.txt"} {
		if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('batch-test',$1,$2,$3,4,'text/plain')`, key, p.ProjectID+"/shared-immutable", digest); e != nil {
			t.Fatal(e)
		}
	}
	keys := []string{"aux/a.txt", "aux/中文.txt"}
	expected := map[string]string{keys[0]: digest, keys[1]: strings.Repeat("b", 64)}
	e = deleteStorageBatch(ctx, conn, "batch-test", keys, expected)
	var fault *storageMutationFault
	if !errors.As(e, &fault) || fault.Status != 412 {
		t.Fatal("Stale batch did not conflict", e)
	}
	var count int
	if e = conn.QueryRow(ctx, `SELECT count(*) FROM neon_storage.objects`).Scan(&count); e != nil || count != 3 {
		t.Fatal("Partial deletion on version conflict")
	}
	expected[keys[1]] = digest
	if e = deleteStorageBatch(ctx, conn, "batch-test", keys, expected); e != nil {
		t.Fatal("Observed atomic batch failed", e)
	}
	var blob string
	if e = conn.QueryRow(ctx, `SELECT blob_key FROM neon_storage.objects WHERE key='keep.txt'`).Scan(&blob); e != nil || blob != p.ProjectID+"/shared-immutable" {
		t.Fatal("Shared original reference changed")
	}
	if e = deleteStorageBatch(ctx, conn, "batch-test", []string{"already-missing"}, nil); e != nil {
		t.Fatal("S3 missing key is not idempotent", e)
	}
	if e = deleteStorageBatch(ctx, conn, "missing-bucket", []string{"anything"}, nil); !errors.As(e, &fault) || fault.Status != 404 {
		t.Fatal("Unknown bucket success")
	}
}

func TestStorageDeleteXMLBoundsAndDigestBeforeSQL(t *testing.T) {
	input, e := parseS3DeleteBody([]byte(`<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Object><Key>folder/中文.txt</Key></Object><Quiet>true</Quiet></Delete>`))
	if e != nil || !input.Quiet || input.Objects[0].Key != "folder/中文.txt" {
		t.Fatal("SDK XML rejected")
	}
	for _, bad := range []string{`<Other/>`, `<Delete/>`, `<Delete><Object><Key>a</Key></Object><Unknown>true</Unknown></Delete>`, `<Delete><Object><Key>a</Key></Object></Delete><Delete/>`, `<!DOCTYPE Delete [<!ENTITY file SYSTEM "file:///etc/passwd">]><Delete><Object><Key>&file;</Key></Object></Delete>`} {
		if _, e = parseS3DeleteBody([]byte(bad)); e == nil {
			t.Fatal("Malformed or unsupported XML accepted")
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/batch-test?delete", strings.NewReader("invalid"))
	w := httptest.NewRecorder()
	(&server{}).s3DeleteObjects(w, r, nil, "batch-test", []byte("invalid"))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "BadDigest") {
		t.Fatal("Bad digest reached SQL")
	}
}
