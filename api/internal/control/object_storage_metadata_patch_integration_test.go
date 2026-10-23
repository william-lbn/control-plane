package control

import (
	"context"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestStorageMetadataEditRealPostgresCASAndS3Replacement(t *testing.T) {
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

	expected := storageMetadataState(p.BranchID, "cors-bucket", "source.txt", p.ProjectID+"/blob_retained", strings.Repeat("a", 64), "text/plain", map[string]string{"purpose": "original"})
	item, e := updateStorageMetadata(ctx, conn, p, "cors-bucket", "source.txt", expected, "", map[string]string{"purpose": "edited"})
	if e != nil {
		t.Fatal(e)
	}
	if stringVal(item["sha256"]) != strings.Repeat("a", 64) || item["metadata_etag"] == expected {
		t.Fatal("metadata edit changed bytes or did not change metadata ETag")
	}
	if _, e = updateStorageMetadata(ctx, conn, p, "cors-bucket", "source.txt", expected, "", map[string]string{"purpose": "stale"}); e == nil {
		t.Fatal("stale metadata overwritten")
	}
	if _, e = updateStorageMetadata(ctx, conn, p, "cors-bucket", "source.txt", storageMetadataState("br_other", "cors-bucket", "source.txt", p.ProjectID+"/blob_retained", strings.Repeat("a", 64), "text/plain", map[string]string{"purpose": "edited"}), "", map[string]string{}); e == nil {
		t.Fatal("foreign branch metadata token allowed")
	}
	_, e = copyStorageObject(ctx, conn, p.ProjectID, "cors-bucket", "source.txt", "cors-bucket", "source.txt", strings.Repeat("a", 64), "", "", storageCopyMetadata{Metadata: map[string]string{"purpose": "sdk-replaced"}, ContentType: "application/json"})
	if e != nil {
		t.Fatal(e)
	}
	var blob, purpose, contentType string
	conn.QueryRow(ctx, `SELECT blob_key,metadata->>'purpose',content_type FROM neon_storage.objects WHERE key='source.txt'`).Scan(&blob, &purpose, &contentType)
	if blob != p.ProjectID+"/blob_retained" || purpose != "sdk-replaced" || contentType != "application/json" {
		t.Fatal("replacement broke immutable content or metadata")
	}
	if _, e = copyStorageObject(ctx, conn, p.ProjectID, "cors-bucket", "source.txt", "cors-bucket", "source.txt", strings.Repeat("b", 64), "", "", storageCopyMetadata{Metadata: map[string]string{}, ContentType: "text/plain"}); e == nil {
		t.Fatal("stale source replaced metadata")
	}
}
