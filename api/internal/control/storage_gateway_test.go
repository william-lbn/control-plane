package control

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type storageShortWriter struct{ header http.Header }

func (w *storageShortWriter) Header() http.Header         { return w.header }
func (w *storageShortWriter) WriteHeader(int)             {}
func (w *storageShortWriter) Write(p []byte) (int, error) { return min(2, len(p)), io.ErrClosedPipe }

func TestStorageUsageCountsAcceptedBytesAndHead(t *testing.T) {
	for _, head := range []bool{false, true} {
		u := &storageUsage{}
		sink := &storageShortWriter{header: http.Header{}}
		w := &storageUsageWriter{ResponseWriter: sink, usage: u, head: head}
		n, err := w.Write([]byte("abcdef"))
		expected := int64(2)
		if head {
			expected = 0
		}
		if n != 2 || !errors.Is(err, io.ErrClosedPipe) || u.egress != expected || !u.failed || u.status != 200 {
			t.Fatalf("short write receipt: %+v", u)
		}
		w.WriteHeader(503)
		if u.status != 200 {
			t.Fatal("duplicate status replaced committed response")
		}
	}
	u := &storageUsage{}
	body := &storageUsageBody{ReadCloser: io.NopCloser(strings.NewReader("123456")), usage: u}
	p := make([]byte, 3)
	if _, err := io.ReadFull(body, p); err != nil {
		t.Fatal(err)
	}
	if u.ingress != 3 {
		t.Fatal("unread body was counted")
	}
}

func TestStorageUsageWrapperIgnoresCallerCountersAndAvoidsDoubleWrapping(t *testing.T) {
	s := &server{}
	var usage *storageUsage
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usage, _ = r.Context().Value(storageUsageKey{}).(*storageUsage)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(206)
		_, _ = w.Write([]byte("abc"))
	})
	handler := s.storageUsageHandler(s.storageUsageHandler(inner))
	r := httptest.NewRequest("PUT", "http://storage.test/s3/br_test/bucket/key", bytes.NewBufferString("actual"))
	r.Header.Set("X-Neon-Usage-Bytes", "900000000")
	r.Header.Set("X-Request-ID", "untrusted")
	r.ContentLength = 999999
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if usage == nil || usage.ingress != 6 || usage.egress != 3 || usage.status != 206 || usage.id != "" || usage.protocol != "s3" || usage.failed {
		t.Fatalf("incorrect observation: %+v", usage)
	}
}

func TestStorageUsageResponseControllerUnwrapAndFlush(t *testing.T) {
	u := &storageUsage{}
	recorder := httptest.NewRecorder()
	writer := &storageUsageWriter{ResponseWriter: recorder, usage: u}
	if writer.Unwrap() != recorder {
		t.Fatal("deadline controller lost underlying writer")
	}
	if err := http.NewResponseController(writer).Flush(); err != nil || !recorder.Flushed || u.status != 200 {
		t.Fatal("stream flush failed", err)
	}
}

func TestStorageGatewayDoesNotExposeConsoleOrHealthOnDataPort(t *testing.T) {
	t.Setenv("NEON_S3_PUBLIC_ORIGIN", "http://storage.test")
	s := &server{}
	handler := s.storageGatewayHandler()
	for _, path := range []string{"/api/v1/projects", "/healthz", "/readyz", "/ai/v1/branch/models", "/functions/test"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://storage.test"+path, nil))
		if w.Code != 404 {
			t.Errorf("%s exposed status %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PUT", "http://storage.test/storage/v1/br_test/uploads?key=a&token=invalid", strings.NewReader("data")))
	if w.Code != 404 && w.Code != 403 {
		t.Fatalf("unsigned native upload admitted: %d", w.Code)
	}
}

func TestStorageGatewayEventsConfiguration(t *testing.T) {
	t.Setenv("NEON_STORAGE_EVENT_MAC_KEY_FILE", "")
	t.Setenv("NEON_STORAGE_EVENT_KEY_VERSION", "")
	platform, err := loadStorageGatewayEvents()
	if err != nil || platform == nil || len(platform.key) != 0 || platform.input.KeyVersion != "" {
		t.Fatal("missing event key must retain fail-closed publisher", err)
	}
	t.Setenv("NEON_STORAGE_EVENT_KEY_VERSION", "v1")
	if _, err = loadStorageGatewayEvents(); err == nil {
		t.Fatal("incomplete event MAC identity accepted")
	}
	t.Setenv("NEON_STORAGE_EVENT_MAC_KEY_FILE", "missing-test-key")
	t.Setenv("NEON_STORAGE_EVENT_KEY_VERSION", "invalid version")
	if _, err = loadStorageGatewayEvents(); err == nil {
		t.Fatal("invalid event identity accepted")
	}
}

func TestStorageUsageTransferReservationBoundsWireAndResponse(t *testing.T) {
	u := &storageUsage{id: newID("sgr_"), ingress: storageTransferReservation() - 2}
	recorder := httptest.NewRecorder()
	writer := &storageUsageWriter{ResponseWriter: recorder, usage: u}
	n, err := writer.Write([]byte("abcd"))
	if n != 2 || !errors.Is(err, io.ErrShortWrite) || !u.failed || u.ingress+u.egress != storageTransferReservation() || recorder.Body.String() != "ab" {
		t.Fatal("response exceeded reserved transfer budget", n, err)
	}
	body := &storageUsageBody{ReadCloser: io.NopCloser(strings.NewReader("x")), usage: u}
	if _, err = body.Read(make([]byte, 1)); !errors.Is(err, errStoragePayloadSize) {
		t.Fatal("wire input exceeded reserved transfer budget", err)
	}
}
