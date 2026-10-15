package control

import (
	"context"
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type storageCopyRequest struct {
	SourceBucket   string `json:"source_bucket"`
	SourceKey      string `json:"source_key"`
	DestinationKey string `json:"destination_key"`
	SourceSHA256   string `json:"source_sha256"`
}

type storageMutationFault struct {
	Status int
	Code   string
}

func (e *storageMutationFault) Error() string { return e.Code }

type storageCopyMetadata struct {
	Metadata    map[string]string
	ContentType string
}

// One metadata transaction references the original immutable, project-owned
// blob. It never fetches an external URL or deletes a shared blob. The branch
// directory lock serializes copy with put/delete/quota checks and PITR metadata.
func copyStorageObject(ctx context.Context, conn *pgx.Conn, project, srcBucket, srcKey, dstBucket, dstKey, srcMatch, dstMatch, dstNone string, replacements ...storageCopyMetadata) (record, error) {
	return copyStorageObjectWithConditions(ctx, conn, project, srcBucket, srcKey, dstBucket, dstKey, storageCopyConditions{Match: srcMatch}, dstMatch, dstNone, replacements...)
}

func copyStorageObjectWithConditions(ctx context.Context, conn *pgx.Conn, project, srcBucket, srcKey, dstBucket, dstKey string, source storageCopyConditions, dstMatch, dstNone string, replacements ...storageCopyMetadata) (record, error) {
	if len(replacements) > 1 {
		return nil, &storageMutationFault{400, "InvalidArgument"}
	}
	var replacement *storageCopyMetadata
	if len(replacements) == 1 {
		replacement = &replacements[0]
		value, e := normalizeStorageMetadata(replacement.Metadata)
		if e != nil {
			return nil, &storageMutationFault{400, "InvalidArgument"}
		}
		replacement.Metadata = value
		if len(replacement.ContentType) > 256 {
			return nil, &storageMutationFault{400, "InvalidArgument"}
		}
		if _, _, e := mime.ParseMediaType(replacement.ContentType); e != nil {
			return nil, &storageMutationFault{400, "InvalidArgument"}
		}
	}
	if !storageBucketName.MatchString(srcBucket) || !storageBucketName.MatchString(dstBucket) || !validStorageKey(srcKey) || !validStorageKey(dstKey) || (srcBucket == dstBucket && srcKey == dstKey && replacement == nil) {
		return nil, &storageMutationFault{400, "InvalidArgument"}
	}
	tx, e := conn.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if e = storageDirectoryLock(ctx, tx); e != nil {
		return nil, e
	}
	var bucketExists bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, dstBucket).Scan(&bucketExists); e != nil {
		return nil, e
	}
	if !bucketExists {
		return nil, &storageMutationFault{404, "NoSuchBucket"}
	}
	var blob, digest, contentType string
	var metadata map[string]string
	var size int64
	var sourceModified time.Time
	if e = tx.QueryRow(ctx, `SELECT blob_key,sha256,size,content_type,metadata,updated_at FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, srcBucket, srcKey).Scan(&blob, &digest, &size, &contentType, &metadata, &sourceModified); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return nil, &storageMutationFault{404, "NoSuchKey"}
		}
		return nil, e
	}
	if !strings.HasPrefix(blob, project+"/") {
		return nil, &storageMutationFault{409, "ObjectIdentityMismatch"}
	}
	if !source.accepts(digest, sourceModified) {
		return nil, &storageMutationFault{412, "PreconditionFailed"}
	}
	if replacement != nil {
		metadata = replacement.Metadata
		contentType = replacement.ContentType
	}
	var oldDigest string
	var oldSize int64
	e = tx.QueryRow(ctx, `SELECT sha256,size FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, dstBucket, dstKey).Scan(&oldDigest, &oldSize)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return nil, e
	}
	if (dstNone != "" && (dstNone != "*" || oldDigest != "")) || (dstMatch != "" && (oldDigest == "" || dstMatch != "\""+oldDigest+"\"")) {
		return nil, &storageMutationFault{412, "PreconditionFailed"}
	}
	var count int
	var total int64
	if e = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(size),0)::bigint FROM neon_storage.objects`).Scan(&count, &total); e != nil {
		return nil, e
	}
	if (oldDigest == "" && count >= 1000) || total-oldSize+size > storageBranchLimit {
		return nil, &storageMutationFault{429, "QuotaExceeded"}
	}
	var modified time.Time
	e = tx.QueryRow(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type,metadata) VALUES($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT(bucket,key) DO UPDATE SET blob_key=$3,sha256=$4,size=$5,content_type=$6,metadata=$7,updated_at=now() RETURNING updated_at`, dstBucket, dstKey, blob, digest, size, contentType, metadata).Scan(&modified)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return record{"key": dstKey, "bucket": dstBucket, "sha256": digest, "size": size, "content_type": contentType, "updated_at": modified.UTC()}, nil
}

func parseS3CopySource(raw string) (string, string, error) {
	if raw == "" || len(raw) > 4096 || strings.HasPrefix(raw, "//") {
		return "", "", errors.New("invalid source")
	}
	u, e := url.Parse("/" + strings.TrimPrefix(raw, "/"))
	if e != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil {
		return "", "", errors.New("unsupported versioned copy source")
	}
	pieces := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
	if len(pieces) != 2 || !storageBucketName.MatchString(pieces[0]) || !validStorageKey(pieces[1]) {
		return "", "", errors.New("invalid source")
	}
	return pieces[0], pieces[1], nil
}

func (s *server) storageCopy(w http.ResponseWriter, r *http.Request) {
	var input storageCopyRequest
	if readJSON(r, &input) != nil || !storageBucketName.MatchString(input.SourceBucket) || !validStorageKey(input.SourceKey) || !validStorageKey(input.DestinationKey) || len(input.SourceSHA256) != 64 {
		fail(w, r, 422, "invalid_copy_request", "Valid source bucket/key/hash and destination key required")
		return
	}
	if _, e := hex.DecodeString(input.SourceSHA256); e != nil {
		fail(w, r, 422, "invalid_copy_request", "SHA-256 must be hex")
		return
	}
	// Console writes require explicit create/overwrite semantics, unlike the S3
	// protocol's default overwrite. A POST cannot bypass conditional write policy.
	if r.Header.Get("If-Match") == "" && r.Header.Get("If-None-Match") != "*" {
		fail(w, r, 428, "object_version_required", "Use If-None-Match: * or the current target ETag")
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
	item, e := copyStorageObject(r.Context(), conn, p.ProjectID, input.SourceBucket, input.SourceKey, r.PathValue("bucket"), input.DestinationKey, "\""+input.SourceSHA256+"\"", r.Header.Get("If-Match"), r.Header.Get("If-None-Match"))
	if e != nil {
		var fault *storageMutationFault
		if errors.As(e, &fault) {
			fail(w, r, fault.Status, "object_copy_conflict", fault.Code)
		} else {
			s.storageFailure(w, r, e)
		}
		return
	}
	close()
	s.audit(r.Context(), userFrom(r).ID, "copy_object", "object_storage", p.BranchID, "succeeded", requestID(r))
	w.Header().Set("ETag", "\""+stringVal(item["sha256"])+"\"")
	jsonResponse(w, 201, item)
}
