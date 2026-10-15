package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestStorageCopySourceConditionsAtomicPostgres(t *testing.T) {
	ctx := context.Background()
	u, err := url.Parse(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil || u.Path != "/control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	admin, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	database := "copy_conditions_ci_" + newID("")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(database)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgQuote(database)+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u.Path = "/" + database
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	project := "prj_" + newID("")
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyObjectStorageSQLTx(ctx, tx, objectStoragePayload{ProjectID: project, BranchID: "br_" + newID(""), Generation: 1, Spec: ObjectStorageSpec{Database: database}}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	modified := time.Date(2026, 10, 15, 10, 30, 0, 800000000, time.UTC)
	past, same, future := modified.Add(-time.Hour), modified.Truncate(time.Second), modified.Add(time.Hour)
	if _, err = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('copy-conditions','private')`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type,updated_at) VALUES('copy-conditions','source',$1,$2,5,'text/plain',$3)`, project+"/original", digest, modified); err != nil {
		t.Fatal(err)
	}
	for n, tc := range []struct {
		name       string
		conditions storageCopyConditions
		allow      bool
	}{
		{"modified", storageCopyConditions{ModifiedSince: &past}, true},
		{"not_modified", storageCopyConditions{ModifiedSince: &future}, false},
		{"modified_same_second", storageCopyConditions{ModifiedSince: &same}, false},
		{"unmodified", storageCopyConditions{UnmodifiedSince: &future}, true},
		{"changed_after", storageCopyConditions{UnmodifiedSince: &past}, false},
		{"unmodified_same_second", storageCopyConditions{UnmodifiedSince: &same}, true},
		{"none_match_same", storageCopyConditions{NoneMatch: digest}, false},
		{"none_match_different", storageCopyConditions{NoneMatch: strings.Repeat("b", 64)}, true},
		{"none_match_wildcard", storageCopyConditions{NoneMatch: "*"}, false},
		{"match_dominates_unmodified", storageCopyConditions{Match: digest, UnmodifiedSince: &past}, true},
		{"none_match_dominates_modified_false", storageCopyConditions{NoneMatch: strings.Repeat("b", 64), ModifiedSince: &future}, true},
		{"none_match_dominates_modified_true", storageCopyConditions{NoneMatch: digest, ModifiedSince: &past}, false},
		{"match_does_not_ignore_other_etag", storageCopyConditions{Match: digest, NoneMatch: digest}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("target-%d", n)
			// A failed predicate must preserve both an absent destination and an
			// existing destination, including metadata and its original blob.
			if !tc.allow {
				if _, err = conn.Exec(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('copy-conditions',$1,$2,$3,7,'text/plain')`, key, project+"/kept", strings.Repeat("c", 64)); err != nil {
					t.Fatal(err)
				}
			}
			_, err := copyStorageObjectWithConditions(ctx, conn, project, "copy-conditions", "source", "copy-conditions", key, tc.conditions, "", "")
			var fault *storageMutationFault
			if tc.allow && err != nil {
				t.Fatal(err)
			}
			if !tc.allow && (!errors.As(err, &fault) || fault.Status != 412) {
				t.Fatal("Expected 412", err)
			}
			var blob string
			if err = conn.QueryRow(ctx, `SELECT blob_key FROM neon_storage.objects WHERE bucket='copy-conditions' AND key=$1`, key).Scan(&blob); err != nil {
				t.Fatal(err)
			}
			want := project + "/original"
			if !tc.allow {
				want = project + "/kept"
			}
			if blob != want {
				t.Fatal("Unexpected copy mutation")
			}
			if !tc.allow {
				_, err = copyStorageObjectWithConditions(ctx, conn, project, "copy-conditions", "source", "copy-conditions", key+"-absent", tc.conditions, "", "")
				if !errors.As(err, &fault) || fault.Status != 412 {
					t.Fatal(err)
				}
				var count int
				if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_storage.objects WHERE key=$1`, key+"-absent").Scan(&count); err != nil || count != 0 {
					t.Fatal("Rejected copy published destination")
				}
			}
		})
	}
}

func TestStorageCopySourceConditionHeaderValidation(t *testing.T) {
	valid := http.Header{}
	valid.Set("X-Amz-Copy-Source-If-Match", `"`+strings.Repeat("a", 64)+`"`)
	valid.Set("X-Amz-Copy-Source-If-Unmodified-Since", "Thu, 15 Oct 2026 10:30:00 GMT")
	if c, err := parseStorageCopyConditions(valid); err != nil || c.Match != strings.Repeat("a", 64) || c.UnmodifiedSince == nil {
		t.Fatal("Valid SDK conditions rejected", err)
	}
	for _, tc := range []struct {
		name, value string
		duplicate   bool
	}{
		{"X-Amz-Copy-Source-If-Match", `W/"` + strings.Repeat("a", 64) + `"`, false},
		{"X-Amz-Copy-Source-If-None-Match", "", false},
		{"X-Amz-Copy-Source-If-None-Match", "*", true},
		{"X-Amz-Copy-Source-If-Modified-Since", "not-a-date", false},
		{"X-Amz-Copy-Source-If-Unmodified-Since", "Thu, 15 Oct 2026 10:30:00 GMT", true},
	} {
		h := http.Header{}
		h.Add(tc.name, tc.value)
		if tc.duplicate {
			h.Add(tc.name, tc.value)
		}
		if _, err := parseStorageCopyConditions(h); err == nil {
			t.Fatal("Malformed/ambiguous condition accepted", tc.name)
		}
	}
}
