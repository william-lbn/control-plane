package control

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/william-lbn/control-plane/api/internal/s3protocol"
)

func TestMultipartParserRejectsAmbiguousOrUnsupportedInput(t *testing.T) {
	valid := `<CompleteMultipartUpload xmlns="` + s3XMLNamespace + `"><Part><PartNumber>1</PartNumber><ETag>"d41d8cd98f00b204e9800998ecf8427e"</ETag></Part></CompleteMultipartUpload>`
	parts, e := parseMultipartCompletion([]byte(valid))
	if e != nil || len(parts) != 1 {
		t.Fatal("standard XML rejected")
	}
	for _, v := range []string{valid + valid, strings.Replace(valid, "<ETag>", "<ChecksumCRC32>x</ChecksumCRC32><ETag>", 1), strings.Replace(valid, "</PartNumber>", "</PartNumber><PartNumber>2</PartNumber>", 1), strings.Replace(valid, s3XMLNamespace, "urn:foreign", 1), `<!DOCTYPE x>` + valid, strings.Replace(valid, "</ETag>", "<foo/></ETag>", 1)} {
		if _, e := parseMultipartCompletion([]byte(v)); e == nil {
			t.Fatal("ambiguous XML accepted")
		}
	}
	for _, v := range []url.Values{{"uploads": {""}, "uploadId": {"id"}}, {"uploadId": {"id"}, "partNumber": {"1", "2"}}, {"uploads": {""}, "delete": {""}}, {"uploadId": {""}}} {
		if validMultipartQuery(v) {
			t.Fatal("ambiguous multipart query accepted")
		}
	}
	if _, _, e := multipartCopyRange("bytes=1-2", 3); e != nil {
		t.Fatal(e)
	}
	for _, v := range []string{"bytes=-1-2", "bytes=2-1", "bytes=0-3", "bytes=0-1,2-2", "bytes=0-"} {
		if _, _, e := multipartCopyRange(v, 3); e == nil {
			t.Fatal("invalid range accepted")
		}
	}
	if _, e := multipartManifestHash([]multipartSelection{{2, strings.Repeat("a", 32)}, {1, strings.Repeat("b", 32)}}, "", ""); e == nil {
		t.Fatal("unordered manifest accepted")
	}
}

func TestMultipartPostgreSQLAtomicPublicationIsolationReplayAndAbort(t *testing.T) {
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
	database := "multipart_ci_" + newID("")
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(database)); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if _, e := admin.Exec(ctx, "DROP DATABASE "+pgQuote(database)+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	u.Path = "/" + database
	conn, e := pgx.Connect(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	p := objectStoragePayload{ProjectID: "prj_" + newID(""), BranchID: "br_" + newID(""), Generation: 1, Spec: ObjectStorageSpec{Database: database}}
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
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('ci-multipart','private')`); e != nil {
		t.Fatal(e)
	}
	// An HTTP S3 blob fixture tests actual minio-go transfer and immutable bytes;
	// the separate live probe exercises real credentials/SigV4/Kubernetes storage.
	var mu sync.Mutex
	blobs := map[string][]byte{}
	blobServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, err := io.ReadAll(io.LimitReader(r.Body, storageObjectLimit+(1<<20)))
		sig, err2 := s3protocol.Parse(r, "us-east-2", time.Now())
		data, err3 := sig.VerifyAndDecode(r, wire, "synthetic-secret-at-least-32-characters", "us-east-2", storageObjectLimit)
		if err != nil || err2 != nil || err3 != nil {
			w.WriteHeader(403)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PUT":
			blobs[r.URL.Path] = data
			w.Header().Set("ETag", fmt.Sprintf("\"%x\"", md5.Sum(data)))
			w.WriteHeader(200)
		case "GET":
			v, ok := blobs[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(v)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("ETag", fmt.Sprintf("\"%x\"", md5.Sum(v)))
			_, _ = w.Write(v)
		default:
			w.WriteHeader(405)
		}
	}))
	defer blobServer.Close()
	client, e := minio.New(strings.TrimPrefix(blobServer.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("bcr_0123456789abcdef01234567", "synthetic-secret-at-least-32-characters", ""), Region: "us-east-2", Secure: false, Transport: blobServer.Client().Transport})
	if e != nil {
		t.Fatal(e)
	}
	s := &server{storage: &storageBlobs{client: client, bucket: "neon-product-ci"}}
	assertCode := func(e error, code string) {
		t.Helper()
		v, ok := e.(*storageMutationFault)
		if !ok || v.Code != code {
			t.Fatalf("want %s got %v", code, e)
		}
	}
	content := append(bytes.Repeat([]byte("a"), multipartMinimumPart), []byte("retained-tail")...)
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	input := multipartCreate{Key: "folder/中文.bin", ContentType: "application/octet-stream", SHA256: digest, Size: int64(len(content)), Metadata: map[string]string{"purpose": "multipart-ci"}}
	created, e := s.createMultipart(ctx, conn, p, "user:one", "ci-multipart", "create-attempt-one", input)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := s.createMultipart(ctx, conn, p, "user:one", "ci-multipart", "create-attempt-one", input)
	if e != nil || replay.ID != created.ID {
		t.Fatal("durable creation replay differs", e)
	}
	changed := input
	changed.Key = "different"
	_, e = s.createMultipart(ctx, conn, p, "user:one", "ci-multipart", "create-attempt-one", changed)
	assertCode(e, "IdempotencyConflict")
	_, e = s.putMultipartPart(ctx, conn, p, "user:foreign", "ci-multipart", input.Key, created.ID, 1, []byte("foreign"), "", nil)
	assertCode(e, "NoSuchUpload")
	clone := p
	clone.BranchID = "br_" + newID("")
	_, e = s.putMultipartPart(ctx, conn, clone, "user:one", "ci-multipart", input.Key, created.ID, 1, []byte("clone"), "", nil)
	assertCode(e, "NoSuchUpload")
	_, e = s.putMultipartPart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, 1, []byte("bad"), "invalid-MD5", nil)
	assertCode(e, "BadDigest")
	first, e := s.putMultipartPart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, 1, []byte("replace-me"), "", nil)
	if e != nil {
		t.Fatal(e)
	}
	first, e = s.putMultipartPart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, 1, content[:multipartMinimumPart], "", nil)
	if e != nil {
		t.Fatal(e)
	}
	last, e := s.putMultipartPart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, 9, content[multipartMinimumPart:], "", nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.storage.read(ctx, first.blob, first.SHA256, first.Size); e != nil {
		obj, err := client.GetObject(ctx, "neon-product-ci", first.blob, minio.GetObjectOptions{})
		if err != nil {
			t.Fatal("synthetic blob fixture GET", err)
		}
		data, err := io.ReadAll(obj)
		_ = obj.Close()
		t.Fatalf("synthetic blob fixture read: %v bytes=%d expected=%d digest=%x expected_digest=%s", err, len(data), first.Size, sha256.Sum256(data), first.SHA256)
	}
	var count int
	if e = conn.QueryRow(ctx, `SELECT count(*) FROM neon_storage.objects WHERE key=$1`, input.Key).Scan(&count); e != nil || count != 0 {
		t.Fatal("partial upload visible")
	}
	manifest := []multipartSelection{{1, first.ETag}, {9, last.ETag}}
	_, e = s.completeMultipart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, []multipartSelection{{1, strings.Repeat("0", 32)}}, "", "*")
	assertCode(e, "InvalidPart")
	// Force receipt update to fail. Object publication must roll back with it.
	if _, e = conn.Exec(ctx, `ALTER TABLE neon_storage.multipart_uploads ADD CONSTRAINT ci_no_completion CHECK (state<>'completed')`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.completeMultipart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, manifest, "", "*"); e == nil {
		t.Fatal("constraint failure ignored")
	}
	if e = conn.QueryRow(ctx, `SELECT count(*) FROM neon_storage.objects WHERE key=$1`, input.Key).Scan(&count); e != nil || count != 0 {
		t.Fatal("publication escaped failed receipt")
	}
	if _, e = conn.Exec(ctx, `ALTER TABLE neon_storage.multipart_uploads DROP CONSTRAINT ci_no_completion`); e != nil {
		t.Fatal(e)
	}
	completed, e := s.completeMultipart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, manifest, "", "*")
	if e != nil || completed.SHA256 != digest {
		t.Fatal("complete failed", e)
	}
	var blob string
	var metadata map[string]string
	if e = conn.QueryRow(ctx, `SELECT blob_key,metadata FROM neon_storage.objects WHERE key=$1`, input.Key).Scan(&blob, &metadata); e != nil || metadata["purpose"] != "multipart-ci" {
		t.Fatal("metadata lost", e)
	}
	stored, e := s.storage.read(ctx, blob, digest, int64(len(content)))
	if e != nil || !bytes.Equal(stored, content) {
		t.Fatal("assembled bytes differ", e)
	}
	// A later write must survive completion replay, even though the original
	// If-None-Match predicate would no longer be true against today's object.
	if _, e = conn.Exec(ctx, `UPDATE neon_storage.objects SET sha256=repeat('f',64) WHERE key=$1`, input.Key); e != nil {
		t.Fatal(e)
	}
	completed, e = s.completeMultipart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, manifest, "", "*")
	if e != nil || completed.SHA256 != digest {
		t.Fatal("lost response not replayable", e)
	}
	var current string
	if e = conn.QueryRow(ctx, `SELECT sha256 FROM neon_storage.objects WHERE key=$1`, input.Key).Scan(&current); e != nil || current != strings.Repeat("f", 64) {
		t.Fatal("replay republished stale object")
	}
	_, e = s.completeMultipart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID, []multipartSelection{{1, first.ETag}}, "", "*")
	assertCode(e, "CompletionConflict")
	assertCode(abortMultipart(ctx, conn, p, "user:one", "ci-multipart", input.Key, created.ID), "NoSuchUpload")
	// Restore only this disposable fixture for range-copy checks.
	if _, e = conn.Exec(ctx, `UPDATE neon_storage.objects SET sha256=$2 WHERE key=$1`, input.Key, digest); e != nil {
		t.Fatal(e)
	}
	copyInput := multipartCreate{Key: "range-copy.bin", ContentType: "application/octet-stream", Size: -1}
	copyUpload, e := s.createMultipart(ctx, conn, p, "credential:one", "ci-multipart", "", copyInput)
	if e != nil {
		t.Fatal(e)
	}
	h := http.Header{"X-Amz-Copy-Source": {url.PathEscape("/ci-multipart/" + input.Key)}, "X-Amz-Copy-Source-Range": {"bytes=2-12"}, "X-Amz-Copy-Source-If-Match": {"\"" + digest + "\""}}
	// Copy source escaping mirrors actual SDK slash-preserving URI encoding.
	h.Set("X-Amz-Copy-Source", "/ci-multipart/"+strings.ReplaceAll(url.PathEscape(input.Key), "%2F", "/"))
	part, e := s.putMultipartPart(ctx, conn, p, "credential:one", "ci-multipart", copyInput.Key, copyUpload.ID, 2, nil, "", h)
	if e != nil || part.Size != 11 {
		t.Fatal("range copy failed", e)
	}
	copyResult, e := s.completeMultipart(ctx, conn, p, "credential:one", "ci-multipart", copyInput.Key, copyUpload.ID, []multipartSelection{{2, part.ETag}}, "", "")
	if e != nil || copyResult.Size != 11 {
		t.Fatal("copied part publication failed", e)
	}
	abortInput := multipartCreate{Key: "abort.bin", ContentType: "application/octet-stream", Size: -1}
	aborted, e := s.createMultipart(ctx, conn, p, "user:one", "ci-multipart", "abort-one", abortInput)
	if e != nil {
		t.Fatal(e)
	}
	if e = abortMultipart(ctx, conn, p, "user:one", "ci-multipart", abortInput.Key, aborted.ID); e != nil {
		t.Fatal(e)
	}
	if e = abortMultipart(ctx, conn, p, "user:one", "ci-multipart", abortInput.Key, aborted.ID); e != nil {
		t.Fatal("abort replay failed", e)
	}
	_, e = s.putMultipartPart(ctx, conn, p, "user:one", "ci-multipart", abortInput.Key, aborted.ID, 1, []byte("late"), "", nil)
	assertCode(e, "NoSuchUpload")
	// SQL failure and logical abort intentionally retain immutable blob evidence.
	mu.Lock()
	retained := len(blobs)
	mu.Unlock()
	if retained < 5 {
		t.Fatal("immutable failure evidence unexpectedly erased")
	}
}
