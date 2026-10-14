package control

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestStorageCORSMatchingAndBrowserHeaders(t *testing.T) {
	rules := []storageCORSRule{{Origins: []string{"https://*.example.test", "http://127.0.0.1:19001"}, Methods: []string{"GET", "PUT"}, Headers: []string{"content-type", "x-amz-meta-*", "if-none-match"}, Expose: []string{"ETag", "x-amz-meta-purpose"}, MaxAge: 60}}
	if !validStorageCORS(rules) {
		t.Fatal("valid rules denied")
	}
	for _, origin := range []string{"https://app.example.test", "http://127.0.0.1:19001"} {
		rule := matchStorageCORS(rules, origin, "PUT", []string{"Content-Type", "x-amz-meta-purpose"})
		if rule == nil {
			t.Fatal("first matching rule missing")
		}
		w := httptest.NewRecorder()
		applyStorageCORS(w, rule, origin, []string{"content-type"}, true)
		if w.Header().Get("Access-Control-Allow-Origin") != origin || w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("wrong browser capability boundary")
		}
	}
	for _, origin := range []string{"null", "https://example.test.evil", "https://evil.test"} {
		if matchStorageCORS(rules, origin, "PUT", nil) != nil {
			t.Error("foreign origin allowed")
		}
	}
	if matchStorageCORS(rules, "https://app.example.test", "DELETE", nil) != nil || matchStorageCORS(rules, "https://app.example.test", "PUT", []string{"authorization"}) != nil {
		t.Fatal("unauthorized method/header allowed")
	}
	if validStorageCORS([]storageCORSRule{{Origins: []string{"https://user:pass@host"}, Methods: []string{"PUT"}}}) {
		t.Fatal("userinfo origin accepted")
	}
	for _, body := range []string{`<CORSConfiguration><Unknown/></CORSConfiguration>`, `<!DOCTYPE x SYSTEM "file:///etc/passwd"><CORSConfiguration/>`, `<CORSConfiguration/><CORSConfiguration/>`} {
		if validCORSXML([]byte(body)) {
			t.Error("invalid XML accepted")
		}
	}
}
func TestStorageMetadataNormalizationAndLimit(t *testing.T) {
	good, e := normalizeStorageMetadata(map[string]string{"Purpose": "local acceptance", "owner-id": "fixture"})
	if e != nil || good["purpose"] != "local acceptance" {
		t.Fatal("metadata normalization failed")
	}
	for _, input := range []map[string]string{{"a": strings.Repeat("x", 2048)}, {"bad key": "x"}, {"x": "line\nbreak"}, {"x": "中文"}, {"A": "one", "a": "two"}} {
		if _, e := normalizeStorageMetadata(input); e == nil {
			t.Error("invalid metadata accepted")
		}
	}
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set("X-Amz-Meta-Purpose", "sdk")
	got, e := metadataFromHeaders(req, true)
	if e != nil || got["purpose"] != "sdk" {
		t.Fatal("SDK metadata parsing failed")
	}
}
func TestStorageCORSMetadataRealPostgresEvolutionAndAtomicPolicy(t *testing.T) {
	ctx := context.Background()
	u, e := url.Parse(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if e != nil || u.Path != "/control_ci" {
		t.Fatal("disposable DB required")
	}
	admin, e := pgx.Connect(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	suffix := newID("")
	db := "storage_cors_ci_" + suffix
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(db)); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(ctx, "DROP DATABASE "+pgQuote(db)+" WITH (FORCE)")
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
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('cors-bucket','private'); INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type,metadata) VALUES('cors-bucket','source.txt',$1,repeat('a',64),4,'text/plain','{"purpose":"original"}')`, pgx.QueryExecModeSimpleProtocol, p.ProjectID+"/blob_retained"); e != nil {
		t.Fatal(e)
	}
	req := httptest.NewRequest(http.MethodPut, "/bucket/cors", nil)
	rules := []storageCORSRule{{Origins: []string{"http://127.0.0.1:19001"}, Methods: []string{"PUT"}, Headers: []string{"*"}}}
	if e = storeStorageCORS(req, conn, "cors-bucket", rules, storageCORSETag(nil)); e != nil {
		t.Fatal(e)
	}
	if e = storeStorageCORS(req, conn, "cors-bucket", nil, storageCORSETag(nil)); e == nil {
		t.Fatal("stale policy overwritten")
	}
	actual, e := loadStorageCORS(req, conn, "cors-bucket")
	if e != nil || storageCORSETag(actual) != storageCORSETag(rules) {
		t.Fatal("policy changed after conflict")
	}
	client, err := minio.New("192.0.2.1:30893", &minio.Options{Region: "us-east-2", Creds: credentials.NewStaticV4("bcr_0123456789abcdef01234567", strings.Repeat("s", 64), ""), Transport: s3SDKTransport(func(r *http.Request) (*http.Response, error) {
		var data []byte
		if r.Body != nil {
			data, _ = io.ReadAll(r.Body)
		}
		w := httptest.NewRecorder()
		(&server{}).s3CORSConfiguration(w, r, conn, "cors-bucket", data)
		response := w.Result()
		response.Request = r
		return response, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := client.GetBucketCors(ctx, "cors-bucket")
	if err != nil || policy == nil {
		t.Fatal("actual SDK CORS GET failed")
	}
	if err = client.SetBucketCors(ctx, "cors-bucket", policy); err != nil {
		t.Fatal("actual SDK CORS PUT failed")
	}
	if err = client.SetBucketCors(ctx, "cors-bucket", nil); err != nil {
		t.Fatal("actual SDK CORS DELETE failed")
	}
	if policy, err = client.GetBucketCors(ctx, "cors-bucket"); err != nil || policy != nil {
		t.Fatal("deleted CORS config unexpectedly remained")
	}
	_, e = copyStorageObject(ctx, conn, p.ProjectID, "cors-bucket", "source.txt", "cors-bucket", "copy.txt", strings.Repeat("a", 64), "", "*")
	if e != nil {
		t.Fatal(e)
	}
	var purpose string
	if e = conn.QueryRow(ctx, `SELECT metadata->>'purpose' FROM neon_storage.objects WHERE key='copy.txt'`).Scan(&purpose); e != nil || purpose != "original" {
		t.Fatal("copy discarded metadata")
	}
	tx, _ = conn.Begin(ctx)
	if e = applyObjectStorageSQLTx(ctx, tx, p); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	var count int
	conn.QueryRow(ctx, `SELECT count(*) FROM neon_storage.objects`).Scan(&count)
	if count != 2 {
		t.Fatal("replay changed data")
	}
}
