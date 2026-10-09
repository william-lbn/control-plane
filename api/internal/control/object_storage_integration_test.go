package control

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestStorageRealBranchDirectoryReplayCloneAndOwnership(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	suffix := strings.TrimPrefix(newID(""), "")
	parent := "storage_ci_" + suffix
	child := parent + "_child"
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(parent)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, db := range []string{child, parent} {
			if _, err = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgQuote(db)+" WITH (FORCE)"); err != nil {
				t.Error(err)
			}
		}
	}()
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	target.Path = "/" + parent
	conn, err := pgx.Connect(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	p := objectStoragePayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ObjectStorageSpec{Database: parent}}
	apply := func(conn *pgx.Conn, p objectStoragePayload, success bool) {
		t.Helper()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		err = applyObjectStorageSQLTx(ctx, tx, p)
		if success {
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("unsafe install accepted")
		}
	}
	apply(conn, p, true)
	if _, err = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('uploads','private'); INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('uploads','report.txt','shared-immutable-blob',repeat('a',64),4,'text/plain')`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	apply(conn, p, true)
	p.Generation = 2
	apply(conn, p, true)
	var count int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_storage.objects`).Scan(&count); err != nil || count != 1 {
		t.Fatal("retry erased object manifest", err)
	}
	var public bool
	if err = conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace n,aclexplode(n.nspacl) a WHERE n.nspname='neon_storage' AND a.grantee=0 AND a.privilege_type IN ('USAGE','CREATE'))`).Scan(&public); err != nil || public {
		t.Fatal("public schema access exposed", err)
	}
	p.Generation = 1
	apply(conn, p, false)
	p.Generation = 2
	_ = conn.Close(ctx)
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(child)+" TEMPLATE "+pgQuote(parent)); err != nil {
		t.Fatal(err)
	}
	target.Path = "/" + child
	clone, err := pgx.Connect(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close(ctx)
	p.BranchID = "br_child_" + suffix
	p.Generation = 1
	apply(clone, p, true)
	if _, err = clone.Exec(ctx, `DELETE FROM neon_storage.objects WHERE key='report.txt'`); err != nil {
		t.Fatal(err)
	}
	target.Path = "/" + parent
	original, err := pgx.Connect(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close(ctx)
	if err = original.QueryRow(ctx, `SELECT count(*) FROM neon_storage.objects`).Scan(&count); err != nil || count != 1 {
		t.Fatal("child deletion changed parent", err)
	}
	p.ProjectID = "foreign-project"
	apply(clone, p, false)
}
