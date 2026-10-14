package control

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

type storageDeleteItem struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
}
type storageDeleteBatch struct {
	Objects []storageDeleteItem `json:"objects"`
}

// A Console batch is conditional and atomic: all versions must still match.
// S3 deliberately allows missing keys, as required by DeleteObjects. Both use
// the same branch directory lock and retain physical blobs for PITR/references.
func deleteStorageBatch(ctx context.Context, conn *pgx.Conn, bucket string, keys []string, expected map[string]string) error {
	if !storageBucketName.MatchString(bucket) || len(keys) == 0 || len(keys) > 1000 {
		return &storageMutationFault{400, "InvalidArgument"}
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if !validStorageKey(key) || seen[key] {
			return &storageMutationFault{400, "InvalidArgument"}
		}
		seen[key] = true
	}
	tx, e := conn.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = storageDirectoryLock(ctx, tx); e != nil {
		return e
	}
	var exists bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, bucket).Scan(&exists); e != nil {
		return e
	}
	if !exists {
		return &storageMutationFault{404, "NoSuchBucket"}
	}
	if expected != nil {
		rows, e := tx.Query(ctx, `SELECT key,sha256 FROM neon_storage.objects WHERE bucket=$1 AND key=ANY($2::text[]) FOR UPDATE`, bucket, keys)
		if e != nil {
			return e
		}
		actual, e := pgx.CollectRows(rows, pgx.RowToMap)
		if e != nil {
			return e
		}
		if len(actual) != len(keys) {
			return &storageMutationFault{412, "PreconditionFailed"}
		}
		for _, item := range actual {
			if expected[stringVal(item["key"])] != stringVal(item["sha256"]) {
				return &storageMutationFault{412, "PreconditionFailed"}
			}
		}
	}
	if _, e = tx.Exec(ctx, `DELETE FROM neon_storage.objects WHERE bucket=$1 AND key=ANY($2::text[])`, bucket, keys); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (s *server) storageDeleteBatch(w http.ResponseWriter, r *http.Request) {
	var input storageDeleteBatch
	if readJSON(r, &input) != nil || len(input.Objects) == 0 || len(input.Objects) > 1000 {
		fail(w, r, 422, "invalid_delete_batch", "One through 1000 observed object versions required")
		return
	}
	keys := []string{}
	expected := map[string]string{}
	for _, item := range input.Objects {
		if !validStorageKey(item.Key) || len(item.SHA256) != 64 || item.SHA256 != strings.ToLower(item.SHA256) || expected[item.Key] != "" {
			fail(w, r, 422, "invalid_delete_batch", "Unique valid keys and SHA-256 values required")
			return
		}
		if _, e := hex.DecodeString(item.SHA256); e != nil {
			fail(w, r, 422, "invalid_delete_batch", "Valid SHA-256 required")
			return
		}
		keys = append(keys, item.Key)
		expected[item.Key] = item.SHA256
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
	if e = deleteStorageBatch(r.Context(), conn, r.PathValue("bucket"), keys, expected); e != nil {
		var fault *storageMutationFault
		if errors.As(e, &fault) {
			fail(w, r, fault.Status, "object_batch_conflict", fault.Code)
		} else {
			s.storageFailure(w, r, e)
		}
		return
	}
	close()
	s.audit(r.Context(), userFrom(r).ID, "delete_object_batch", "object_storage", p.BranchID, "succeeded", requestID(r))
	jsonResponse(w, 200, record{"deleted_keys": keys, "physical_gc": "held_for_branch_and_pitr_references"})
}

type s3DeleteObject struct {
	Key     string                       `xml:"Key"`
	Version string                       `xml:"VersionId"`
	Unknown []struct{ XMLName xml.Name } `xml:",any"`
}
type s3DeleteRequest struct {
	XMLName xml.Name                     `xml:"Delete"`
	Objects []s3DeleteObject             `xml:"Object"`
	Quiet   bool                         `xml:"Quiet"`
	Unknown []struct{ XMLName xml.Name } `xml:",any"`
}
type s3DeleteFailure struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func parseS3DeleteBody(data []byte) (s3DeleteRequest, error) {
	var input s3DeleteRequest
	if len(data) == 0 || len(data) > 2<<20 {
		return input, errors.New("invalid XML size")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	if e := decoder.Decode(&input); e != nil {
		return input, e
	}
	var extra any
	if e := decoder.Decode(&extra); !errors.Is(e, io.EOF) {
		return input, errors.New("trailing XML")
	}
	if (input.XMLName.Space != "" && input.XMLName.Space != s3XMLNamespace) || len(input.Unknown) != 0 || len(input.Objects) == 0 || len(input.Objects) > 1000 {
		return input, errors.New("invalid Delete root or count")
	}
	return input, nil
}

func (s *server) s3DeleteObjects(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, bucket string, data []byte) {
	for _, header := range []string{"X-Amz-Mfa", "X-Amz-Bypass-Governance-Retention", "X-Amz-Request-Payer"} {
		if r.Header.Get(header) != "" {
			s3Error(w, r, 501, "NotImplemented")
			return
		}
	}
	checksum := md5.Sum(data)
	if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(checksum[:]) {
		s3Error(w, r, 400, "BadDigest")
		return
	}
	input, e := parseS3DeleteBody(data)
	if e != nil {
		s3Error(w, r, 400, "MalformedXML")
		return
	}
	var exists bool
	if e = conn.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, bucket).Scan(&exists); e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	if !exists {
		s3Error(w, r, 404, "NoSuchBucket")
		return
	}
	keys := []string{}
	seen := map[string]bool{}
	failed := []s3DeleteFailure{}
	for _, item := range input.Objects {
		if item.Version != "" || len(item.Unknown) != 0 {
			failed = append(failed, s3DeleteFailure{item.Key, "NotImplemented", "Version and conditional identifiers are unsupported"})
			continue
		}
		if !validStorageKey(item.Key) {
			failed = append(failed, s3DeleteFailure{item.Key, "InvalidArgument", "Invalid object key"})
			continue
		}
		if !seen[item.Key] {
			keys = append(keys, item.Key)
			seen[item.Key] = true
		}
	}
	if len(keys) > 0 {
		if e = deleteStorageBatch(r.Context(), conn, bucket, keys, nil); e != nil {
			var fault *storageMutationFault
			if errors.As(e, &fault) {
				s3Error(w, r, fault.Status, fault.Code)
			} else {
				s3Error(w, r, 503, "ServiceUnavailable")
			}
			return
		}
	}
	deleted := []struct {
		Key string `xml:"Key"`
	}{}
	if !input.Quiet {
		for _, key := range keys {
			deleted = append(deleted, struct {
				Key string `xml:"Key"`
			}{key})
		}
	}
	s3XML(w, 200, struct {
		XMLName xml.Name          `xml:"DeleteResult"`
		XMLNS   string            `xml:"xmlns,attr"`
		Deleted any               `xml:"Deleted"`
		Errors  []s3DeleteFailure `xml:"Error"`
	}{XMLNS: s3XMLNamespace, Deleted: deleted, Errors: failed})
}
