package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func capacityFixture(t *testing.T, values map[string]string) storageCapacityConfig {
	t.Helper()
	value, err := parseStorageCapacity(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	prior := storageCapacitySnapshot.Swap(&value)
	t.Cleanup(func() { storageCapacitySnapshot.Store(prior) })
	return value
}

func TestStorageCapacityFiveGiBConfigurationAndFiniteBudget(t *testing.T) {
	defaults, err := parseStorageCapacity(func(string) string { return "" })
	if err != nil || defaults.ObjectBytes != 64<<20 || defaults.BranchBytes != 256<<20 || defaults.BufferBytes != 256<<20 || defaults.TransferTimeout != 150*time.Second {
		t.Fatal(defaults, err)
	}
	large := map[string]string{"NEON_STORAGE_OBJECT_BYTES": "5368709120", "NEON_STORAGE_BRANCH_BYTES": "21474836480", "NEON_STORAGE_BUFFER_BYTES": "10737418240", "NEON_STORAGE_TRANSFER_SECONDS": "1800"}
	v, err := parseStorageCapacity(func(name string) string { return large[name] })
	if err != nil || v.ObjectBytes != 5<<30 || v.BranchBytes != 20<<30 || v.BufferBytes != 10<<30 || v.TransferTimeout != 30*time.Minute {
		t.Fatal(v, err)
	}
	for name, values := range map[string][]string{"NEON_STORAGE_OBJECT_BYTES": {"5368709121", "0", "-1", "+67108864", " 67108864", "1.5", "18446744073709551615"}, "NEON_STORAGE_BRANCH_BYTES": {"21474836481", "8388608"}, "NEON_STORAGE_BUFFER_BYTES": {"10737418239", "9223372036854775807"}, "NEON_STORAGE_TRANSFER_SECONDS": {"149", "3601", "0"}} {
		for _, bad := range values {
			altered := map[string]string{}
			for k, v := range large {
				altered[k] = v
			}
			altered[name] = bad
			if _, err = parseStorageCapacity(func(name string) string { return altered[name] }); err == nil {
				t.Fatal("invalid capacity accepted", name, bad)
			}
		}
	}
}

func TestStorageCapacityCrossesLegacyBoundaryWithBoundedFile(t *testing.T) {
	t.Setenv("NEON_STORAGE_BUFFER_DIR", t.TempDir())
	capacityFixture(t, map[string]string{"NEON_STORAGE_OBJECT_BYTES": "73400320"})
	source := &storagePatternReader{remaining: 65 << 20}
	p, err := readStoragePayload(context.Background(), source, storageObjectAdmissionLimit())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Size != 65<<20 || source.oversizedRead {
		t.Fatal("configured upload did not cross the old bound with bounded reads")
	}
	reader, err := p.Reader()
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, reader)
	if err != nil || n != 65<<20 {
		t.Fatal("configured payload was truncated", n, err)
	}
	if p, err = readStoragePayload(context.Background(), strings.NewReader("oversize"), 7); p != nil || !errors.Is(err, errStoragePayloadSize) {
		t.Fatal("configured path lost its hard byte limit", err)
	}
}

func TestStorageCapacityFiveGiBMetadataAnd64BitCountersWithoutLargeAllocation(t *testing.T) {
	capacityFixture(t, map[string]string{"NEON_STORAGE_OBJECT_BYTES": "5368709120", "NEON_STORAGE_BRANCH_BYTES": "21474836480", "NEON_STORAGE_BUFFER_BYTES": "10737418240"})
	w := &storagePayloadWriter{ctx: context.Background(), dst: io.Discard, remaining: 4, size: (1 << 32) - 1}
	if n, err := w.Write([]byte("abcd")); err != nil || n != 4 || w.size != (1<<32)+3 || w.remaining != 0 {
		t.Fatal("64-bit transfer counter wrapped", w, err)
	}
	if _, err := w.Write([]byte("e")); !errors.Is(err, errStoragePayloadSize) {
		t.Fatal("counter limit bypassed", err)
	}
	s := &server{idempotencyKey: []byte(strings.Repeat("k", 32))}
	v := storageUpload{Branch: "br_0123456789abcdef", Bucket: "capacity-fixture", Key: "owned/object", Actor: "owned", Generation: 1, Expires: time.Now().Add(time.Minute).Unix(), SHA256: strings.Repeat("a", 64), Size: 5 << 30, ContentType: "application/octet-stream"}
	if parsed, ok := s.decodeStorageUpload(s.signStorageUpload(v)); !ok || parsed.Size != 5<<30 {
		t.Fatal("5 GiB signed metadata truncated")
	}
	v.Size++
	if _, ok := s.decodeStorageUpload(s.signStorageUpload(v)); ok {
		t.Fatal("format ceiling bypassed")
	}
}

type deadlineStorageWriter struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (w *deadlineStorageWriter) SetReadDeadline(value time.Time) error  { w.read = value; return nil }
func (w *deadlineStorageWriter) SetWriteDeadline(value time.Time) error { w.write = value; return nil }
func TestStorageCapacityAdmittedExchangeUsesConfiguredDeadlines(t *testing.T) {
	capacityFixture(t, map[string]string{"NEON_STORAGE_TRANSFER_SECONDS": "1800"})
	w := &deadlineStorageWriter{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	r, done, ok := storageAdmit(w, httptest.NewRequest(http.MethodPut, "/owned", nil))
	if !ok {
		t.Fatal("capacity fixture not admitted")
	}
	defer done()
	deadline, ok := r.Context().Deadline()
	if !ok || deadline.Before(before.Add(1799*time.Second)) || w.read.Before(before.Add(1799*time.Second)) || w.write.Sub(w.read) != 5*time.Second {
		t.Fatal("storage retained ordinary short request deadline")
	}
}

func TestStorageCapacityPostgresFiveGiBBoundsReplayAndSmallPublication(t *testing.T) {
	capacityFixture(t, map[string]string{"NEON_STORAGE_OBJECT_BYTES": "5368709120", "NEON_STORAGE_BRANCH_BYTES": "21474836480", "NEON_STORAGE_BUFFER_BYTES": "10737418240"})
	s, f := checksumBlobFixture(t)
	// These are size-only disposable catalog rows, not a 5 GiB physical transfer.
	if _, err := f.conn.Exec(f.ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('settings-bucket','five-gib',$1,repeat('b',64),$2,'application/octet-stream')`, f.p.ProjectID+"/size-only-fixture", int64(5<<30)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.Exec(f.ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('settings-bucket','too-large',$1,repeat('b',64),$2,'application/octet-stream')`, f.p.ProjectID+"/rejected", int64(5<<30)+1); err == nil {
		t.Fatal("native format ceiling not enforced")
	}
	u, err := s.createMultipart(f.ctx, f.conn, f.p, "owned", "settings-bucket", "capacity-five-gib", multipartCreate{Key: "owned-five-gib", Size: 5 << 30})
	if err != nil || u.ExpectedSize != 5<<30 {
		t.Fatal(u, err)
	}
	if _, err = s.createMultipart(f.ctx, f.conn, f.p, "owned", "settings-bucket", "capacity-overflow", multipartCreate{Key: "too-large", Size: (5 << 30) + 1}); err == nil {
		t.Fatal("multipart metadata ceiling bypassed")
	}
	// Replay must never reinstall the old 64 MiB constraint over new large rows.
	tx, err := f.conn.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err = applyObjectStorageSQLTx(f.ctx, tx, f.p); err != nil {
		t.Fatal("native replay narrowed capacity", err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	var size int64
	if err = f.conn.QueryRow(f.ctx, `SELECT size FROM neon_storage.objects WHERE key='five-gib'`).Scan(&size); err != nil || size != 5<<30 {
		t.Fatal("native replay rewrote representable size", size, err)
	}
	p, _ := smallStoragePayload([]byte("small actual bytes"))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/settings-bucket/small", nil)
	r.Header.Set("If-None-Match", "*")
	if !s.s3WritePayload(w, r, f.conn, f.p, "settings-bucket", "small", p) || w.Code != 200 {
		t.Fatal("large-capacity profile broke small publication", w.Code, w.Body.String())
	}
	if _, err = f.conn.Exec(f.ctx, `UPDATE neon_storage.objects SET size=$1 WHERE key='small'`, int64(5<<30)+1); err == nil {
		t.Fatal("invalid size committed")
	}
}

func TestStorageCapacityDefaultPresignRejectsAboveEffectiveLimitBeforeSQL(t *testing.T) {
	capacityFixture(t, nil)
	input, _ := json.Marshal(map[string]any{"key": "owned/object", "expires_seconds": 60, "sha256": strings.Repeat("a", 64), "size": int64(64<<20) + 1, "content_type": "application/octet-stream"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/owned/presign", strings.NewReader(string(input)))
	r.Header.Set("Content-Type", "application/json")
	(&server{}).storagePresignUpload(w, r)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "invalid_presign") {
		t.Fatal("format ceiling silently raised default admission", w.Code, w.Body.String())
	}
}

func TestStorageCapacityPostgresLoweredWriteBudgetPreservesLargeReferences(t *testing.T) {
	capacityFixture(t, nil)
	f := newSettingsPGFixture(t)
	if _, err := f.conn.Exec(f.ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('settings-bucket','above-soft',$1,repeat('a',64),$2,'application/octet-stream')`, f.p.ProjectID+"/representable-large", int64(65<<20)); err != nil {
		t.Fatal(err)
	}
	_, err := copyStorageObject(f.ctx, f.conn, f.p.ProjectID, "settings-bucket", "above-soft", "settings-bucket", "rejected-copy", "", "", "*")
	var fault *storageMutationFault
	if !errors.As(err, &fault) || fault.Status != 413 {
		t.Fatal("copy bypassed lowered object admission", err)
	}
	var preserved bool
	if err = f.conn.QueryRow(f.ctx, `SELECT (SELECT size=$1 FROM neon_storage.objects WHERE key='above-soft') AND NOT EXISTS(SELECT 1 FROM neon_storage.objects WHERE key='rejected-copy')`, int64(65<<20)).Scan(&preserved); err != nil || !preserved {
		t.Fatal("admission failure rewrote retained reference", err)
	}
}
