package control

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestStorageLargeSchemaUpgradePreservesExistingObjectsAndPartBounds(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	suffix := newID("")[:16]
	name := "storage_large_ci_" + suffix
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(name)); err != nil {
		t.Fatal(err)
	}
	target := cfg.Copy()
	target.Database = name
	conn, err := pgx.ConnectConfig(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		conn.Close(context.Background())
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, e := admin.Exec(cleanup, "DROP DATABASE "+pgQuote(name)+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	p := objectStoragePayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ObjectStorageSpec{Database: name}}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := conn.Exec(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	// Construct the actual old format, rather than starting with the new schema.
	exec(`CREATE SCHEMA neon_storage;`+objectStorageSchema, pgx.QueryExecModeSimpleProtocol)
	exec("COMMENT ON SCHEMA neon_storage IS " + pgLiteral("neon-control:object-storage:"+p.ProjectID))
	exec(`ALTER TABLE neon_storage.objects ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}';ALTER TABLE neon_storage.buckets ADD COLUMN cors jsonb NOT NULL DEFAULT '[]';`+objectMultipartSchema, pgx.QueryExecModeSimpleProtocol)
	exec(`INSERT INTO neon_storage.installations(branch_id,generation) VALUES($1,1)`, p.BranchID)
	exec(`INSERT INTO neon_storage.buckets(name,access) VALUES('retained-bucket','private')`)
	exec(`INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type,metadata) VALUES('retained-bucket','retained','original/immutable-blob',$1,7,'text/plain','{"retain":"original"}')`, strings.Repeat("a", 64))
	exec(`INSERT INTO neon_storage.multipart_uploads(id,branch_id,principal,bucket,key,content_type,expected_size) VALUES('original-upload',$1,'owner','retained-bucket','pending','text/plain',1024)`, p.BranchID)
	snapshot := func() []byte {
		t.Helper()
		var value []byte
		if e := conn.QueryRow(ctx, `SELECT jsonb_build_object('objects',(SELECT jsonb_agg(to_jsonb(o)-'tags'-'response_headers'-'part_layout'-'checksums'-'checksum_policy') FROM neon_storage.objects o WHERE key='retained'),'uploads',(SELECT jsonb_agg(to_jsonb(u)-'tags'-'response_headers'-'checksum_algorithm'-'checksum_type'-'result_checksum_policy') FROM neon_storage.multipart_uploads u WHERE id='original-upload'))`).Scan(&value); e != nil {
			t.Fatal(e)
		}
		return value
	}
	original := snapshot()
	insertLarge := func(key string, size int64) error {
		_, e := conn.Exec(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('retained-bucket',$1,'owned-disposable-fixture',$2,$3,'application/octet-stream')`, key, strings.Repeat("b", 64), size)
		return e
	}
	if e := insertLarge("before-upgrade", 9<<20); e == nil {
		t.Fatal("fixture did not retain the old 8 MiB constraint")
	}
	upgrade := func() {
		t.Helper()
		tx, e := conn.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = applyObjectStorageSQLTx(ctx, tx, p); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	upgrade()
	var defaults bool
	if e := conn.QueryRow(ctx, `SELECT (SELECT bool_and(tags='[]'::jsonb AND response_headers='{}'::jsonb AND part_layout='[]'::jsonb AND checksums='{}'::jsonb AND checksum_policy='{}'::jsonb) FROM neon_storage.objects WHERE key='retained') AND (SELECT bool_and(tags='[]'::jsonb AND response_headers='{}'::jsonb AND checksum_algorithm='' AND checksum_type='' AND result_checksum_policy='{}'::jsonb) FROM neon_storage.multipart_uploads WHERE id='original-upload')`).Scan(&defaults); e != nil || !defaults {
		t.Fatal("additive settings changed the original default behavior", e)
	}
	if !bytes.Equal(original, snapshot()) {
		t.Fatal("schema upgrade rewrote retained objects or unfinished uploads")
	}
	if e := insertLarge("large", 17<<20); e != nil {
		t.Fatal("large object remained blocked by old constraint", e)
	}
	if e := insertLarge("too-large", storageObjectLimit+1); e == nil {
		t.Fatal("object capacity constraint missing")
	}
	if _, e := conn.Exec(ctx, `INSERT INTO neon_storage.multipart_parts(upload_id,part_number,blob_key,sha256,etag,size) VALUES('original-upload',1,'owned-part',$1,$2,$3)`, strings.Repeat("c", 64), strings.Repeat("d", 32), storagePartLimit+1); e == nil {
		t.Fatal("large object upgrade removed the bounded part constraint")
	}
	var format int
	if e := conn.QueryRow(ctx, `SELECT format_version FROM neon_storage.installations WHERE branch_id=$1`, p.BranchID).Scan(&format); e != nil || format != 7 {
		t.Fatal("native format marker missing", e)
	}
	upgrade()
	if !bytes.Equal(original, snapshot()) {
		t.Fatal("schema replay rewrote original manifest or upload receipt")
	}
}
