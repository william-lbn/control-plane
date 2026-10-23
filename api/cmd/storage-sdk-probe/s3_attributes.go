package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
)

func probeS3Attributes(ctx context.Context, client *minio.Client, c config, stage, check func(string)) error {
	if !strings.HasPrefix(c.Output, "/run/e2e-private/") || !strings.HasPrefix(c.Bucket, "ci-s3-attributes-") {
		return errors.New("protected attribute fixture required")
	}
	state := struct {
		Bucket  string   `json:"bucket"`
		Checks  []string `json:"checks"`
		Running string   `json:"running"`
	}{Bucket: c.Bucket, Checks: []string{}}
	if raw, e := os.ReadFile(c.Output); e == nil {
		if json.Unmarshal(raw, &state) != nil || state.Bucket != c.Bucket {
			return errors.New("original attribute checkpoint differs")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	for _, v := range state.Checks {
		check(v)
	}
	save := func() error {
		raw, e := json.Marshal(state)
		if e != nil {
			return e
		}
		return os.WriteFile(c.Output, raw, 0o600)
	}
	step := func(name string, run func() error) error {
		for _, v := range state.Checks {
			if v == name {
				return nil
			}
		}
		stage(name)
		if state.Running != "" {
			return errors.New("inspect original attribute mutation before resuming")
		}
		state.Running = name
		if e := save(); e != nil {
			return e
		}
		if e := run(); e != nil {
			return e
		}
		state.Running = ""
		state.Checks = append(state.Checks, name)
		if e := save(); e != nil {
			return e
		}
		check(name)
		return nil
	}
	first := bytes.Repeat([]byte("owned attribute multipart bytes\n"), 180000)[:5<<20]
	tail := []byte("owned attribute tail\n")
	content := append(append([]byte{}, first...), tail...)
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	key := "attributes/multipart.bin"
	if e := step("SDK_new_multipart_publishes_exact_selected_part_layout", func() error {
		core := minio.Core{Client: client}
		id, e := core.NewMultipartUpload(ctx, c.Bucket, key, minio.PutObjectOptions{ContentType: "application/octet-stream"})
		if e != nil {
			return e
		}
		a, e := core.PutObjectPart(ctx, c.Bucket, key, id, 2, bytes.NewReader(first), int64(len(first)), minio.PutObjectPartOptions{})
		if e != nil {
			return e
		}
		b, e := core.PutObjectPart(ctx, c.Bucket, key, id, 7, bytes.NewReader(tail), int64(len(tail)), minio.PutObjectPartOptions{})
		if e != nil {
			return e
		}
		_, e = core.CompleteMultipartUpload(ctx, c.Bucket, key, id, []minio.CompletePart{{PartNumber: 2, ETag: a.ETag}, {PartNumber: 7, ETag: b.ETag}}, minio.PutObjectOptions{})
		return e
	}); e != nil {
		return e
	}
	if e := step("SDK_typed_attributes_pages_checksums_and_terminal_marker", func() error {
		a, e := client.GetObjectAttributes(ctx, c.Bucket, key, minio.ObjectAttributesOptions{MaxParts: 1, VersionID: "null"})
		if e != nil {
			return e
		}
		partSHA := sha256.Sum256(first)
		fullSHA := sha256.Sum256(content)
		if a.ETag != digest || a.ObjectSize != len(content) || a.LastModified.IsZero() || a.VersionID != "null" || a.ObjectParts.PartsCount != 2 || len(a.ObjectParts.Parts) != 1 || a.ObjectParts.Parts[0].PartNumber != 2 || a.ObjectParts.Parts[0].Size != len(first) || a.ObjectParts.Parts[0].ChecksumSHA256 != base64.StdEncoding.EncodeToString(partSHA[:]) || a.Checksum.ChecksumSHA256 != base64.StdEncoding.EncodeToString(fullSHA[:]) || a.Checksum.ChecksumType != "FULL_OBJECT" || !a.ObjectParts.IsTruncated || a.ObjectParts.NextPartNumberMarker != 2 {
			return errors.New("first typed attribute page differs")
		}
		a, e = client.GetObjectAttributes(ctx, c.Bucket, key, minio.ObjectAttributesOptions{MaxParts: 1, PartNumberMarker: 2})
		if e != nil {
			return e
		}
		tailSHA := sha256.Sum256(tail)
		if a.ObjectParts.PartsCount != 2 || len(a.ObjectParts.Parts) != 1 || a.ObjectParts.Parts[0].PartNumber != 7 || a.ObjectParts.Parts[0].ChecksumSHA256 != base64.StdEncoding.EncodeToString(tailSHA[:]) || a.ObjectParts.IsTruncated {
			return errors.New("last typed attribute page differs")
		}
		a, e = client.GetObjectAttributes(ctx, c.Bucket, key, minio.ObjectAttributesOptions{MaxParts: 1, PartNumberMarker: 7})
		if e != nil {
			return e
		}
		if len(a.ObjectParts.Parts) != 0 || a.ObjectParts.PartsCount != 2 || a.ObjectParts.IsTruncated {
			return errors.New("terminal attribute marker differs")
		}
		return nil
	}); e != nil {
		return e
	}
	if e := step("SDK_single_COPY_clears_multipart_layout_and_preserves_exact_bytes", func() error {
		if _, e := client.CopyObject(ctx, minio.CopyDestOptions{Bucket: c.Bucket, Object: "attributes/copied.bin"}, minio.CopySrcOptions{Bucket: c.Bucket, Object: key}); e != nil {
			return e
		}
		a, e := client.GetObjectAttributes(ctx, c.Bucket, "attributes/copied.bin", minio.ObjectAttributesOptions{})
		if e != nil {
			return e
		}
		if a.ObjectParts.PartsCount != 0 || a.ObjectSize != len(content) || a.ETag != digest {
			return errors.New("single COPY retained source multipart layout")
		}
		obj, e := client.GetObject(ctx, c.Bucket, "attributes/copied.bin", minio.GetObjectOptions{})
		if e != nil {
			return e
		}
		defer obj.Close()
		raw, e := io.ReadAll(io.LimitReader(obj, int64(len(content)+1)))
		if e != nil {
			return e
		}
		if !bytes.Equal(raw, content) {
			return errors.New("copy bytes differ")
		}
		return nil
	}); e != nil {
		return e
	}
	return step("SDK_normal_overwrite_replaces_generation_and_clears_old_parts", func() error {
		content := []byte("Owned replacement generation, previous multipart receipt retained.\n")
		if _, e := client.PutObject(ctx, c.Bucket, key, bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{DisableMultipart: true, SendContentMd5: true}); e != nil {
			return e
		}
		a, e := client.GetObjectAttributes(ctx, c.Bucket, key, minio.ObjectAttributesOptions{})
		if e != nil {
			return e
		}
		if a.ObjectParts.PartsCount != 0 || a.ObjectSize != len(content) || a.ETag != fmt.Sprintf("%x", sha256.Sum256(content)) {
			return errors.New("overwrite retained previous part layout")
		}
		return nil
	})
}
