package main

import (
	"bytes"
	"context"
	"crypto/md5"
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

// Every dispatched multipart mutation is checkpointed to protected storage.
// Failed/unknown runs must inspect this original checkpoint before continuing.
func probeMultipart(ctx context.Context, client *minio.Client, c config, check func(string)) error {
	if !strings.HasPrefix(c.Output, "/run/e2e-private/") {
		return errors.New("protected multipart checkpoint required")
	}
	core := minio.Core{Client: client}
	key := "multipart/sdk-中文.bin"
	source := "multipart/ui.bin"
	checkpoint := map[string]any{"bucket": c.Bucket, "key": key, "stage": "create"}
	save := func(stage string) error {
		checkpoint["stage"] = stage
		v, e := json.Marshal(checkpoint)
		if e != nil {
			return e
		}
		return os.WriteFile(c.Output, v, 0o600)
	}
	if e := save("before_create"); e != nil {
		return e
	}
	id, e := core.NewMultipartUpload(ctx, c.Bucket, key, minio.PutObjectOptions{ContentType: "application/octet-stream", UserMetadata: map[string]string{"purpose": "sdk-multipart"}})
	if e != nil {
		return e
	}
	checkpoint["upload_id"] = id
	if e = save("created"); e != nil {
		return e
	}
	check("actual_signed_sdk_create_multipart")
	uploads, e := core.ListMultipartUploads(ctx, c.Bucket, "multipart/", "", "", "", 1)
	if e != nil || len(uploads.Uploads) != 1 || uploads.Uploads[0].UploadID != id {
		return errors.New("actual SDK upload list differs")
	}
	check("actual_signed_sdk_list_active_upload")
	firstBytes := bytes.Repeat([]byte("b"), 5<<20)
	md5sum := md5.Sum(firstBytes)
	sha := sha256.Sum256(firstBytes)
	if e = save("before_part_one"); e != nil {
		return e
	}
	first, e := core.PutObjectPart(ctx, c.Bucket, key, id, 1, bytes.NewReader(firstBytes), int64(len(firstBytes)), minio.PutObjectPartOptions{Md5Base64: base64.StdEncoding.EncodeToString(md5sum[:]), Sha256Hex: fmt.Sprintf("%x", sha)})
	if e != nil {
		return e
	}
	checkpoint["first_etag"] = first.ETag
	if e = save("part_one_committed"); e != nil {
		return e
	}
	if e = save("before_copy_part"); e != nil {
		return e
	}
	last, e := core.CopyObjectPart(ctx, c.Bucket, source, c.Bucket, key, id, 9, 0, 97, nil)
	if e != nil {
		return e
	}
	checkpoint["last_etag"] = last.ETag
	if e = save("copy_part_committed"); e != nil {
		return e
	}
	check("actual_signed_sdk_upload_part_and_copy_source_range")
	page, e := core.ListObjectParts(ctx, c.Bucket, key, id, 0, 1)
	if e != nil || len(page.ObjectParts) != 1 || !page.IsTruncated || page.ObjectParts[0].PartNumber != 1 {
		return errors.New("first actual SDK part page differs")
	}
	page2, e := core.ListObjectParts(ctx, c.Bucket, key, id, page.NextPartNumberMarker, 1)
	if e != nil || len(page2.ObjectParts) != 1 || page2.IsTruncated || page2.ObjectParts[0].PartNumber != 9 {
		return errors.New("second actual SDK part page differs")
	}
	check("actual_signed_sdk_nonconsecutive_part_pagination")
	if e = save("before_complete"); e != nil {
		return e
	}
	_, e = core.CompleteMultipartUpload(ctx, c.Bucket, key, id, []minio.CompletePart{{PartNumber: 1, ETag: first.ETag}, {PartNumber: 9, ETag: last.ETag}}, minio.PutObjectOptions{})
	if e != nil {
		return e
	}
	if e = save("completed"); e != nil {
		return e
	}
	obj, e := client.GetObject(ctx, c.Bucket, key, minio.GetObjectOptions{})
	if e != nil {
		return e
	}
	content, e := io.ReadAll(io.LimitReader(obj, (8<<20)+1))
	_ = obj.Close()
	expected := append(firstBytes, bytes.Repeat([]byte("u"), 97)...)
	if e != nil || !bytes.Equal(content, expected) {
		return errors.New("actual SDK multipart bytes differ")
	}
	stat, e := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
	if e != nil || stat.ETag != fmt.Sprintf("%x", sha256.Sum256(expected)) || stat.Metadata.Get("X-Amz-Meta-Purpose") != "sdk-multipart" {
		return errors.New("actual SDK completion metadata differs")
	}
	check("actual_signed_sdk_complete_full_bytes_digest_and_metadata")
	if e = save("before_abort_create"); e != nil {
		return e
	}
	abortID, e := core.NewMultipartUpload(ctx, c.Bucket, "multipart/aborted.bin", minio.PutObjectOptions{})
	if e != nil {
		return e
	}
	checkpoint["abort_upload_id"] = abortID
	if e = save("before_abort"); e != nil {
		return e
	}
	if e = core.AbortMultipartUpload(ctx, c.Bucket, "multipart/aborted.bin", abortID); e != nil {
		return e
	}
	_, e = core.ListObjectParts(ctx, c.Bucket, "multipart/aborted.bin", abortID, 0, 1000)
	if minio.ToErrorResponse(e).Code != "NoSuchUpload" {
		return errors.New("aborted SDK upload remained writable/listable")
	}
	if e = save("aborted"); e != nil {
		return e
	}
	check("actual_signed_sdk_abort_closes_upload_without_deleting_fixture_bytes")
	return nil
}
