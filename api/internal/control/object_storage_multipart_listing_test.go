package control

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestMultipartAdministrationDirectoryGroupingMarkersAndRetiredInitiator(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	u, e := url.Parse(dsn)
	if e != nil || u.Path != "/control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	admin, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	database := "multipart_admin_ci_" + newID("")
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
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('ci-multipart-admin','private'),('ci-other-bucket','private')`); e != nil {
		t.Fatal(e)
	}
	s := &server{}
	const principal = "credential:retired"
	created := []multipartUpload{}
	for i, key := range []string{"p/a/x.bin", "p/a/y.bin", "p/b.bin", "p/same.bin", "p/same.bin", "p/中文/z.bin"} {
		v, e := s.createMultipart(ctx, conn, p, principal, "ci-multipart-admin", "", multipartCreate{Key: key, ContentType: "application/octet-stream", Size: -1})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = conn.Exec(ctx, `UPDATE neon_storage.multipart_uploads SET created_at=$2 WHERE id=$1`, v.ID, time.Date(2026, 10, 16, 0, 0, i, 0, time.UTC)); e != nil {
			t.Fatal(e)
		}
		created = append(created, v)
	}
	// Force random IDs opposite to initiation order. Paging must use retained
	// receipt chronology, including when the previous page's upload is aborted.
	firstID, lastID := "mpu_ffffffffffffffffffffffff", "mpu_000000000000000000000000"
	for _, v := range []struct{ old, next string }{{created[3].ID, firstID}, {created[4].ID, lastID}} {
		if _, e = conn.Exec(ctx, `UPDATE neon_storage.multipart_uploads SET id=$2 WHERE id=$1`, v.old, v.next); e != nil {
			t.Fatal(e)
		}
	}
	created[3].ID = firstID
	created[4].ID = lastID
	foreign, e := s.createMultipart(ctx, conn, p, "credential:foreign", "ci-multipart-admin", "", multipartCreate{Key: "p/foreign.bin", Size: -1})
	if e != nil {
		t.Fatal(e)
	}
	client, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4("bcr_0123456789abcdef01234567", strings.Repeat("s", 64), ""), Region: "us-east-2", Transport: s3SDKTransport(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if !s3QuerySupported(q) || !validMultipartQuery(q) {
			t.Fatal("actual SDK grouped query rejected")
		}
		w := httptest.NewRecorder()
		s.s3ListMultipart(w, r, conn, p, principal, "ci-multipart-admin", q)
		response := w.Result()
		response.Request = r
		return response, nil
	})})
	if e != nil {
		t.Fatal(e)
	}
	core := minio.Core{Client: client}
	marker, idmarker := "", ""
	observed := []string{}
	for page := 0; page < 10; page++ {
		v, e := core.ListMultipartUploads(ctx, "ci-multipart-admin", "p/", marker, idmarker, "/", 1)
		if e != nil {
			t.Fatal("actual SDK delimiter page rejected", e)
		}
		if len(v.Uploads)+len(v.CommonPrefixes) != 1 {
			t.Fatal("grouped page exceeds or loses its one-entry bound", v)
		}
		for _, x := range v.CommonPrefixes {
			observed = append(observed, "group:"+x.Prefix)
		}
		for _, x := range v.Uploads {
			observed = append(observed, x.Key+":"+x.UploadID)
		}
		if !v.IsTruncated {
			break
		}
		if marker == v.NextKeyMarker && idmarker == v.NextUploadIDMarker {
			t.Fatal("cursor failed to advance")
		}
		marker, idmarker = v.NextKeyMarker, v.NextUploadIDMarker
	}
	want := []string{"group:p/a/", "p/b.bin:" + created[2].ID, "p/same.bin:" + firstID, "p/same.bin:" + lastID, "group:p/中文/"}
	if !reflect.DeepEqual(observed, want) {
		t.Fatal("grouping/order/UTF8 pagination differs", observed, want)
	}
	first, e := core.ListMultipartUploads(ctx, "ci-multipart-admin", "p/same.bin", "", "", "", 1)
	if e != nil || len(first.Uploads) != 1 || first.Uploads[0].UploadID != firstID {
		t.Fatal("initiation order lost", e)
	}
	owner, e := abortMultipartAsAdmin(ctx, conn, p, "ci-multipart-admin", firstID)
	if e != nil || owner != principal {
		t.Fatal("admin could not retire original initiator", e)
	}
	next, e := core.ListMultipartUploads(ctx, "ci-multipart-admin", "p/same.bin", first.NextKeyMarker, first.NextUploadIDMarker, "", 1)
	if e != nil || len(next.Uploads) != 1 || next.Uploads[0].UploadID != lastID {
		t.Fatal("completion between pages lost same-key continuation", e)
	}
	query := url.Values{"uploads": {""}, "prefix": {"p/"}, "delimiter": {"/"}, "key-marker": {"p/a/x.bin"}, "encoding-type": {"url"}}
	w := httptest.NewRecorder()
	s.s3ListMultipart(w, httptest.NewRequest("GET", "/?"+query.Encode(), nil), conn, p, principal, "ci-multipart-admin", query)
	var raw struct {
		Prefixes []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
		Uploads []struct {
			Key string `xml:"Key"`
		} `xml:"Upload"`
	}
	if w.Code != 200 || xml.Unmarshal(w.Body.Bytes(), &raw) != nil || len(raw.Prefixes) != 1 || raw.Prefixes[0].Prefix != "p%2F%E4%B8%AD%E6%96%87%2F" {
		t.Fatal("marker inside a group leaked/repeated prior group", w.Code, w.Body.String())
	}
	query.Set("max-uploads", "0")
	w = httptest.NewRecorder()
	s.s3ListMultipart(w, httptest.NewRequest("GET", "/?"+query.Encode(), nil), conn, p, principal, "ci-multipart-admin", query)
	if w.Code != 400 {
		t.Fatal("zero page size accepted")
	}
	clone := p
	clone.BranchID = "br_" + newID("")
	if _, e = abortMultipartAsAdmin(ctx, conn, clone, "ci-multipart-admin", foreign.ID); e == nil {
		t.Fatal("other branch could administer upload")
	}
	if _, e = abortMultipartAsAdmin(ctx, conn, p, "ci-other-bucket", foreign.ID); e == nil {
		t.Fatal("other bucket could administer upload")
	}
	if _, e = conn.Exec(ctx, `UPDATE neon_storage.multipart_uploads SET state='completed' WHERE id=$1`, foreign.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = abortMultipartAsAdmin(ctx, conn, p, "ci-multipart-admin", foreign.ID); e == nil {
		t.Fatal("admin can abort a published upload")
	}
	r := httptest.NewRequest("GET", "/administration?upload_id="+firstID, nil)
	w = httptest.NewRecorder()
	s.storageMultipartAdminList(w, r, conn, p, "ci-multipart-admin")
	var detail struct {
		Items []record `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || len(detail.Items) != 1 || detail.Items[0]["state"] != "aborted" || detail.Items[0]["initiator"] != principal {
		t.Fatal("original terminal state not inspectable")
	}
	// Filling the remaining logical slots then aborting a retired credential's
	// upload must free a slot without erasing physical bytes or object entries.
	for {
		_, e = s.createMultipart(ctx, conn, p, principal, "ci-multipart-admin", "", multipartCreate{Key: "quota/fixture.bin", Size: -1})
		if e != nil {
			break
		}
	}
	if f, ok := e.(*storageMutationFault); !ok || f.Code != "QuotaExceeded" {
		t.Fatal("wrong quota result", e)
	}
	if _, e = abortMultipartAsAdmin(ctx, conn, p, "ci-multipart-admin", lastID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.createMultipart(ctx, conn, p, principal, "ci-multipart-admin", "", multipartCreate{Key: "quota/recovered.bin", Size: -1}); e != nil {
		t.Fatal("admin abort did not free logical quota", e)
	}
}

func TestMultipartAdministrationStrictQueryControls(t *testing.T) {
	for _, q := range []url.Values{{"max-parts": {"1"}}, {"key-marker": {"key"}}, {"upload-id-marker": {"id"}}} {
		if s3QuerySupported(q) {
			t.Fatal("orphan multipart query fell through to ordinary object route")
		}
	}
	if validMultipartQuery(url.Values{"uploadId": {"mpu_0123456789abcdef01234567"}, "encoding-type": {"url"}}) {
		t.Fatal("unsupported ListParts encoding silently accepted")
	}
	for _, v := range []string{string([]byte{0xff}), "\x00", strings.Repeat("x", 1025)} {
		if validMultipartFilter(v) {
			t.Fatal("unsafe XML filter accepted")
		}
	}
	t.Setenv("NEON_S3_PUBLIC_ORIGIN", "http://192.0.2.1:30893")
	location := multipartObjectLocation("br_fixture", "ci-bucket", "folder/中文 ?#.bin")
	u, e := url.Parse(location)
	if e != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/s3/br_fixture/ci-bucket/folder/中文 ?#.bin" {
		t.Fatal("location lost object key identity")
	}
}
