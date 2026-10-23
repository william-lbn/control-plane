package control

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/william-lbn/control-plane/api/internal/s3protocol"
)

func TestStorageSettingsUTCDateNumericOffset(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("local-zero-offset", 0)
	defer func() { time.Local = previous }()
	for _, value := range []string{"2027-01-01T00:00:00Z", "2027-01-01T00:00:00+00:00", "2027-01-01T00:00:00-00:00"} {
		if !storageDateValid(&value) {
			t.Fatal("valid UTC midnight rejected", value)
		}
	}
	for _, value := range []string{"2027-01-01T00:00:00+01:00", "2027-01-01T01:00:00Z", "2027-01-01T00:00:00.001Z", "invalid"} {
		if storageDateValid(&value) {
			t.Fatal("non UTC midnight admitted", value)
		}
	}
}
func TestStorageObjectAttributePaginationAndCorruption(t *testing.T) {
	h := make(http.Header)
	for _, v := range []string{"0", "1001", "-1", " 1", "1,2", ""} {
		h.Set("X-Amz-Max-Parts", v)
		if _, _, e := storageAttributePagination(h); e == nil {
			t.Fatal("invalid page bound admitted", v)
		}
	}
	h.Set("X-Amz-Max-Parts", "1")
	h.Add("X-Amz-Max-Parts", "2")
	if _, _, e := storageAttributePagination(h); e == nil {
		t.Fatal("duplicate bound admitted")
	}
	layout := []storageObjectPart{{2, 3, strings.Repeat("a", 64)}, {7, 2, strings.Repeat("b", 64)}}
	p, e := storagePaginateObjectParts(layout, 5, 1, 2)
	if e != nil || p.Count != 2 || p.Parts[0].Number != 7 || p.Truncated || p.Next != 0 {
		t.Fatal("noncontiguous pagination", p, e)
	}
	for _, bad := range [][]storageObjectPart{{{2, 4, strings.Repeat("a", 64)}}, {{2, 5, "bad"}}, {{2, 3, strings.Repeat("a", 64)}, {2, 2, strings.Repeat("b", 64)}}} {
		if _, e = storagePaginateObjectParts(bad, 5, 1, 0); e == nil {
			t.Fatal("corrupt stored layout admitted")
		}
	}
}
func TestStorageObjectAttributesPostgresActualSDKPublicationAndOverwrite(t *testing.T) {
	f := newSettingsPGFixture(t)
	s := &server{}
	var mu sync.Mutex
	blobs := map[string][]byte{}
	blobServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, e := io.ReadAll(io.LimitReader(r.Body, storageObjectLimit+(1<<20)))
		sig, se := s3protocol.Parse(r, "us-east-2", time.Now())
		data, de := sig.VerifyAndDecode(r, wire, "synthetic-secret-at-least-32-characters", "us-east-2", storageObjectLimit)
		if e != nil || se != nil || de != nil {
			w.WriteHeader(403)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PUT":
			blobs[r.URL.Path] = data
			w.Header().Set("ETag", fmt.Sprintf("\"%x\"", md5.Sum(data)))
		case "GET":
			v, ok := blobs[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(v)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("ETag", fmt.Sprintf("\"%x\"", md5.Sum(v)))
			_, _ = w.Write(v)
		default:
			w.WriteHeader(405)
		}
	}))
	defer blobServer.Close()
	blobClient, e := minio.New(strings.TrimPrefix(blobServer.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("bcr_0123456789abcdef01234567", "synthetic-secret-at-least-32-characters", ""), Region: "us-east-2", Secure: false, Transport: blobServer.Client().Transport})
	if e != nil {
		t.Fatal(e)
	}
	s.storage = &storageBlobs{client: blobClient, bucket: "neon-product-ci"}
	key := "attributes/multipart.bin"
	first := bytes.Repeat([]byte("p"), multipartMinimumPart)
	last := []byte("tail")
	all := append(append([]byte{}, first...), last...)
	digest := fmt.Sprintf("%x", sha256.Sum256(all))
	u, e := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "attributes-owned-upload", multipartCreate{Key: key, ContentType: "application/octet-stream", SHA256: digest, Size: int64(len(all))})
	if e != nil {
		t.Fatal(e)
	}
	a, e := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", key, u.ID, 2, first, "", nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", key, u.ID, 4, []byte("not-selected"), "", nil); e != nil {
		t.Fatal(e)
	}
	b, e := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", key, u.ID, 7, last, "", nil)
	if e != nil {
		t.Fatal(e)
	}
	selection := []multipartSelection{{2, a.ETag}, {7, b.ETag}}
	if _, e = s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", key, u.ID, selection, "", "*"); e != nil {
		t.Fatal(e)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig, se := s3protocol.Parse(r, "us-east-2", time.Now())
		if se != nil || sig.Verify(r, nil, "synthetic-secret-at-least-32-characters", "us-east-2") != nil {
			w.WriteHeader(403)
			return
		}
		s.s3ObjectAttributes(w, r, f.conn, f.p, "settings-bucket", strings.TrimPrefix(r.URL.Path, "/settings-bucket/"))
	}))
	defer api.Close()
	client, e := minio.New(strings.TrimPrefix(api.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("bcr_0123456789abcdef01234567", "synthetic-secret-at-least-32-characters", ""), Region: "us-east-2", Secure: false, Transport: api.Client().Transport})
	if e != nil {
		t.Fatal(e)
	}
	page, e := client.GetObjectAttributes(f.ctx, "settings-bucket", key, minio.ObjectAttributesOptions{MaxParts: 1, VersionID: "null"})
	if e != nil || page.ETag != digest || page.LastModified.IsZero() || page.VersionID != "null" || page.ObjectSize != len(all) || page.ObjectParts.PartsCount != 2 || len(page.ObjectParts.Parts) != 1 || page.ObjectParts.Parts[0].PartNumber != 2 || !page.ObjectParts.IsTruncated || page.ObjectParts.NextPartNumberMarker != 2 {
		t.Fatalf("first typed SDK page: %+v %v", page, e)
	}
	page, e = client.GetObjectAttributes(f.ctx, "settings-bucket", key, minio.ObjectAttributesOptions{MaxParts: 1, PartNumberMarker: 2})
	if e != nil || len(page.ObjectParts.Parts) != 1 || page.ObjectParts.Parts[0].PartNumber != 7 || page.ObjectParts.Parts[0].Size != len(last) || page.ObjectParts.IsTruncated {
		t.Fatalf("last typed SDK page: %+v %v", page, e)
	}
	page, e = client.GetObjectAttributes(f.ctx, "settings-bucket", key, minio.ObjectAttributesOptions{MaxParts: 1, PartNumberMarker: 7})
	if e != nil || len(page.ObjectParts.Parts) != 0 || page.ObjectParts.PartsCount != 2 || page.ObjectParts.IsTruncated {
		t.Fatal("terminal page", e)
	}
	if _, e = copyStorageObject(f.ctx, f.conn, f.p.ProjectID, "settings-bucket", key, "settings-bucket", "copied.bin", "\""+digest+"\"", "", "*"); e != nil {
		t.Fatal(e)
	}
	page, e = client.GetObjectAttributes(f.ctx, "settings-bucket", "copied.bin", minio.ObjectAttributesOptions{})
	if e != nil || page.ObjectParts.PartsCount != 0 {
		t.Fatal("single COPY retained source layout", e)
	}
	r := httptest.NewRequest("PUT", "http://s3.test/settings-bucket/"+key, nil)
	w := httptest.NewRecorder()
	if !s.s3WriteObject(w, r, f.conn, f.p, "settings-bucket", key, []byte("new-generation")) || w.Code != 200 {
		t.Fatal("overwrite", w.Code, w.Body.String())
	}
	// The original completion receipt must not republish its old layout or bytes.
	if _, e = s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", key, u.ID, selection, "", "*"); e != nil {
		t.Fatal(e)
	}
	page, e = client.GetObjectAttributes(f.ctx, "settings-bucket", key, minio.ObjectAttributesOptions{})
	if e != nil || page.ObjectParts.PartsCount != 0 || page.ObjectSize != len("new-generation") {
		t.Fatal("completion replay restored old layout", e)
	}
}
