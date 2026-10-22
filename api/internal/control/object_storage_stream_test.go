package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type storagePatternReader struct {
	remaining     int64
	oversizedRead bool
}

func (r *storagePatternReader) Read(data []byte) (int, error) {
	if len(data) > 64<<10 {
		r.oversizedRead = true
		return 0, errors.New("producer asked for a full-object allocation")
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int64(len(data))
	if n > r.remaining {
		n = r.remaining
	}
	for i := int64(0); i < n; i++ {
		data[i] = 'x'
	}
	r.remaining -= n
	return int(n), nil
}

func TestStoragePayloadLargerThanLegacyLimitStreamsAndRewinds(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("NEON_STORAGE_BUFFER_DIR", directory)
	const size = 17 << 20
	source := &storagePatternReader{remaining: size}
	p, err := readStoragePayload(context.Background(), source, size)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Size != size || source.oversizedRead {
		t.Fatal("large payload did not use bounded reads")
	}
	info, err := p.file.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private upload bytes are not private", err)
	}
	expected := sha256.New()
	io.Copy(expected, &storagePatternReader{remaining: size})
	if p.SHA256 != hex.EncodeToString(expected.Sum(nil)) {
		t.Fatal("incremental digest differs from uploaded bytes")
	}
	for i := 0; i < 2; i++ {
		reader, e := p.Reader()
		if e != nil {
			t.Fatal(e)
		}
		actual := sha256.New()
		n, e := io.Copy(actual, reader)
		if e != nil || n != size || hex.EncodeToString(actual.Sum(nil)) != p.SHA256 {
			t.Fatal("verified payload replay differs", e)
		}
	}
	name := p.file.Name()
	p.Close()
	p.Close()
	if _, err = os.Stat(name); !os.IsNotExist(err) {
		t.Fatal("private payload survived closure")
	}
}

func TestStoragePayloadRejectsOversizeCancellationAndFailedProducerWithoutLeaks(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("NEON_STORAGE_BUFFER_DIR", directory)
	if p, err := readStoragePayload(context.Background(), strings.NewReader("one byte too much"), 4); p != nil || !errors.Is(err, errStoragePayloadSize) {
		t.Fatal("oversize did not discard private bytes", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p, err := buildStoragePayload(ctx, 20, func(dst io.Writer) error {
		if _, e := dst.Write([]byte("private")); e != nil {
			return e
		}
		cancel()
		_, e := dst.Write([]byte("extra"))
		return e
	})
	if p != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled upload returned publishable bytes", err)
	}
	failed := errors.New("failed original signature")
	p, err = buildStoragePayload(context.Background(), 20, func(dst io.Writer) error { dst.Write([]byte("unverified")); return failed })
	if p != nil || !errors.Is(err, failed) {
		t.Fatal("unverified bytes survived authentication failure", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 || len(storagePayloadSlots) != 0 {
		t.Fatal("failed payload retained private bytes or capacity", err)
	}
}

func TestStoragePayloadCapacityIsRecoveredAfterOriginalRequestCloses(t *testing.T) {
	t.Setenv("NEON_STORAGE_BUFFER_DIR", t.TempDir())
	first, err := readStoragePayload(context.Background(), strings.NewReader("a"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := readStoragePayload(context.Background(), strings.NewReader("b"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if p, e := readStoragePayload(context.Background(), strings.NewReader("c"), 1); p != nil || !errors.Is(e, errStoragePayloadCapacity) {
		t.Fatal("private buffer quota bypassed", e)
	}
	first.Close()
	third, err := readStoragePayload(context.Background(), strings.NewReader("c"), 1)
	if err != nil {
		t.Fatal("closed request did not release buffer capacity", err)
	}
	third.Close()
}

func TestStoragePayloadRestartRemovesOnlyOwnedUnfinishedBuffers(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("NEON_STORAGE_BUFFER_DIR", directory)
	owned, err := os.CreateTemp(directory, ".neon-storage-payload-")
	if err != nil {
		t.Fatal(err)
	}
	owned.WriteString("unfinished private bytes")
	owned.Close()
	keep := filepath.Join(directory, "retained.txt")
	if err = os.WriteFile(keep, []byte("retained bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, ".neon-storage-payload-123")
	if err = os.Symlink(keep, link); err != nil {
		t.Fatal(err)
	}
	if err = prepareStoragePayloadDirectory(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(owned.Name()); !os.IsNotExist(err) {
		t.Fatal("unfinished buffer retained")
	}
	if raw, e := os.ReadFile(keep); e != nil || string(raw) != "retained bytes" {
		t.Fatal("unrelated bytes changed", e)
	}
	if _, err = os.Lstat(link); err != nil {
		t.Fatal("cleanup followed or removed an unrelated link")
	}
}

func TestStoragePayloadChecksumAlgorithmsAndControlRequestSeparation(t *testing.T) {
	p, err := smallStoragePayload([]byte("123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if p.checksums["X-Amz-Checksum-Crc32c"] != base64.StdEncoding.EncodeToString([]byte{0xe3, 0x06, 0x92, 0x83}) || p.checksums["X-Amz-Checksum-Crc32"] != base64.StdEncoding.EncodeToString([]byte{0xcb, 0xf4, 0x39, 0x26}) {
		t.Fatal("CRC32 and CRC32C were interchanged")
	}
	headers := http.Header{}
	headers.Set("X-Amz-Sdk-Checksum-Algorithm", "CRC32C")
	headers.Set("X-Amz-Checksum-Crc32c", p.checksums["X-Amz-Checksum-Crc32c"])
	if err = validateStoragePayloadChecksums(headers, p); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(http.Header){
		func(h http.Header) { h.Set("X-Amz-Checksum-Crc32c", "AAAAAA==") },
		func(h http.Header) { h.Del("X-Amz-Checksum-Crc32c") },
		func(h http.Header) {
			h["Content-Md5"] = []string{p.checksums["Content-MD5"], p.checksums["Content-MD5"]}
		},
		func(h http.Header) { h.Set("X-Amz-Checksum-Crc64nvme", "unsupported") },
		func(h http.Header) { h.Set("X-Amz-Checksum-Type", "COMPOSITE") },
	} {
		bad := headers.Clone()
		change(bad)
		if validateStoragePayloadChecksums(bad, p) == nil {
			t.Fatal("invalid or unsupported checksum accepted")
		}
	}
	for _, tc := range []struct {
		method, path, copy string
		stream             bool
	}{
		{"PUT", "/s3/br_0123456789abcdef/bucket/key", "", true}, {"PUT", "/bucket/key", "", true}, {"PUT", "/bucket", "", false}, {"GET", "/bucket/key", "", false},
		{"PUT", "/bucket/key?uploadId=owned&partNumber=1", "", false}, {"PUT", "/bucket/key?uploads", "", false}, {"PUT", "/bucket/key", "/source/key", false},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.copy != "" {
			r.Header.Set("X-Amz-Copy-Source", tc.copy)
		}
		if s3StreamingObjectPUT(r) != tc.stream {
			t.Fatal("control or COPY body treated as object bytes", tc.path)
		}
	}
}
