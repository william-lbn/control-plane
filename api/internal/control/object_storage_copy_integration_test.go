package control

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestStorageCopyUsesRealPostgresConditionsAndSharedBlobReferences(t *testing.T) {
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
	database := "storage_copy_ci_" + suffix
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
	blob := p.ProjectID + "/blob_original"
	digest := strings.Repeat("a", 64)
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('copy-source','private'),('copy-target','private')`); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('copy-source','folder/中文.txt',$1,$2,11,'text/plain')`, blob, digest); e != nil {
		t.Fatal(e)
	}
	result, e := copyStorageObject(ctx, conn, p.ProjectID, "copy-source", "folder/中文.txt", "copy-target", "copied.txt", digest, "", "*")
	if e != nil || result["sha256"] != digest || result["size"] != int64(11) {
		t.Fatal("Real directory copy failed", e)
	}
	var copiedBlob string
	if e = conn.QueryRow(ctx, `SELECT blob_key FROM neon_storage.objects WHERE bucket='copy-target' AND key='copied.txt'`).Scan(&copiedBlob); e != nil || copiedBlob != blob {
		t.Fatal("Copy duplicated or changed immutable content")
	}
	for _, tc := range []struct{ source, match, none, key string }{{"\"" + digest + "\"", "", "*", "copied.txt"}, {"\"stale\"", "", "*", "stale.txt"}, {"\"" + digest + "\"", "\"\"", "", "absent.txt"}} {
		_, e = copyStorageObject(ctx, conn, p.ProjectID, "copy-source", "folder/中文.txt", "copy-target", tc.key, tc.source, tc.match, tc.none)
		var fault *storageMutationFault
		if !errors.As(e, &fault) || fault.Status != 412 {
			t.Fatal("Failed condition mutated copy", e)
		}
	}
	var count int
	if e = conn.QueryRow(ctx, `SELECT count(*) FROM neon_storage.objects`).Scan(&count); e != nil || count != 2 {
		t.Fatal("Failed copy left unexpected catalogue entry")
	}
	if _, e = conn.Exec(ctx, `DELETE FROM neon_storage.objects WHERE bucket='copy-source' AND key='folder/中文.txt'`); e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(ctx, `SELECT blob_key FROM neon_storage.objects WHERE bucket='copy-target' AND key='copied.txt'`).Scan(&copiedBlob); e != nil || copiedBlob != blob {
		t.Fatal("Source deletion erased shared copy reference")
	}
	if _, e = conn.Exec(ctx, `UPDATE neon_storage.objects SET blob_key='another-project/blob_foreign' WHERE bucket='copy-target' AND key='copied.txt'`); e != nil {
		t.Fatal(e)
	}
	_, e = copyStorageObject(ctx, conn, p.ProjectID, "copy-target", "copied.txt", "copy-source", "denied.txt", "", "", "*")
	var fault *storageMutationFault
	if !errors.As(e, &fault) || fault.Code != "ObjectIdentityMismatch" {
		t.Fatal("Foreign blob namespace copied", e)
	}
}

func TestStorageCopySourceParsingRejectsVersionsAndPathConfusion(t *testing.T) {
	b, k, e := parseS3CopySource("/copy-source/folder/%E4%B8%AD%E6%96%87%20file.txt")
	if e != nil || b != "copy-source" || k != "folder/中文 file.txt" {
		t.Fatal("SDK URL-encoded source rejected")
	}
	for _, value := range []string{"//attacker/key", "https://attacker/file", "/copy-source/a?versionId=x", "/copy-source/a#fragment", "/copy-source/%2e%2e/a", "/s3/br_0123456789abcdef/bucket/key", "/copy-source/"} {
		if _, _, e = parseS3CopySource(value); e == nil {
			t.Fatal("Ambiguous or unsupported source accepted", value)
		}
	}
}
