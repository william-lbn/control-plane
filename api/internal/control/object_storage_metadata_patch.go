package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"mime"
	"net/http"
	"strings"
)

// This is a representation ETag, not a secret or a monotonically increasing
// mutation sequence. Identical metadata and the same immutable blob match.
func storageMetadataState(branch, bucket, key, blob, digest, contentType string, metadata map[string]string) string {
	if metadata == nil {
		metadata = map[string]string{}
	}
	raw, _ := json.Marshal(struct {
		Branch, Bucket, Key, Blob, Digest, ContentType string
		Metadata                                       map[string]string
	}{branch, bucket, key, blob, digest, contentType, metadata})
	return fmt.Sprintf(`"%x"`, sha256.Sum256(raw))
}
func updateStorageMetadata(ctx context.Context, conn *pgx.Conn, p objectStoragePayload, bucket, key, expected, contentType string, metadata map[string]string) (record, error) {
	if !validStorageKey(key) || !storageBucketName.MatchString(bucket) {
		return nil, &storageMutationFault{422, "InvalidArgument"}
	}
	normalized, e := normalizeStorageMetadata(metadata)
	if e != nil {
		return nil, &storageMutationFault{422, "InvalidArgument"}
	}
	tx, e := conn.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if e = storageDirectoryLock(ctx, tx); e != nil {
		return nil, e
	}
	var blob, digest, oldType string
	var old map[string]string
	if e = tx.QueryRow(ctx, `SELECT blob_key,sha256,content_type,metadata FROM neon_storage.objects WHERE bucket=$1 AND key=$2 FOR UPDATE`, bucket, key).Scan(&blob, &digest, &oldType, &old); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return nil, &storageMutationFault{404, "NoSuchKey"}
		}
		return nil, e
	}
	if !strings.HasPrefix(blob, p.ProjectID+"/") {
		return nil, &storageMutationFault{409, "ObjectIdentityMismatch"}
	}
	if expected != storageMetadataState(p.BranchID, bucket, key, blob, digest, oldType, old) {
		return nil, &storageMutationFault{412, "PreconditionFailed"}
	}
	if contentType == "" {
		contentType = oldType
	}
	if len(contentType) > 256 {
		return nil, &storageMutationFault{422, "InvalidArgument"}
	}
	if _, _, e = mime.ParseMediaType(contentType); e != nil {
		return nil, &storageMutationFault{422, "InvalidArgument"}
	}
	if _, e = tx.Exec(ctx, `UPDATE neon_storage.objects SET metadata=$3,content_type=$4,updated_at=now() WHERE bucket=$1 AND key=$2`, bucket, key, normalized, contentType); e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return record{"metadata": normalized, "content_type": contentType, "metadata_etag": storageMetadataState(p.BranchID, bucket, key, blob, digest, contentType, normalized), "sha256": digest, "key": key}, nil
}
func (s *server) storageMetadataPatch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("If-Match") == "" {
		fail(w, r, 428, "metadata_version_required", "Current X-Neon-Metadata-ETag required")
		return
	}
	var input struct {
		Metadata    map[string]string `json:"metadata"`
		ContentType string            `json:"content_type"`
	}
	if readJSON(r, &input) != nil || input.Metadata == nil {
		fail(w, r, 422, "invalid_metadata", "Metadata JSON required")
		return
	}
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, p, close, e := s.openObjectStorage(r, r.PathValue("project"), r.PathValue("branch"))
	if e != nil {
		s.storageFailure(w, r, e)
		return
	}
	defer close()
	item, e := updateStorageMetadata(r.Context(), conn, p, r.PathValue("bucket"), r.URL.Query().Get("key"), r.Header.Get("If-Match"), input.ContentType, input.Metadata)
	if e != nil {
		if f, ok := e.(*storageMutationFault); ok {
			fail(w, r, f.Status, "object_metadata_conflict", "Metadata/key/type/version rejected; refresh original state")
		} else {
			s.storageFailure(w, r, e)
		}
		return
	}
	w.Header().Set("X-Neon-Metadata-ETag", stringVal(item["metadata_etag"]))
	jsonResponse(w, 200, item)
	close()
	s.audit(r.Context(), userFrom(r).ID, "update_object_metadata", "object_storage", p.BranchID, "succeeded", requestID(r))
}
