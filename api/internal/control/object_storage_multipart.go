package control

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const multipartActiveLimit = 32
const multipartPartLimit = 1000
const multipartMinimumPart = 5 << 20

var multipartID = regexp.MustCompile(`^mpu_[a-f0-9]{24}$`)
var multipartETag = regexp.MustCompile(`^[a-f0-9]{32}$`)

type multipartUpload struct {
	ID              string            `json:"upload_id"`
	Key             string            `json:"key"`
	ContentType     string            `json:"content_type"`
	Metadata        map[string]string `json:"metadata"`
	ExpectedSHA256  string            `json:"expected_sha256"`
	ExpectedSize    int64             `json:"expected_size"`
	State           string            `json:"state"`
	Created         time.Time         `json:"created_at"`
	SHA256          string            `json:"sha256"`
	Size            int64             `json:"size"`
	Tags            []storageTag      `json:"tags,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	completion      string
}
type multipartPart struct {
	Number   int       `json:"part_number" xml:"PartNumber"`
	ETag     string    `json:"etag" xml:"ETag"`
	Size     int64     `json:"size" xml:"Size"`
	SHA256   string    `json:"sha256" xml:"-"`
	Modified time.Time `json:"updated_at" xml:"LastModified"`
	blob     string
}
type multipartSelection struct {
	Number int    `json:"part_number" xml:"PartNumber"`
	ETag   string `json:"etag" xml:"ETag"`
}
type multipartCreate struct {
	Key             string            `json:"key"`
	ContentType     string            `json:"content_type"`
	Metadata        map[string]string `json:"metadata"`
	SHA256          string            `json:"sha256"`
	Size            int64             `json:"size"`
	Tags            []storageTag      `json:"tags,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
}

func multipartFault(status int, code string) error { return &storageMutationFault{status, code} }

// Every mutation shares the branch directory lock, including normal PUT/COPY/
// DELETE. Publication and the completed receipt use ONE transaction. A lost
// response can be retried without ever applying publication a second time.
func multipartTx(ctx context.Context, conn *pgx.Conn) (pgx.Tx, error) {
	tx, e := conn.Begin(ctx)
	if e != nil {
		return nil, e
	}
	if e = storageDirectoryLock(ctx, tx); e != nil {
		_ = tx.Rollback(ctx)
		return nil, e
	}
	return tx, nil
}
func loadMultipart(ctx context.Context, tx pgx.Tx, p objectStoragePayload, principal, bucket, key, id string) (multipartUpload, error) {
	var u multipartUpload
	if !multipartID.MatchString(id) {
		return u, multipartFault(404, "NoSuchUpload")
	}
	e := tx.QueryRow(ctx, `SELECT id,key,content_type,metadata,expected_sha256,expected_size,state,created_at,result_sha256,result_size,completion_hash,tags,response_headers FROM neon_storage.multipart_uploads WHERE id=$1 AND branch_id=$2 AND principal=$3 AND bucket=$4 AND ($5='' OR key=$5) FOR UPDATE`, id, p.BranchID, principal, bucket, key).Scan(&u.ID, &u.Key, &u.ContentType, &u.Metadata, &u.ExpectedSHA256, &u.ExpectedSize, &u.State, &u.Created, &u.SHA256, &u.Size, &u.completion, &u.Tags, &u.ResponseHeaders)
	if isNoRows(e) {
		e = multipartFault(404, "NoSuchUpload")
	}
	return u, e
}
func (s *server) createMultipart(ctx context.Context, conn *pgx.Conn, p objectStoragePayload, principal, bucket, requestKey string, input multipartCreate) (multipartUpload, error) {
	var u multipartUpload
	if !validStorageKey(input.Key) || !storageBucketName.MatchString(bucket) || len(input.ContentType) > 256 || input.Size < -1 || input.Size > storageObjectLimit || (input.SHA256 != "" && !storageSHA256.MatchString(input.SHA256)) {
		return u, multipartFault(400, "InvalidArgument")
	}
	if input.ContentType == "" {
		input.ContentType = "application/octet-stream"
	}
	if _, _, e := mime.ParseMediaType(input.ContentType); e != nil {
		return u, multipartFault(400, "InvalidArgument")
	}
	metadata, e := normalizeStorageMetadata(input.Metadata)
	if e != nil {
		return u, multipartFault(400, "InvalidArgument")
	}
	input.Metadata = metadata
	input.Tags, e = normalizeStorageTags(input.Tags, 10)
	if e != nil {
		return u, multipartFault(400, "InvalidTag")
	}
	input.ResponseHeaders, e = normalizeStorageResponseHeaders(input.ResponseHeaders)
	if e != nil {
		return u, multipartFault(400, "InvalidArgument")
	}
	raw, _ := json.Marshal(input)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(append([]byte(bucket+"\x00"), raw...)))
	tx, e := multipartTx(ctx, conn)
	if e != nil {
		return u, e
	}
	defer tx.Rollback(ctx)
	if requestKey != "" {
		var id, prior string
		e = tx.QueryRow(ctx, `SELECT id,request_hash FROM neon_storage.multipart_uploads WHERE branch_id=$1 AND principal=$2 AND request_key=$3`, p.BranchID, principal, keyHash(requestKey)).Scan(&id, &prior)
		if e == nil {
			if prior != fingerprint {
				return u, multipartFault(409, "IdempotencyConflict")
			}
			u, e = loadMultipart(ctx, tx, p, principal, bucket, input.Key, id)
			if e == nil {
				e = tx.Commit(ctx)
			}
			return u, e
		}
		if !isNoRows(e) {
			return u, e
		}
	}
	var exists bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, bucket).Scan(&exists); e != nil {
		return u, e
	}
	if !exists {
		return u, multipartFault(404, "NoSuchBucket")
	}
	var active int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM neon_storage.multipart_uploads WHERE branch_id=$1 AND state='uploading'`, p.BranchID).Scan(&active); e != nil {
		return u, e
	}
	if active >= multipartActiveLimit {
		return u, multipartFault(429, "QuotaExceeded")
	}
	u.ID = newID("mpu_")
	_, e = tx.Exec(ctx, `INSERT INTO neon_storage.multipart_uploads(id,branch_id,principal,bucket,key,content_type,metadata,expected_sha256,expected_size,request_key,request_hash,tags,response_headers) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13)`, u.ID, p.BranchID, principal, bucket, input.Key, input.ContentType, metadata, input.SHA256, input.Size, func() string {
		if requestKey != "" {
			return keyHash(requestKey)
		}
		return ""
	}(), fingerprint, input.Tags, input.ResponseHeaders)
	if e == nil {
		u, e = loadMultipart(ctx, tx, p, principal, bucket, input.Key, u.ID)
	}
	if e == nil {
		e = tx.Commit(ctx)
	}
	return u, e
}
func (s *server) putMultipartPart(ctx context.Context, conn *pgx.Conn, p objectStoragePayload, principal, bucket, key, id string, number int, data []byte, contentMD5 string, copyHeaders http.Header) (multipartPart, error) {
	var part multipartPart
	if number < 1 || number > 10000 || len(data) > storagePartLimit {
		return part, multipartFault(400, "InvalidArgument")
	}
	tx, e := multipartTx(ctx, conn)
	if e != nil {
		return part, e
	}
	defer tx.Rollback(ctx)
	u, e := loadMultipart(ctx, tx, p, principal, bucket, key, id)
	if e != nil {
		return part, e
	}
	if u.State != "uploading" {
		return part, multipartFault(404, "NoSuchUpload")
	}
	if source := copyHeaders.Get("X-Amz-Copy-Source"); source != "" {
		if len(data) != 0 {
			return part, multipartFault(400, "InvalidRequest")
		}
		sb, sk, err := parseS3CopySource(source)
		if err != nil {
			return part, multipartFault(400, "InvalidArgument")
		}
		conditions, err := parseStorageCopyConditions(copyHeaders)
		if err != nil {
			return part, err
		}
		var blob, digest string
		var size int64
		var modified time.Time
		err = tx.QueryRow(ctx, `SELECT blob_key,sha256,size,updated_at FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, sb, sk).Scan(&blob, &digest, &size, &modified)
		if isNoRows(err) {
			return part, multipartFault(404, "NoSuchKey")
		}
		if err != nil {
			return part, err
		}
		if !conditions.accepts(digest, modified) {
			return part, multipartFault(412, "PreconditionFailed")
		}
		if !strings.HasPrefix(blob, p.ProjectID+"/") {
			return part, multipartFault(409, "ObjectIdentityMismatch")
		}
		a, b := int64(0), size-1
		if value := copyHeaders.Get("X-Amz-Copy-Source-Range"); value != "" {
			a, b, err = multipartCopyRange(value, size)
			if err != nil {
				return part, err
			}
		}
		if b-a+1 > storagePartLimit {
			return part, multipartFault(413, "EntityTooLarge")
		}
		source, err := s.storage.readPayload(ctx, blob, digest, size)
		if err != nil {
			return part, err
		}
		defer source.Close()
		reader, err := source.Reader()
		if err != nil {
			return part, err
		}
		if _, err = reader.Seek(a, io.SeekStart); err != nil {
			return part, err
		}
		data, err = io.ReadAll(io.LimitReader(reader, b-a+1))
		if err != nil || int64(len(data)) != b-a+1 {
			return part, errors.New("original part-copy range unavailable")
		}

	}
	sum := md5.Sum(data)
	if contentMD5 != "" && base64.StdEncoding.EncodeToString(sum[:]) != contentMD5 {
		return part, multipartFault(400, "BadDigest")
	}
	var count int
	var total, old int64
	e = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(size),0)::bigint FROM neon_storage.multipart_parts WHERE upload_id=$1`, id).Scan(&count, &total)
	if e != nil {
		return part, e
	}
	e = tx.QueryRow(ctx, `SELECT size FROM neon_storage.multipart_parts WHERE upload_id=$1 AND part_number=$2`, id, number).Scan(&old)
	fresh := isNoRows(e)
	if e != nil && !fresh {
		return part, e
	}
	if fresh && count >= multipartPartLimit || total-old+int64(len(data)) > storageObjectLimit {
		return part, multipartFault(429, "QuotaExceeded")
	}
	var pending int64
	e = tx.QueryRow(ctx, `SELECT COALESCE(sum(v.size),0)::bigint FROM neon_storage.multipart_parts v JOIN neon_storage.multipart_uploads u ON u.id=v.upload_id WHERE u.branch_id=$1 AND u.state='uploading'`, p.BranchID).Scan(&pending)
	if e != nil {
		return part, e
	}
	if pending-old+int64(len(data)) > storageBranchLimit {
		return part, multipartFault(429, "QuotaExceeded")
	}
	part = multipartPart{Number: number, ETag: fmt.Sprintf("%x", sum), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data)), blob: p.ProjectID + "/" + newID("part_")}
	if e = s.storage.put(ctx, part.blob, "application/octet-stream", data); e != nil {
		return part, e
	}
	e = tx.QueryRow(ctx, `INSERT INTO neon_storage.multipart_parts(upload_id,part_number,blob_key,sha256,etag,size) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(upload_id,part_number) DO UPDATE SET blob_key=$3,sha256=$4,etag=$5,size=$6,updated_at=now() RETURNING updated_at`, id, number, part.blob, part.SHA256, part.ETag, part.Size).Scan(&part.Modified)
	if e == nil {
		e = tx.Commit(ctx)
	}
	return part, e
}
func multipartCopyRange(value string, size int64) (int64, int64, error) {
	v := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if !strings.HasPrefix(value, "bytes=") || len(v) != 2 {
		return 0, 0, multipartFault(400, "InvalidArgument")
	}
	a, e := strconv.ParseInt(v[0], 10, 64)
	b, f := strconv.ParseInt(v[1], 10, 64)
	if e != nil || f != nil || a < 0 || b < a || b >= size {
		return 0, 0, multipartFault(416, "InvalidRange")
	}
	return a, b, nil
}
func listMultipartParts(ctx context.Context, tx pgx.Tx, id string) ([]multipartPart, error) {
	rows, e := tx.Query(ctx, `SELECT part_number,etag,size,sha256,updated_at,blob_key FROM neon_storage.multipart_parts WHERE upload_id=$1 ORDER BY part_number`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []multipartPart{}
	for rows.Next() {
		var p multipartPart
		if e = rows.Scan(&p.Number, &p.ETag, &p.Size, &p.SHA256, &p.Modified, &p.blob); e != nil {
			return nil, e
		}
		items = append(items, p)
	}
	return items, rows.Err()
}
func multipartManifestHash(parts []multipartSelection, match, none string) (string, error) {
	if len(parts) == 0 || len(parts) > multipartPartLimit {
		return "", multipartFault(400, "InvalidPart")
	}
	last := 0
	for i := range parts {
		v := &parts[i]
		if v.Number <= last || v.Number > 10000 {
			return "", multipartFault(400, "InvalidPartOrder")
		}
		last = v.Number
		v.ETag = strings.Trim(v.ETag, "\"")
		if !multipartETag.MatchString(v.ETag) {
			return "", multipartFault(400, "InvalidPart")
		}
	}
	b, _ := json.Marshal(record{"parts": parts, "match": match, "none": none})
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}
func (s *server) completeMultipart(ctx context.Context, conn *pgx.Conn, p objectStoragePayload, principal, bucket, key, id string, selection []multipartSelection, match, none string) (multipartUpload, error) {
	var u multipartUpload
	hash, e := multipartManifestHash(selection, match, none)
	if e != nil {
		return u, e
	}
	tx, e := multipartTx(ctx, conn)
	if e != nil {
		return u, e
	}
	defer tx.Rollback(ctx)
	u, e = loadMultipart(ctx, tx, p, principal, bucket, key, id)
	if e != nil {
		return u, e
	}
	if u.State == "completed" {
		if u.completion != hash {
			return u, multipartFault(409, "CompletionConflict")
		}
		return u, tx.Commit(ctx)
	}
	if u.State != "uploading" {
		return u, multipartFault(404, "NoSuchUpload")
	}
	parts, e := listMultipartParts(ctx, tx, id)
	if e != nil {
		return u, e
	}
	indexed := map[int]multipartPart{}
	for _, v := range parts {
		indexed[v.Number] = v
	}
	var size int64
	layout := make([]storageObjectPart, 0, len(selection))
	for i, v := range selection {
		part, found := indexed[v.Number]
		if !found || part.ETag != v.ETag {
			return u, multipartFault(400, "InvalidPart")
		}
		if i < len(selection)-1 && part.Size < multipartMinimumPart {
			return u, multipartFault(400, "EntityTooSmall")
		}
		size += part.Size
		layout = append(layout, storageObjectPart{part.Number, part.Size, part.SHA256})
	}
	if size > storageObjectLimit {
		return u, multipartFault(413, "EntityTooLarge")
	}
	if u.ExpectedSize >= 0 && u.ExpectedSize != size {
		return u, multipartFault(400, "BadDigest")
	}
	var old string
	var oldSize int64
	e = tx.QueryRow(ctx, `SELECT sha256,size FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, u.Key).Scan(&old, &oldSize)
	if e != nil && !isNoRows(e) {
		return u, e
	}
	if match != "" && (old == "" || match != "\""+old+"\"") || none != "" && (none != "*" || old != "") {
		return u, multipartFault(412, "PreconditionFailed")
	}
	var count int
	var total int64
	if e = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(size),0)::bigint FROM neon_storage.objects`).Scan(&count, &total); e != nil {
		return u, e
	}
	if old == "" && count >= 1000 || total-oldSize+size > storageBranchLimit {
		return u, multipartFault(429, "QuotaExceeded")
	}
	content, e := buildStoragePayload(ctx, size, func(dst io.Writer) error {
		for _, v := range selection {
			part := indexed[v.Number]
			if !strings.HasPrefix(part.blob, p.ProjectID+"/") {
				return multipartFault(409, "ObjectIdentityMismatch")
			}
			data, err := s.storage.read(ctx, part.blob, part.SHA256, part.Size)
			if err != nil {
				return err
			}
			if _, err = dst.Write(data); err != nil {
				return err
			}
		}
		return nil
	})
	if e != nil {
		return u, e
	}
	defer content.Close()
	u.SHA256, u.Size = content.SHA256, content.Size
	if u.Size != size || u.ExpectedSHA256 != "" && u.ExpectedSHA256 != u.SHA256 {
		return u, multipartFault(400, "BadDigest")
	}
	settings, err := normalizeStorageObjectSettings(storageObjectSettings{Tags: u.Tags, ResponseHeaders: u.ResponseHeaders})
	if err != nil {
		return u, multipartFault(409, "InvalidUploadSettings")
	}
	u.Tags, u.ResponseHeaders = settings.Tags, settings.ResponseHeaders
	blob := p.ProjectID + "/" + newID("blob_")
	reader, e := content.Reader()
	if e != nil {
		return u, e
	}
	if e = s.storage.putReader(ctx, blob, u.ContentType, reader, u.Size); e != nil {
		return u, e
	}

	_, e = tx.Exec(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type,metadata,tags,response_headers,part_layout,checksums) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(bucket,key) DO UPDATE SET blob_key=$3,sha256=$4,size=$5,content_type=$6,metadata=$7,tags=$8,response_headers=$9,part_layout=$10,checksums=$11,updated_at=now()`, bucket, u.Key, blob, u.SHA256, size, u.ContentType, u.Metadata, u.Tags, u.ResponseHeaders, layout, content.storedChecksums())
	if e != nil {
		return u, e
	}
	if e = s.publishStorageFunctionEvent(ctx, tx, p, bucket, u.Key, u.SHA256, size); e != nil {
		return u, e
	}
	_, e = tx.Exec(ctx, `UPDATE neon_storage.multipart_uploads SET state='completed',completion_hash=$2,result_sha256=$3,result_size=$4,updated_at=now() WHERE id=$1`, id, hash, u.SHA256, size)
	if e != nil {
		return u, e
	}
	u.State = "completed"
	return u, tx.Commit(ctx)
}
func abortMultipart(ctx context.Context, conn *pgx.Conn, p objectStoragePayload, principal, bucket, key, id string) error {
	tx, e := multipartTx(ctx, conn)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	u, e := loadMultipart(ctx, tx, p, principal, bucket, key, id)
	if e != nil {
		return e
	}
	if u.State == "completed" {
		return multipartFault(404, "NoSuchUpload")
	}
	// This closes the logical upload. Immutable part bytes remain retained for
	// separately qualified reference-safe GC; never pretend abort erased them.
	_, e = tx.Exec(ctx, `UPDATE neon_storage.multipart_uploads SET state='aborted',updated_at=now() WHERE id=$1`, id)
	if e == nil {
		e = tx.Commit(ctx)
	}
	return e
}
func multipartError(w http.ResponseWriter, r *http.Request, e error, s3 bool) {
	var fault *storageMutationFault
	status, code := 503, "ServiceUnavailable"
	if errors.As(e, &fault) {
		status, code = fault.Status, fault.Code
	}
	if s3 {
		s3Error(w, r, status, code)
	} else {
		fail(w, r, status, "multipart_"+strings.ToLower(code), code)
	}
}
