package control

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/william-lbn/control-plane/api/internal/s3protocol"
)

func TestStorageFullChecksumKnownVectorsAndStrictDeclaration(t *testing.T) {
	p, err := smallStoragePayload([]byte("123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if p.checksums["X-Amz-Checksum-Crc64nvme"] != base64.StdEncoding.EncodeToString([]byte{0xae, 0x8b, 0x14, 0x86, 0x0a, 0x79, 0x98, 0x88}) {
		t.Fatal("CRC64/NVME vector differs")
	}
	for algorithm, name := range map[string]string{"SHA256": "X-Amz-Checksum-Sha256", "SHA1": "X-Amz-Checksum-Sha1", "CRC32": "X-Amz-Checksum-Crc32", "CRC32C": "X-Amz-Checksum-Crc32c", "CRC64NVME": "X-Amz-Checksum-Crc64nvme"} {
		h := http.Header{}
		h.Set("X-Amz-Sdk-Checksum-Algorithm", algorithm)
		h.Set(name, p.checksums[name])
		if err = validateStoragePayloadChecksums(h, p); err != nil {
			t.Fatal(algorithm, err)
		}
		for _, change := range []func(http.Header){func(h http.Header) { h.Set(name, "not-a-digest") }, func(h http.Header) { h.Del(name) }, func(h http.Header) { h.Add(name, p.checksums[name]) }, func(h http.Header) { h.Add("X-Amz-Sdk-Checksum-Algorithm", algorithm) }, func(h http.Header) {
			h.Add("X-Amz-Checksum-Type", "FULL_OBJECT")
			h.Add("X-Amz-Checksum-Type", "FULL_OBJECT")
		}} {
			bad := h.Clone()
			change(bad)
			if validateStoragePayloadChecksums(bad, p) == nil {
				t.Fatal("invalid checksum declaration accepted", algorithm)
			}
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/bucket/key", nil)
	r.Header.Set("X-Amz-Checksum-Crc64nvme", p.checksums["X-Amz-Checksum-Crc64nvme"])
	writeStoragePayloadChecksums(w, r, p)
	if w.Header().Get("X-Amz-Checksum-Crc64nvme") != p.checksums["X-Amz-Checksum-Crc64nvme"] {
		t.Fatal("upload receipt lost requested checksum")
	}
}

func TestStorageStoredChecksumLegacyFallbackCorruptionAndNoAliasing(t *testing.T) {
	p, _ := smallStoragePayload([]byte("preserved bytes"))
	stored := p.storedChecksums()
	if len(stored) != 5 {
		t.Fatal("complete checksum set missing")
	}
	out, err := storageStoredChecksums(p.SHA256, stored)
	if err != nil || !reflect.DeepEqual(out, stored) {
		t.Fatal(out, err)
	}
	legacy, err := storageStoredChecksums(p.SHA256, nil)
	if err != nil || len(legacy) != 1 || legacy["X-Amz-Checksum-Sha256"] == "" {
		t.Fatal("old object checksum fabricated", err)
	}
	attr, err := storageAttributeChecksums(p.SHA256, stored)
	if err != nil || attr.CRC64NVME != stored["X-Amz-Checksum-Crc64nvme"] || attr.Type != "FULL_OBJECT" {
		t.Fatal(attr, err)
	}
	for name, value := range map[string]string{"X-Amz-Checksum-Sha256": strings.Repeat("A", 44), "X-Amz-Checksum-Crc64nvme": "AAAAAA==", "X-Amz-Checksum-Unknown": "AAAAAA==", "x-amz-checksum-crc32": "AAAAAA=="} {
		bad := map[string]string{name: value}
		if _, err = storageStoredChecksums(p.SHA256, bad); err == nil {
			t.Fatal("corrupt stored checksum accepted", name)
		}
	}
	stored["X-Amz-Checksum-Sha256"] = "changed"
	if p.checksums["X-Amz-Checksum-Sha256"] == "changed" {
		t.Fatal("persistence aliases payload checksum map")
	}
}

// Actual pinned MinIO client and SigV4 wire against a disposable blob fixture.
// This never reaches the lab's product bucket or any prior object bytes.
func checksumBlobFixture(t *testing.T) (*server, *settingsPGFixture) {
	t.Helper()
	f := newSettingsPGFixture(t)
	var mu sync.Mutex
	blobs := map[string][]byte{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, e := io.ReadAll(io.LimitReader(r.Body, storageObjectLimit+(1<<20)))
		sig, se := s3protocol.Parse(r, "us-east-2", time.Now())
		if e != nil || se != nil {
			w.WriteHeader(403)
			return
		}
		data, de := sig.VerifyAndDecode(r, wire, "synthetic-secret-at-least-32-characters", "us-east-2", storageObjectLimit)
		if de != nil {
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
	t.Cleanup(upstream.Close)
	client, e := minio.New(strings.TrimPrefix(upstream.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("bcr_0123456789abcdef01234567", "synthetic-secret-at-least-32-characters", ""), Region: "us-east-2", Secure: false, Transport: upstream.Client().Transport})
	if e != nil {
		t.Fatal(e)
	}
	return &server{storage: &storageBlobs{client: client, bucket: "neon-product-ci"}}, f
}

func TestStorageFullChecksumPostgresPublicationReadsCopyAndOverwrite(t *testing.T) {
	s, f := checksumBlobFixture(t)
	payload, _ := smallStoragePayload([]byte("123456789"))
	expected := payload.storedChecksums()
	put := func(key string, p *storagePayload, none bool) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/settings-bucket/"+key, nil)
		r.Header.Set("Content-Type", "text/plain")
		if none {
			r.Header.Set("If-None-Match", "*")
		}
		if !s.s3WritePayload(w, r, f.conn, f.p, "settings-bucket", key, p) || w.Code != 200 {
			t.Fatal("publication", w.Code, w.Body.String())
		}
	}
	put("checksum-original", payload, true)
	var stored map[string]string
	if e := f.conn.QueryRow(f.ctx, `SELECT checksums FROM neon_storage.objects WHERE key='checksum-original'`).Scan(&stored); e != nil || !reflect.DeepEqual(stored, expected) {
		t.Fatal(stored, e)
	}
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/settings-bucket/checksum-original", nil)
		r.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
		s.serveS3Object(w, r, f.conn, f.p, "settings-bucket", "checksum-original")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		for name, value := range expected {
			if w.Header().Get(name) != value {
				t.Fatal("read lost checksum", method, name)
			}
		}
		if method == "GET" && !bytes.Equal(w.Body.Bytes(), []byte("123456789")) {
			t.Fatal("changed bytes")
		}
		if method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("HEAD returned bytes")
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/settings-bucket/checksum-original", nil)
	r.Header.Set("Range", "bytes=1-3")
	r.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
	s.serveS3Object(w, r, f.conn, f.p, "settings-bucket", "checksum-original")
	if w.Code != 206 || w.Body.String() != "234" || w.Header().Get("X-Amz-Checksum-Crc64nvme") != "" {
		t.Fatal("range checksum scope", w.Code)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/settings-bucket/checksum-original?attributes", nil)
	r.Header.Set("X-Amz-Object-Attributes", "Checksum,ObjectSize")
	s.s3ObjectAttributes(w, r, f.conn, f.p, "settings-bucket", "checksum-original")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<ChecksumCRC64NVME>"+expected["X-Amz-Checksum-Crc64nvme"]+"</ChecksumCRC64NVME>") {
		t.Fatal("attributes lost checksums", w.Code, w.Body.String())
	}
	if _, e := copyStorageObject(f.ctx, f.conn, f.p.ProjectID, "settings-bucket", "checksum-original", "settings-bucket", "checksum-copy", "\""+payload.SHA256+"\"", "", "*"); e != nil {
		t.Fatal(e)
	}
	if e := f.conn.QueryRow(f.ctx, `SELECT checksums FROM neon_storage.objects WHERE key='checksum-copy'`).Scan(&stored); e != nil || !reflect.DeepEqual(stored, expected) {
		t.Fatal("copy lost checksums", stored, e)
	}
	replacement, _ := smallStoragePayload([]byte("replacement"))
	put("checksum-copy", replacement, false)
	if e := f.conn.QueryRow(f.ctx, `SELECT checksums FROM neon_storage.objects WHERE key='checksum-copy'`).Scan(&stored); e != nil || !reflect.DeepEqual(stored, replacement.storedChecksums()) {
		t.Fatal("overwrite retained old checksum", e)
	}
	// A corrupt declaration must fail closed even when the SHA-256 bytes match.
	bad := payload.storedChecksums()
	bad["X-Amz-Checksum-Crc64nvme"] = "AAAAAAAAAAA="
	if _, e := f.conn.Exec(f.ctx, `UPDATE neon_storage.objects SET checksums=$1 WHERE key='checksum-original'`, bad); e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	s.serveS3Object(w, httptest.NewRequest("GET", "/settings-bucket/checksum-original", nil), f.conn, f.p, "settings-bucket", "checksum-original")
	if w.Code != 503 || strings.Contains(w.Body.String(), "123456789") {
		t.Fatal("corrupt checksum read published bytes", w.Code)
	}
}

func TestStorageFullChecksumPostgresMultipartReplayAndAdditiveSchema(t *testing.T) {
	s, f := checksumBlobFixture(t)
	u, e := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "checksum-owned-upload", multipartCreate{Key: "checksum-multipart", Size: -1})
	if e != nil {
		t.Fatal(e)
	}
	data := []byte("small multipart bytes")
	part, e := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, 1, data, "", nil)
	if e != nil {
		t.Fatal(e)
	}
	selection := []multipartSelection{{Number: 1, ETag: part.ETag}}
	first, e := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, selection, "", "*")
	if e != nil {
		t.Fatal(e)
	}
	snapshot := func() string {
		t.Helper()
		var out string
		if e := f.conn.QueryRow(f.ctx, `SELECT to_jsonb(o)::text FROM neon_storage.objects o WHERE key=$1`, u.Key).Scan(&out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	original := snapshot()
	p, _ := smallStoragePayload(data)
	var row map[string]json.RawMessage
	if e = json.Unmarshal([]byte(original), &row); e != nil {
		t.Fatal(e)
	}
	var stored map[string]string
	if e = json.Unmarshal(row["checksums"], &stored); e != nil || !reflect.DeepEqual(stored, p.storedChecksums()) {
		t.Fatal("multipart checksum not atomic", e)
	}
	again, e := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, selection, "", "*")
	if e != nil || again.SHA256 != first.SHA256 || snapshot() != original {
		t.Fatal("receipt replay rewrote checksum generation", e)
	}
	if _, e = f.conn.Exec(f.ctx, objectChecksumsSchema); e != nil || snapshot() != original {
		t.Fatal("schema replay changed checksum publication", e)
	}
	var legacy bool
	if e = f.conn.QueryRow(f.ctx, `SELECT checksums='{}'::jsonb FROM neon_storage.objects WHERE key='source.txt'`).Scan(&legacy); e != nil || !legacy {
		t.Fatal("legacy checksums invented", e)
	}
}
