package control

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/minio/crc64nvme"
)

// Buffers contain uncommitted private bytes, never durable object references.
// One admitted payload owns one slot until its file is closed and removed.
var storagePayloadSlots = make(chan struct{}, 2)
var errStoragePayloadSize = errors.New("object payload exceeds its bounded size")
var errStoragePayloadCapacity = errors.New("object payload buffers exhausted")

// Called once before this API process serves requests. The chart gives each
// Pod its own emptyDir; a container restart can leave its unfinished buffers.
func prepareStoragePayloadDirectory() error {
	directory := os.Getenv("NEON_STORAGE_BUFFER_DIR")
	if directory == "" {
		return nil
	}
	info, err := os.Lstat(directory)
	if err != nil || !filepath.IsAbs(directory) || !info.IsDir() {
		return errors.New("private object buffer directory unavailable")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return errors.New("private object buffer directory unavailable")
	}
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), ".neon-storage-payload-")
		if !ok || len(suffix) == 0 || len(suffix) > 20 || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return errors.New("private object buffer inspection failed")
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Mode().Perm() != 0600 {
			return errors.New("unexpected private object buffer permissions")
		}
		if os.Remove(filepath.Join(directory, entry.Name())) != nil {
			return errors.New("unfinished private object buffer cleanup failed")
		}
	}
	return nil
}

type storagePayload struct {
	file      *os.File
	memory    io.ReadSeeker
	Size      int64
	SHA256    string
	checksums map[string]string
	close     sync.Once
}

func (p *storagePayload) Close() {
	p.close.Do(func() {
		if p.file != nil {
			p.file.Close()
			os.Remove(p.file.Name())
			<-storagePayloadSlots
		}
	})
}
func (p *storagePayload) Reader() (io.ReadSeeker, error) {
	if p.file == nil {
		if p.memory == nil {
			return nil, errors.New("object payload unavailable")
		}
		_, err := p.memory.Seek(0, io.SeekStart)
		return p.memory, err
	}
	if _, err := p.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return p.file, nil
}

func smallStoragePayload(data []byte) (*storagePayload, error) {
	if len(data) > storagePartLimit {
		return nil, errStoragePayloadSize
	}
	p := &storagePayload{memory: bytes.NewReader(data), Size: int64(len(data)), checksums: map[string]string{}}
	for name, h := range storagePayloadHashes() {
		h.Write(data)
		p.checksums[name] = base64.StdEncoding.EncodeToString(h.Sum(nil))
		if name == "X-Amz-Checksum-Sha256" {
			p.SHA256 = hex.EncodeToString(h.Sum(nil))
		}
	}
	return p, nil
}

func storagePayloadHashes() map[string]hash.Hash {
	return map[string]hash.Hash{"Content-MD5": md5.New(), "X-Amz-Checksum-Crc64nvme": crc64nvme.New(), "X-Amz-Checksum-Sha256": sha256.New(), "X-Amz-Checksum-Sha1": sha1.New(), "X-Amz-Checksum-Crc32": crc32.NewIEEE(), "X-Amz-Checksum-Crc32c": crc32.New(crc32.MakeTable(crc32.Castagnoli))}
}

type storagePayloadWriter struct {
	ctx       context.Context
	dst       io.Writer
	remaining int64
	size      int64
}

func (w *storagePayloadWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > w.remaining {
		return 0, errStoragePayloadSize
	}
	n, err := w.dst.Write(data)
	w.remaining -= int64(n)
	w.size += int64(n)
	return n, err
}

// The producer must finish authentication/decoding before this returns. Its
// writes are never published directly; every failure removes the private file.
func buildStoragePayload(ctx context.Context, limit int64, produce func(io.Writer) error) (*storagePayload, error) {
	if limit < 0 || limit > storageObjectLimit {
		return nil, errStoragePayloadSize
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case storagePayloadSlots <- struct{}{}:
	default:
		return nil, errStoragePayloadCapacity
	}
	directory := os.Getenv("NEON_STORAGE_BUFFER_DIR")
	if directory == "" {
		directory = os.TempDir()
	}
	file, err := os.CreateTemp(directory, ".neon-storage-payload-")
	if err != nil {
		<-storagePayloadSlots
		return nil, errors.New("private object buffer unavailable")
	}
	p := &storagePayload{file: file, checksums: map[string]string{}}
	success := false
	defer func() {
		if !success {
			p.Close()
		}
	}()
	// These are S3 byte-integrity checksums, not password or authentication hashes.
	hashes := storagePayloadHashes()
	writers := []io.Writer{file}
	for _, h := range hashes {
		writers = append(writers, h)
	}
	sink := &storagePayloadWriter{ctx: ctx, dst: io.MultiWriter(writers...), remaining: limit}
	if err = produce(sink); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	p.Size = sink.size
	p.SHA256 = hex.EncodeToString(hashes["X-Amz-Checksum-Sha256"].Sum(nil))
	for name, h := range hashes {
		p.checksums[name] = base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	if _, err = p.Reader(); err != nil {
		return nil, err
	}
	success = true
	return p, nil
}

func (s *server) storagePayloadFailure(w http.ResponseWriter, r *http.Request, err error, s3 bool) {
	status, code := 503, "storage_unavailable"
	var fault *storageMutationFault
	switch {
	case errors.Is(err, errStoragePayloadSize):
		status, code = 413, "object_too_large"
	case errors.Is(err, errStoragePayloadCapacity):
		status, code = 429, "storage_capacity_exhausted"
	case errors.As(err, &fault):
		status, code = fault.Status, fault.Code
	}
	if s3 {
		if code == "object_too_large" {
			code = "EntityTooLarge"
		}
		if code == "storage_capacity_exhausted" {
			status, code = 503, "SlowDown"
		}
		if code == "storage_unavailable" {
			code = "ServiceUnavailable"
		}
		s3Error(w, r, status, code)
		return
	}
	fail(w, r, status, code, "Object payload unavailable, too large, or its checksum does not match")
}

func readStoragePayload(ctx context.Context, source io.Reader, limit int64) (*storagePayload, error) {
	return buildStoragePayload(ctx, limit, func(dst io.Writer) error {
		_, err := io.CopyBuffer(dst, io.LimitReader(source, limit+1), make([]byte, 64<<10))
		return err
	})
}

func validateStoragePayloadChecksums(headers http.Header, p *storagePayload) error {
	for _, name := range []string{"X-Amz-Checksum-Type", "X-Amz-Sdk-Checksum-Algorithm"} {
		if len(headers.Values(name)) > 1 {
			return multipartFault(400, "BadDigest")
		}
	}
	for name, expected := range p.checksums {
		values := headers.Values(name)
		if len(values) > 1 || len(values) == 1 && values[0] != expected {
			return multipartFault(400, "BadDigest")
		}
	}
	for name := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if strings.HasPrefix(strings.ToLower(name), "x-amz-checksum-") && canonical != "X-Amz-Checksum-Type" && canonical != "X-Amz-Checksum-Mode" {
			if _, ok := p.checksums[canonical]; !ok {
				return multipartFault(501, "NotImplemented")
			}
		}
	}
	if value := headers.Get("X-Amz-Checksum-Type"); value != "" && value != "FULL_OBJECT" {
		return multipartFault(501, "NotImplemented")
	}
	if algorithm := headers.Get("X-Amz-Sdk-Checksum-Algorithm"); algorithm != "" {
		name := map[string]string{"SHA256": "X-Amz-Checksum-Sha256", "SHA1": "X-Amz-Checksum-Sha1", "CRC32": "X-Amz-Checksum-Crc32", "CRC32C": "X-Amz-Checksum-Crc32c", "CRC64NVME": "X-Amz-Checksum-Crc64nvme"}[algorithm]
		if name == "" {
			return multipartFault(501, "NotImplemented")
		}
		if headers.Get(name) == "" {
			return multipartFault(400, "BadDigest")
		}
	}
	return nil
}

func writeStoragePayloadChecksums(w http.ResponseWriter, r *http.Request, p *storagePayload) {
	w.Header().Set("X-Amz-Checksum-Sha256", p.checksums["X-Amz-Checksum-Sha256"])
	w.Header().Set("X-Amz-Checksum-Type", "FULL_OBJECT")
	for name, value := range p.checksums {
		if name != "Content-MD5" && r.Header.Get(name) != "" {
			w.Header().Set(name, value)
		}
	}
}

func s3StreamingObjectPUT(r *http.Request) bool {
	if r.Method != http.MethodPut || r.Header.Get("X-Amz-Copy-Source") != "" {
		return false
	}
	query := r.URL.Query()
	if resource, valid := s3SettingResource(query); !valid || resource != "" {
		return false
	}
	for _, name := range []string{"uploads", "uploadId", "partNumber", "cors", "location", "delete"} {
		if query.Has(name) {
			return false
		}
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if strings.HasPrefix(path, "s3/") {
		_, tail, ok := strings.Cut(strings.TrimPrefix(path, "s3/"), "/")
		if !ok {
			return false
		}
		path = tail
	}
	_, key, ok := strings.Cut(path, "/")
	return ok && key != ""
}
