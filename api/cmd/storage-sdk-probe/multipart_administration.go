package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
)

// Deliberately retain these owned uploads: the UI revokes this credential and
// demonstrates audited administrator recovery without transferring byte access.
func probeMultipartAdministration(ctx context.Context, client *minio.Client, c config, check func(string)) error {
	if !strings.HasPrefix(c.Output, "/run/e2e-private/") {
		return errors.New("protected administration checkpoint required")
	}
	core := minio.Core{Client: client}
	ids := []string{}
	keys := []string{"maintenance/group/a.bin", "maintenance/group/b.bin", "maintenance/z.bin"}
	state := map[string]any{"bucket": c.Bucket, "keys": keys, "upload_ids": ids, "stage": "before_create"}
	save := func(stage string) error {
		state["stage"] = stage
		state["upload_ids"] = ids
		v, e := json.Marshal(state)
		if e != nil {
			return e
		}
		return os.WriteFile(c.Output, v, 0o600)
	}
	for _, key := range keys {
		state["next_key"] = key
		if e := save("before_create"); e != nil {
			return e
		}
		id, e := core.NewMultipartUpload(ctx, c.Bucket, key, minio.PutObjectOptions{ContentType: "application/octet-stream"})
		if e != nil {
			return e
		}
		ids = append(ids, id)
		if e = save("created"); e != nil {
			return e
		}
	}
	payload := []byte("Owned retired-credential multipart fixture; bytes retained")
	if e := save("before_part"); e != nil {
		return e
	}
	if _, e := core.PutObjectPart(ctx, c.Bucket, keys[0], ids[0], 1, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectPartOptions{}); e != nil {
		return e
	}
	if e := save("part_committed"); e != nil {
		return e
	}
	check("actual_sdk_three_owned_uploads_and_retained_part_created")
	first, e := core.ListMultipartUploads(ctx, c.Bucket, "maintenance/", "", "", "/", 1)
	if e != nil || !first.IsTruncated || len(first.CommonPrefixes) != 1 || first.CommonPrefixes[0].Prefix != "maintenance/group/" || len(first.Uploads) != 0 {
		return errors.New("actual SDK grouped first page differs")
	}
	second, e := core.ListMultipartUploads(ctx, c.Bucket, "maintenance/", first.NextKeyMarker, first.NextUploadIDMarker, "/", 1)
	if e != nil || second.IsTruncated || len(second.CommonPrefixes) != 0 || len(second.Uploads) != 1 || second.Uploads[0].UploadID != ids[2] {
		return errors.New("actual SDK grouped second page differs")
	}
	check("actual_signed_sdk_multipart_common_prefix_count_and_marker_pagination")
	inside, e := core.ListMultipartUploads(ctx, c.Bucket, "maintenance/", "maintenance/group/a.bin", "", "/", 10)
	if e != nil || len(inside.CommonPrefixes) != 0 || len(inside.Uploads) != 1 || inside.Uploads[0].UploadID != ids[2] {
		return errors.New("actual SDK marker inside group repeated old prefix")
	}
	check("actual_signed_sdk_marker_inside_group_excludes_common_prefix")
	return save("ready_for_credential_revocation_and_admin_abort")
}
