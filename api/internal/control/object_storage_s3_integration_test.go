package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type s3SDKTransport func(*http.Request) (*http.Response, error)

func (f s3SDKTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestS3SDKListUsesActualPostgreSQLDirectoryAndPagination(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	u, e := url.Parse(dsn)
	if e != nil || u.Path != "/control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	suffix := newID("")
	database := "s3_list_ci_" + suffix
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(database)); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if _, e = admin.Exec(ctx, "DROP DATABASE "+pgQuote(database)+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	u.Path = "/" + database
	conn, e := pgx.Connect(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	p := objectStoragePayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ObjectStorageSpec{Database: database}}
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
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('ci-s3-bucket','private'); INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('ci-s3-bucket','folder/a file.bin','retained-a',repeat('a',64),4,'text/plain'),('ci-s3-bucket','folder/中文 file.bin','retained-b',repeat('b',64),4,'text/plain')`, pgx.QueryExecModeSimpleProtocol); e != nil {
		t.Fatal(e)
	}
	s := &server{}
	calls := 0
	client, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4("bcr_0123456789abcdef01234567", "synthetic-key-at-least-32-characters-long", ""), Region: "us-east-2", Transport: s3SDKTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if !s3QuerySupported(q) || q.Get("fetch-owner") != "true" {
			t.Fatal("Actual SDK query unsupported")
		}
		w := httptest.NewRecorder()
		s.s3ListObjects(w, r, conn, p, "ci-s3-bucket", q)
		response := w.Result()
		response.Request = r
		return response, nil
	})})
	if e != nil {
		t.Fatal(e)
	}
	var keys []string
	for item := range client.ListObjects(ctx, "ci-s3-bucket", minio.ListObjectsOptions{Recursive: true, Prefix: "folder/", MaxKeys: 1}) {
		if item.Err != nil {
			t.Fatal("Actual SDK list rejected")
		}
		if item.Owner.ID != p.BranchID {
			t.Fatal("Owner is not the authorized branch identity")
		}
		keys = append(keys, item.Key)
	}
	if calls != 2 || len(keys) != 2 || keys[0] != "folder/a file.bin" || keys[1] != "folder/中文 file.bin" {
		t.Fatal("SDK UTF-8/pagination differs from actual directory")
	}
	for _, query := range []url.Values{{"uploads": {""}}, {"metadata": {"true"}}} {
		if s3QuerySupported(query) {
			t.Fatal("Unimplemented subresource accepted")
		}
	}
}
