package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/signer"
)

// The fixture is generated incrementally; no whole large object is buffered.
type largePattern struct{ offset, end int64 }

func (r *largePattern) Read(p []byte) (int, error) {
	if r.offset >= r.end {
		return 0, io.EOF
	}
	if int64(len(p)) > r.end-r.offset {
		p = p[:r.end-r.offset]
	}
	for i := range p {
		p[i] = byte((r.offset + int64(i)) % 251)
	}
	r.offset += int64(len(p))
	return len(p), nil
}

type largeCheckpoint struct {
	Bucket   string               `json:"bucket"`
	Branch   string               `json:"branch_id"`
	Stage    string               `json:"stage"`
	UploadID string               `json:"upload_id,omitempty"`
	Parts    []minio.CompletePart `json:"parts,omitempty"`
}

func probeLargeObjects(ctx context.Context, client *minio.Client, c config, transport *onceTransport, stage func(string), check func(string)) error {
	if !strings.HasPrefix(c.Output, "/run/e2e-private/") || !strings.HasPrefix(c.Branch, "br_") {
		return errors.New("protected large object checkpoint required")
	}
	const size int64 = 17<<20 + 19
	const source = "large/sdk-single.bin"
	const target = "large/sdk-multipart.bin"
	state := largeCheckpoint{Bucket: c.Bucket, Branch: c.Branch}
	if raw, err := os.ReadFile(c.Output); err == nil {
		if json.Unmarshal(raw, &state) != nil || state.Bucket != c.Bucket || state.Branch != c.Branch {
			return errors.New("original large object checkpoint scope differs")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	previousStage := state.Stage
	save := func(value string) error {
		stage(value)
		state.Stage = value
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return os.WriteFile(c.Output, raw, 0600)
	}
	h := sha256.New()
	if _, err := io.CopyBuffer(h, &largePattern{end: size}, make([]byte, 64<<10)); err != nil {
		return err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	checksum := base64.StdEncoding.EncodeToString(h.Sum(nil))
	verify := func(key string) error {
		info, err := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{Checksum: true})
		if err != nil {
			return err
		}
		if info.Size != size || info.ETag != digest || info.ChecksumSHA256 != checksum {
			return errors.New("large object HEAD size, SHA256 or checksum differs")
		}
		object, err := client.GetObject(ctx, c.Bucket, key, minio.GetObjectOptions{Checksum: true})
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.CopyBuffer(h, io.LimitReader(object, size+1), make([]byte, 64<<10))
		closeErr := object.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if n != size || hex.EncodeToString(h.Sum(nil)) != digest {
			return errors.New("large object download bytes differ")
		}
		return nil
	}
	stage("inspect_original_large_single_target")
	if _, err := client.StatObject(ctx, c.Bucket, source, minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code == "NoSuchKey" {
		if err = save("before_single_put"); err != nil {
			return err
		}
		info, err := client.PutObject(ctx, c.Bucket, source, &largePattern{end: size}, size, minio.PutObjectOptions{DisableMultipart: true, ContentType: "application/octet-stream", UserMetadata: map[string]string{"purpose": "bounded-large-stream"}})
		if err != nil {
			return err
		}
		if info.Size != size {
			return errors.New("large upload acknowledgement size differs")
		}
	} else if err != nil {
		return err
	}
	if err := verify(source); err != nil {
		return err
	}
	check("actual_sdk_17MiB_single_put_full_get_head_sha256_checksum")
	if err := save("single_verified"); err != nil {
		return err
	}
	// Explicit HTTP checks preserve status and Content-Range evidence, using the
	// official SDK signer and the same no-retry transport as the SDK operations.
	for _, tc := range []struct {
		name, rangeValue, ifRange string
		status                    int
		start, length             int64
	}{
		{"cross_part_boundary", "bytes=6291441-6291475", "", 206, 6291441, 35},
		{"suffix", "bytes=-97", "", 206, size - 97, 97},
		{"out_of_range", fmt.Sprintf("bytes=%d-", size), "", 416, 0, 0},
		{"if_range_mismatch", "bytes=1-4", "\"stale\"", 200, 0, size},
	} {
		stage("large_range_" + tc.name)
		r, err := http.NewRequestWithContext(ctx, "GET", c.Endpoint+"/"+c.Bucket+"/"+source, nil)
		if err != nil {
			return err
		}
		r.Header.Set("Range", tc.rangeValue)
		if tc.ifRange != "" {
			r.Header.Set("If-Range", tc.ifRange)
		}
		r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		r = signer.SignV4(*r, c.Access, c.Secret, "", c.Region)
		response, err := (&http.Client{Transport: transport}).Do(r)
		if err != nil {
			return err
		}
		if response.StatusCode != tc.status {
			response.Body.Close()
			return fmt.Errorf("large range status differs: %s", tc.name)
		}
		if tc.status == 416 {
			io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
			response.Body.Close()
			if response.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", size) {
				return errors.New("416 content range differs")
			}
		} else {
			if tc.status == 206 && response.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.start+tc.length-1, size) {
				response.Body.Close()
				return errors.New("206 content range differs")
			}
			expected := sha256.New()
			io.CopyBuffer(expected, &largePattern{offset: tc.start, end: tc.start + tc.length}, make([]byte, 64<<10))
			actual := sha256.New()
			n, e := io.CopyBuffer(actual, io.LimitReader(response.Body, tc.length+1), make([]byte, 64<<10))
			response.Body.Close()
			if e != nil || n != tc.length || !bytes.Equal(actual.Sum(nil), expected.Sum(nil)) {
				return errors.New("range bytes differ")
			}
		}
		check("actual_sdk_signed_large_" + tc.name)
	}
	core := minio.Core{Client: client}
	stage("inspect_original_large_multipart_target")
	_, targetErr := client.StatObject(ctx, c.Bucket, target, minio.StatObjectOptions{})
	if minio.ToErrorResponse(targetErr).Code == "NoSuchKey" {
		if state.UploadID == "" {
			// Recover an unknown create response by observing the original unique
			// target. A missing upload after an ambiguous create requires review.
			if previousStage == "before_multipart_create" {
				list, e := core.ListMultipartUploads(ctx, c.Bucket, target, "", "", "", 1000)
				if e != nil {
					return e
				}
				for _, u := range list.Uploads {
					if u.Key == target {
						if state.UploadID != "" {
							return errors.New("multiple original multipart uploads")
						}
						state.UploadID = u.UploadID
					}
				}
				if state.UploadID == "" {
					return errors.New("original multipart create outcome requires inspection")
				}
			} else {
				if err := save("before_multipart_create"); err != nil {
					return err
				}
				id, e := core.NewMultipartUpload(ctx, c.Bucket, target, minio.PutObjectOptions{ContentType: "application/octet-stream"})
				if e != nil {
					return e
				}
				state.UploadID = id
			}
			if err := save("multipart_created"); err != nil {
				return err
			}
		}
		parts, e := core.ListObjectParts(ctx, c.Bucket, target, state.UploadID, 0, 1000)
		if e != nil {
			return e
		}
		state.Parts = nil
		for number := 1; number <= 3; number++ {
			start := int64(number-1) * (6 << 20)
			length := int64(6 << 20)
			if length > size-start {
				length = size - start
			}
			h := sha256.New()
			md5sum := md5.New()
			io.CopyBuffer(io.MultiWriter(h, md5sum), &largePattern{offset: start, end: start + length}, make([]byte, 64<<10))
			partSHA := hex.EncodeToString(h.Sum(nil))
			partETag := hex.EncodeToString(md5sum.Sum(nil))
			etag := ""
			for _, p := range parts.ObjectParts {
				if p.PartNumber == number {
					if p.Size != length || strings.Trim(p.ETag, "\"") != partETag {
						return errors.New("original multipart part differs")
					}
					etag = strings.Trim(p.ETag, "\"")
				}
			}
			if etag == "" {
				if err := save(fmt.Sprintf("before_multipart_part_%d", number)); err != nil {
					return err
				}
				if number == 1 {
					p, err := core.CopyObjectPart(ctx, c.Bucket, source, c.Bucket, target, state.UploadID, number, start, length, nil)
					if err != nil {
						return err
					}
					etag = strings.Trim(p.ETag, "\"")
				} else {
					p, err := core.PutObjectPart(ctx, c.Bucket, target, state.UploadID, number, &largePattern{offset: start, end: start + length}, length, minio.PutObjectPartOptions{Sha256Hex: partSHA})
					if err != nil {
						return err
					}
					etag = strings.Trim(p.ETag, "\"")
				}
			}
			if etag != partETag {
				return errors.New("multipart part acknowledgement digest differs")
			}
			state.Parts = append(state.Parts, minio.CompletePart{PartNumber: number, ETag: etag})
			if err := save(fmt.Sprintf("multipart_part_%d_verified", number)); err != nil {
				return err
			}
		}
		if err := save("before_multipart_complete"); err != nil {
			return err
		}
		if _, err := core.CompleteMultipartUpload(ctx, c.Bucket, target, state.UploadID, state.Parts, minio.PutObjectOptions{}); err != nil {
			return err
		}
	} else if targetErr != nil {
		return targetErr
	}
	if err := verify(target); err != nil {
		return err
	}
	check("actual_sdk_17MiB_multipart_copy_large_source_range_complete_full_sha256")
	if err := save("multipart_verified"); err != nil {
		return err
	}
	for _, valid := range []bool{true, false} {
		key := "large/checksum-valid.txt"
		value := crc32.Checksum([]byte("123456789"), crc32.MakeTable(crc32.Castagnoli))
		status := 200
		if !valid {
			key = "large/checksum-rejected.txt"
			value++
			status = 400
		}
		stage("crc32c_checksum_" + fmt.Sprint(valid))
		_, err := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
		if err == nil && valid {
			continue
		}
		if minio.ToErrorResponse(err).Code != "NoSuchKey" {
			return errors.New("checksum target outcome requires inspection")
		}
		if err := save("before_crc32c_" + fmt.Sprint(valid)); err != nil {
			return err
		}
		r, err := http.NewRequestWithContext(ctx, "PUT", c.Endpoint+"/"+c.Bucket+"/"+key, strings.NewReader("123456789"))
		if err != nil {
			return err
		}
		b := []byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
		r.Header.Set("X-Amz-Checksum-Crc32c", base64.StdEncoding.EncodeToString(b))
		r.Header.Set("X-Amz-Content-Sha256", fmt.Sprintf("%x", sha256.Sum256([]byte("123456789"))))
		r.Header.Set("If-None-Match", "*")
		r = signer.SignV4(*r, c.Access, c.Secret, "", c.Region)
		response, err := (&http.Client{Transport: transport}).Do(r)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 8192))
		response.Body.Close()
		if err != nil || response.StatusCode != status {
			return errors.New("CRC32C validation response differs")
		}
		if !valid {
			if !bytes.Contains(body, []byte("<Code>BadDigest</Code>")) {
				return errors.New("bad checksum error code differs")
			}
			if _, err = client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
				return errors.New("bad checksum published object")
			}
		}
	}
	check("actual_sdk_crc32c_validated_and_bad_digest_rejected_before_publication")
	return save("complete")
}
