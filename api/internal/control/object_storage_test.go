package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorageConditionalWriteBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, method, match, none, previous string
		status                              int
	}{
		{"create", "PUT", "", "*", "", 200},
		{"create conflict", "PUT", "", "*", "aaa", 412},
		{"overwrite", "PUT", `"aaa"`, "", "aaa", 200},
		{"stale overwrite", "PUT", `"old"`, "", "aaa", 412},
		{"unguarded overwrite", "PUT", "", "", "aaa", 428},
		{"unguarded delete", "DELETE", "", "*", "aaa", 428},
		{"matched delete", "DELETE", `"aaa"`, "", "aaa", 200},
		{"missing delete", "DELETE", `"aaa"`, "", "", 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/", nil)
			r.Header.Set("If-Match", tc.match)
			r.Header.Set("If-None-Match", tc.none)
			w := httptest.NewRecorder()
			got := storageWriteCondition(w, r, tc.previous)
			if got != (tc.status == 200) || w.Code != tc.status {
				t.Fatalf("unexpected precondition result %v %d", got, w.Code)
			}
		})
	}
}
func TestStorageKeysCannotEscapeDirectory(t *testing.T) {
	for _, key := range []string{"a.txt", "documents/文件.txt", "a..b", "images/a b.png"} {
		if !validStorageKey(key) {
			t.Fatal("valid key rejected", key)
		}
	}
	for _, key := range []string{"", "/abs", "../escape", "dir/../escape", "dir//key", "dir/./key", "line\nkey", strings.Repeat("a", 1025), string([]byte{0xff})} {
		if validStorageKey(key) {
			t.Fatal("unsafe key accepted")
		}
	}
}
func TestStorageDownloadSignatureScopeExpiryAndTampering(t *testing.T) {
	s := &server{idempotencyKey: bytes.Repeat([]byte{42}, 32)}
	v := storageDownload{Branch: "br_one", Bucket: "uploads", Key: "report.txt", Blob: "prj_one/blob_random", Actor: "usr_one", Generation: 3, Expires: time.Now().Add(time.Minute).Unix()}
	token := s.signStorageDownload(v)
	got, ok := s.decodeStorageDownload(token)
	if !ok || got != v {
		t.Fatal("valid capability rejected")
	}
	if _, ok = s.decodeStorageDownload(token + "tampered"); ok {
		t.Fatal("tampered signature accepted")
	}
	other := &server{idempotencyKey: bytes.Repeat([]byte{43}, 32)}
	if _, ok = other.decodeStorageDownload(token); ok {
		t.Fatal("foreign issuer key accepted")
	}
	for _, expiry := range []int64{time.Now().Unix() - 1, time.Now().Add(time.Hour).Unix()} {
		v.Expires = expiry
		if _, ok = s.decodeStorageDownload(s.signStorageDownload(v)); ok {
			t.Fatal("expired/unbounded capability accepted")
		}
	}
}
func TestStorageBlobConfigAndIntegrityBoundary(t *testing.T) {
	content := []byte("immutable object bytes")
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Error("SDK did not sign S3 request")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Length", "22")
		_, _ = w.Write(content)
	}))
	defer endpoint.Close()
	config := storageBlobConfig{endpoint.URL, "neon-product-tests", "us-east-1", "restricted-test-user", "synthetic-test-secret", true}
	p := filepath.Join(t.TempDir(), "config.json")
	write := func() {
		raw, _ := json.Marshal(config)
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	t.Setenv("NEON_OBJECT_STORAGE_CONFIG_FILE", p)
	b, err := loadStorageBlobs()
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt size/hash must fail before any data becomes an HTTP response.
	if _, err = b.read(context.Background(), "prj_test/blob_test", strings.Repeat("0", 64), int64(len(content))); err == nil {
		t.Fatal("corrupt blob accepted")
	}
	config.Bucket = "neon-pageserver"
	write()
	if _, err = loadStorageBlobs(); err == nil {
		t.Fatal("database page bucket accepted")
	}
	config.Bucket = "neon-product-tests"
	config.LabHTTP = false
	write()
	if _, err = loadStorageBlobs(); err == nil {
		t.Fatal("implicit insecure transport accepted")
	}
}
