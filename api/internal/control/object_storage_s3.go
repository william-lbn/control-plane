package control

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/s3protocol"
)

const s3XMLNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

func s3Origin() string { return os.Getenv("NEON_S3_PUBLIC_ORIGIN") }
func s3Region() string { return env("NEON_S3_REGION", "us-east-2") }
func s3Error(w http.ResponseWriter, r *http.Request, status int, code string) {
	s3XML(w, status, struct {
		XMLName   xml.Name `xml:"Error"`
		Code      string   `xml:"Code"`
		RequestID string   `xml:"RequestId"`
	}{Code: code, RequestID: requestID(r)})
}
func s3XML(w http.ResponseWriter, status int, value any) {
	data, e := xml.Marshal(value)
	if e != nil {
		http.Error(w, "S3 response unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(append([]byte(xml.Header), data...))
}
func (s *server) s3Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newID("s3r_")
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		w.Header().Set("X-Amz-Request-Id", id)
		origin, e := url.Parse(s3Origin())
		if e != nil || origin.Host == "" || !strings.EqualFold(r.Host, origin.Host) {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		if r.Method == http.MethodOptions {
			s.s3Preflight(w, r)
			return
		}
		s.s3Invoke(w, r)
	})
}
func (s *server) s3Invoke(w http.ResponseWriter, r *http.Request) {
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	if rejectUnsupportedS3Headers(w, r) {
		return
	}
	if s.s3BrowserPost(w, r) {
		return
	}
	if s.s3AnonymousRead(w, r) {
		return
	}
	signature, e := s3protocol.Parse(r, s3Region(), time.Now())
	if e != nil {
		s3Error(w, r, 403, "SignatureDoesNotMatch")
		return
	}
	token, anchor, e := s.loadStorageSigning(r.Context(), signature.KeyID)
	if e != nil {
		s3Error(w, r, 403, "InvalidAccessKeyId")
		return
	}
	var data []byte
	var payload *storagePayload
	if s3StreamingObjectPUT(r) {
		payload, e = buildStoragePayload(r.Context(), storageObjectAdmissionLimit(), func(dst io.Writer) error {
			return signature.VerifyAndDecodeTo(r, r.Body, dst, storageSigningSecret(token), s3Region(), storageObjectAdmissionLimit())
		})
		if e != nil {
			if errors.Is(e, s3protocol.ErrSignature) {
				s3Error(w, r, 403, "SignatureDoesNotMatch")
			} else {
				s.storagePayloadFailure(w, r, e, true)
			}
			return
		}
		defer payload.Close()
	} else {
		wire, err := io.ReadAll(io.LimitReader(r.Body, storagePartLimit+(1<<20)+1))
		if err != nil || len(wire) > storagePartLimit+(1<<20) {
			s3Error(w, r, 413, "EntityTooLarge")
			return
		}
		data, e = signature.VerifyAndDecode(r, wire, storageSigningSecret(token), s3Region(), storagePartLimit)
		if e != nil {
			s3Error(w, r, 403, "SignatureDoesNotMatch")
			return
		}
	}

	// The decoder has authenticated the seed and the complete entity. Promote
	// only its verified checksum for the existing payload/part admission layer.
	marker := r.Header.Get("X-Amz-Content-Sha256")
	if marker == s3protocol.SignedTrailerPayload || marker == s3protocol.UnsignedTrailerPayload {
		name := r.Header.Get("X-Amz-Trailer")
		r.Header.Set(name, r.Trailer.Get(name))
	}
	target, objectPath := anchor, strings.TrimPrefix(r.URL.Path, "/")
	if strings.HasPrefix(objectPath, "s3/") {
		pieces := strings.SplitN(strings.TrimPrefix(objectPath, "s3/"), "/", 2)
		if !dataBranchID.MatchString(pieces[0]) {
			s3Error(w, r, 400, "InvalidURI")
			return
		}
		target = pieces[0]
		objectPath = ""
		if len(pieces) == 2 {
			objectPath = pieces[1]
		}
	}
	scope := "storage:read"
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		scope = "storage:write"
	}
	if _, e = s.verifyBackendCredential(r.Context(), token, target, scope, ""); e != nil {
		s3Error(w, r, 403, "AccessDenied")
		return
	}
	if r.Header.Get("X-Amz-Copy-Source") != "" {
		if r.Method != http.MethodPut {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		if _, e = s.verifyBackendCredential(r.Context(), token, target, "storage:read", ""); e != nil {
			s3Error(w, r, 403, "AccessDenied")
			return
		}
	}
	query, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	// Reject unsupported subresources before opening SQL or changing an object.
	if !s3QuerySupported(query) {
		s3Error(w, r, 501, "NotImplemented")
		return
	}
	if payload != nil {
		if err := validateStoragePayloadChecksums(r.Header, payload); err != nil {
			s.storagePayloadFailure(w, r, err, true)
			return
		}
	}
	conn, p, close, e := s.openObjectStorage(r, "", target)
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	defer func() {
		// Release admission before a separate usage connection: no full-pool wait.
		close()
		usageCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.db.Exec(usageCtx, `UPDATE backend_credentials SET last_used_at=now() WHERE id=$1`, signature.KeyID)
	}()
	if owner := r.Header.Get("X-Amz-Expected-Bucket-Owner"); owner != "" && owner != p.BranchID {
		s3Error(w, r, 403, "AccessDenied")
		return
	}
	r.SetPathValue("project", p.ProjectID)
	r.SetPathValue("branch", target)
	if objectPath == "" {
		if r.Method != http.MethodGet {
			s3Error(w, r, 405, "MethodNotAllowed")
			return
		}
		s.s3ListBuckets(w, r, conn)
		return
	}
	pieces := strings.SplitN(objectPath, "/", 2)
	bucket, key := pieces[0], ""
	if len(pieces) == 2 {
		key = pieces[1]
	}
	if !storageBucketName.MatchString(bucket) {
		s3Error(w, r, 400, "InvalidBucketName")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		rules, err := loadStorageCORS(r, conn, bucket)
		if err == nil {
			if rule := matchStorageCORS(rules, origin, r.Method, nil); rule != nil {
				applyStorageCORS(w, rule, origin, nil, false)
			}
		}
	}
	if key != "" && !validStorageKey(key) {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	if query.Has("versionId") && query.Get("versionId") != "null" {
		s3Error(w, r, 404, "NoSuchVersion")
		return
	}
	if s.s3Settings(w, r, conn, p, bucket, key, data, query) {
		return
	}
	if query.Has("uploads") || query.Has("uploadId") || query.Has("partNumber") {
		s.s3Multipart(w, r, conn, p, "credential:"+signature.KeyID, bucket, key, data, query)
		return
	}
	if query.Has("cors") {
		if key != "" {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		s.s3CORSConfiguration(w, r, conn, bucket, data)
		return
	}
	if query.Has("delete") {
		if key != "" || r.Method != http.MethodPost {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		s.s3DeleteObjects(w, r, conn, bucket, data)
		return
	}
	if key == "" {
		s.s3Bucket(w, r, conn, p, bucket, data, query)
		return
	}
	if !validStorageKey(key) {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.serveS3Object(w, r, conn, p, bucket, key)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		s3Error(w, r, 501, "NotImplemented")
		return
	}
	if source := r.Header.Get("X-Amz-Copy-Source"); source != "" {
		var replacements []storageCopyMetadata
		var replacement storageCopyMetadata
		if r.Header.Get("X-Amz-Metadata-Directive") == "REPLACE" {
			metadata, e := metadataFromHeaders(r, true)
			if e != nil {
				s3Error(w, r, 400, "InvalidArgument")
				return
			}
			contentType := r.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			headers, err := storageResponseHeaders(r)
			if err != nil {
				s3Error(w, r, 400, "InvalidArgument")
				return
			}
			replacement = storageCopyMetadata{Metadata: metadata, ContentType: contentType, Headers: headers}
			replacements = []storageCopyMetadata{replacement}
		} else {
			for header := range r.Header {
				if strings.HasPrefix(strings.ToLower(header), "x-amz-meta-") {
					s3Error(w, r, 400, "InvalidArgument")
					return
				}
			}
		}
		for _, header := range []string{"X-Amz-Acl", "X-Amz-Storage-Class"} {
			if r.Header.Get(header) != "" {
				s3Error(w, r, 501, "NotImplemented")
				return
			}
		}

		directive := r.Header.Get("X-Amz-Tagging-Directive")
		if directive != "" && directive != "COPY" && directive != "REPLACE" {
			s3Error(w, r, 400, "InvalidArgument")
			return
		}
		if directive == "REPLACE" {
			tags, err := storageTagsHeader(r)
			if err != nil {
				s3Error(w, r, 400, "InvalidTag")
				return
			}
			if len(replacements) == 0 {
				replacement = storageCopyMetadata{OnlyTags: true}
			}
			replacement.Tags = &tags
			replacements = []storageCopyMetadata{replacement}
		} else if r.Header.Get("X-Amz-Tagging") != "" {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		if len(data) != 0 || r.Header.Get("X-Amz-Server-Side-Encryption") != "" || (r.Header.Get("X-Amz-Metadata-Directive") != "" && r.Header.Get("X-Amz-Metadata-Directive") != "COPY" && r.Header.Get("X-Amz-Metadata-Directive") != "REPLACE") {
			s3Error(w, r, 501, "NotImplemented")
			return
		}
		srcBucket, srcKey, err := parseS3CopySource(source)
		if err != nil {
			s3Error(w, r, 400, "InvalidArgument")
			return
		}
		conditions, err := parseStorageCopyConditions(r.Header)
		if err != nil {
			s3Error(w, r, 400, "InvalidArgument")
			return
		}
		item, err := copyStorageObjectWithConditions(s.storagePublicationContext(r.Context(), p), conn, p.ProjectID, srcBucket, srcKey, bucket, key, conditions, r.Header.Get("If-Match"), r.Header.Get("If-None-Match"), replacements...)
		if err != nil {
			var fault *storageMutationFault
			if errors.As(err, &fault) {
				s3Error(w, r, fault.Status, fault.Code)
			} else {
				s3Error(w, r, 503, "ServiceUnavailable")
			}
			return
		}
		s3XML(w, 200, struct {
			XMLName  xml.Name `xml:"CopyObjectResult"`
			XMLNS    string   `xml:"xmlns,attr"`
			ETag     string   `xml:"ETag"`
			Modified string   `xml:"LastModified"`
		}{XMLNS: s3XMLNamespace, ETag: "\"" + stringVal(item["sha256"]) + "\"", Modified: item["updated_at"].(time.Time).Format(time.RFC3339)})
		return
	}
	if payload != nil {
		s.s3WritePayload(w, r, conn, p, bucket, key, payload)
		return
	}
	if !s.s3WriteObject(w, r, conn, p, bucket, key, data) {
		return
	}
}
func (s *server) serveS3Object(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket, key string) {
	var blob, digest, contentType string
	var metadata, responseHeaders, checksums map[string]string
	var tags []storageTag
	var size int64
	var modified time.Time
	e := conn.QueryRow(r.Context(), `SELECT blob_key,sha256,size,content_type,updated_at,metadata,response_headers,tags,checksums FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, key).Scan(&blob, &digest, &size, &contentType, &modified, &metadata, &responseHeaders, &tags, &checksums)
	if isNoRows(e) {
		s3Error(w, r, 404, "NoSuchKey")
		return
	}
	if e != nil || !strings.HasPrefix(blob, p.ProjectID+"/") {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	data, e := s.storage.readPayload(r.Context(), blob, digest, size)
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	defer data.Close()
	source, e := data.Reader()
	if e != nil {
		s.storagePayloadFailure(w, r, e, true)
		return
	}
	declared, e := storageStoredChecksums(digest, checksums)
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	for name, value := range declared {
		if data.checksums[name] != value {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
	}
	if r.Header.Get("X-Amz-Checksum-Mode") == "ENABLED" && r.Header.Get("Range") == "" {
		writeStoredStorageChecksums(w, declared)
	}
	w.Header().Set("ETag", "\""+digest+"\"")
	w.Header().Set("X-Amz-Version-Id", "null")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Neon-Metadata-ETag", storageMetadataState(p.BranchID, bucket, key, blob, digest, contentType, metadata))
	writeStorageMetadata(w, metadata)
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": key[strings.LastIndex(key, "/")+1:]}))
	w.Header().Set("Cache-Control", "private, no-store")
	if writeStorageResponseHeaders(w, responseHeaders) != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	w.Header().Set("X-Amz-Tagging-Count", strconv.Itoa(len(tags)))
	http.ServeContent(w, r, "", modified, source)
}
func (s *server) s3ListBuckets(w http.ResponseWriter, r *http.Request, conn *pgx.Conn) {
	rows, e := conn.Query(r.Context(), `SELECT name,created_at FROM neon_storage.buckets ORDER BY name`)
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	defer rows.Close()
	type bucket struct {
		Name    string `xml:"Name"`
		Created string `xml:"CreationDate"`
	}
	out := struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		XMLNS   string   `xml:"xmlns,attr"`
		Buckets []bucket `xml:"Buckets>Bucket"`
	}{XMLNS: s3XMLNamespace, Buckets: []bucket{}}
	for rows.Next() {
		var b bucket
		var t time.Time
		if rows.Scan(&b.Name, &t) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		b.Created = t.UTC().Format(time.RFC3339)
		out.Buckets = append(out.Buckets, b)
	}
	if rows.Err() != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	s3XML(w, 200, out)
}
func (s *server) s3Bucket(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket string, data []byte, q url.Values) {
	if rejectUnsupportedS3Headers(w, r) {
		return
	}
	if r.Method == http.MethodPut {
		if len(data) > 4096 {
			s3Error(w, r, 400, "InvalidArgument")
			return
		}
		if len(data) > 0 {
			var c struct {
				XMLName  xml.Name `xml:"CreateBucketConfiguration"`
				Location string   `xml:"LocationConstraint"`
			}
			if xml.Unmarshal(data, &c) != nil || c.Location != s3Region() {
				s3Error(w, r, 400, "InvalidLocationConstraint")
				return
			}
		}
		tx, e := conn.Begin(r.Context())
		if e != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		defer tx.Rollback(r.Context())
		if storageDirectoryLock(r.Context(), tx) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		var count int
		var exists bool
		if tx.QueryRow(r.Context(), `SELECT count(*),EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1) FROM neon_storage.buckets`, bucket).Scan(&count, &exists) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		if exists {
			s3Error(w, r, 409, "BucketAlreadyOwnedByYou")
			return
		}
		if count >= 32 {
			s3Error(w, r, 429, "TooManyBuckets")
			return
		}
		if _, e = tx.Exec(r.Context(), `INSERT INTO neon_storage.buckets(name,access) VALUES($1,'private')`, bucket); e == nil {
			e = tx.Commit(r.Context())
		}
		if e != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(200)
		return
	}
	var exists bool
	if conn.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, bucket).Scan(&exists) != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	if !exists {
		s3Error(w, r, 404, "NoSuchBucket")
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(200)
		return
	}
	if r.Method == http.MethodDelete {
		tx, e := conn.Begin(r.Context())
		if e != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		defer tx.Rollback(r.Context())
		if storageDirectoryLock(r.Context(), tx) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		var occupied bool
		if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.objects WHERE bucket=$1) OR EXISTS(SELECT 1 FROM neon_storage.multipart_uploads WHERE bucket=$1 AND branch_id=$2 AND state='uploading')`, bucket, p.BranchID).Scan(&occupied) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		if occupied {
			s3Error(w, r, 409, "BucketNotEmpty")
			return
		}
		if _, e = tx.Exec(r.Context(), `DELETE FROM neon_storage.buckets WHERE name=$1`, bucket); e == nil {
			e = tx.Commit(r.Context())
		}
		if e != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.Method != http.MethodGet {
		s3Error(w, r, 501, "NotImplemented")
		return
	}
	if q.Has("location") {
		s3XML(w, 200, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			XMLNS   string   `xml:"xmlns,attr"`
			Value   string   `xml:",chardata"`
		}{XMLNS: s3XMLNamespace, Value: s3Region()})
		return
	}
	s.s3ListObjects(w, r, conn, p, bucket, q)
}
func (s *server) s3WriteObject(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket, key string, data []byte) bool {
	payload, err := smallStoragePayload(data)
	if err != nil {
		s.storagePayloadFailure(w, r, err, true)
		return false
	}
	defer payload.Close()
	return s.s3WritePayload(w, r, conn, p, bucket, key, payload)
}
func (s *server) s3WritePayload(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket, key string, data *storagePayload) bool {
	if rejectUnsupportedS3Headers(w, r) {
		return false
	}
	if r.Header.Get("X-Amz-Copy-Source") != "" || r.Header.Get("X-Amz-Server-Side-Encryption") != "" {
		s3Error(w, r, 501, "NotImplemented")
		return false
	}
	if err := validateStoragePayloadChecksums(r.Header, data); err != nil {
		s.storagePayloadFailure(w, r, err, true)
		return false
	}

	tags, parseErr := storageTagsHeader(r)
	if parseErr != nil {
		s3Error(w, r, 400, "InvalidTag")
		return false
	}
	responseHeaders, parseErr := storageResponseHeaders(r)
	if parseErr != nil {
		s3Error(w, r, 400, "InvalidArgument")
		return false
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-object-lock-") {
			s3Error(w, r, 501, "NotImplemented")
			return false
		}
	}
	if acl := r.Header.Get("X-Amz-Acl"); acl != "" && acl != "private" {
		s3Error(w, r, 501, "NotImplemented")
		return false
	}
	if class := r.Header.Get("X-Amz-Storage-Class"); class != "" && class != "STANDARD" {
		s3Error(w, r, 501, "NotImplemented")
		return false
	}

	tx, e := conn.Begin(r.Context())
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return false
	}
	defer tx.Rollback(r.Context())
	if storageDirectoryLock(r.Context(), tx) != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return false
	}
	var exists bool
	if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, bucket).Scan(&exists) != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return false
	}
	if !exists {
		s3Error(w, r, 404, "NoSuchBucket")
		return false
	}
	var previous string
	var oldSize int64
	e = tx.QueryRow(r.Context(), `SELECT sha256,size FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, key).Scan(&previous, &oldSize)
	if e != nil && !isNoRows(e) {
		s3Error(w, r, 503, "ServiceUnavailable")
		return false
	}
	if v := r.Header.Get("If-Match"); v != "" && v != "\""+previous+"\"" {
		s3Error(w, r, 412, "PreconditionFailed")
		return false
	}
	if v := r.Header.Get("If-None-Match"); v != "" && (v != "*" || previous != "") {
		s3Error(w, r, 412, "PreconditionFailed")
		return false
	}
	if r.Method == http.MethodDelete {
		_, e = tx.Exec(r.Context(), `DELETE FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, key)
	} else {
		contentType := r.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if len(contentType) > 256 {
			s3Error(w, r, 400, "InvalidArgument")
			return false
		}
		if _, _, e = mime.ParseMediaType(contentType); e != nil {
			s3Error(w, r, 400, "InvalidArgument")
			return false
		}
		var count int
		var total int64
		if tx.QueryRow(r.Context(), `SELECT count(*),COALESCE(sum(size),0)::bigint FROM neon_storage.objects`).Scan(&count, &total) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return false
		}
		if previous == "" && count >= 1000 || total-oldSize+data.Size > storageBranchAdmissionLimit() {
			s3Error(w, r, 429, "QuotaExceeded")
			return false
		}
		metadata, metadataErr := metadataFromHeaders(r, true)
		if metadataErr != nil {
			s3Error(w, r, 400, "InvalidArgument")
			return false
		}
		digest := data.SHA256
		blob := p.ProjectID + "/" + newID("blob_")
		source, err := data.Reader()
		if err != nil {
			s.storagePayloadFailure(w, r, err, true)
			return false
		}
		if s.storage.putReader(r.Context(), blob, contentType, source, data.Size) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return false
		}
		_, e = tx.Exec(r.Context(), `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type,metadata,tags,response_headers,checksums) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(bucket,key) DO UPDATE SET blob_key=$3,sha256=$4,size=$5,content_type=$6,metadata=$7,tags=$8,response_headers=$9,checksums=$10,part_layout='[]'::jsonb,updated_at=now()`, bucket, key, blob, digest, data.Size, contentType, metadata, tags, responseHeaders, data.storedChecksums())
		if e == nil {
			e = s.publishStorageFunctionEvent(r.Context(), tx, p, bucket, key, digest, data.Size)
		}
		w.Header().Set("ETag", "\""+digest+"\"")
		w.Header().Set("X-Amz-Version-Id", "null")
		writeStoragePayloadChecksums(w, r, data)
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return false
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(200)
	}
	return true
}

func s3QuerySupported(q url.Values) bool {
	if q.Has("withUpdatedAt") && (!q.Has("lifecycle") || q.Get("withUpdatedAt") != "true") {
		return false
	}
	for _, name := range []string{"part-number-marker", "max-parts", "key-marker", "upload-id-marker", "max-uploads"} {
		if q.Has(name) && !q.Has("uploads") && !q.Has("uploadId") {
			return false
		}
	}
	for name := range q {
		if strings.HasPrefix(name, "X-Amz-") {
			continue
		}
		switch name {
		case "uploads", "uploadId", "partNumber", "part-number-marker", "max-parts", "key-marker", "upload-id-marker", "max-uploads":
		case "acl", "policy", "tagging", "versioning", "lifecycle", "object-lock", "publicAccessBlock", "attributes", "versionId", "x-id", "withUpdatedAt":
		case "cors", "delete", "list-type", "prefix", "marker", "max-keys", "continuation-token", "start-after", "delimiter", "encoding-type", "location", "fetch-owner":
		default:
			return false
		}
	}
	_, valid := s3SettingResource(q)
	return valid
}

type s3ObjectOwner struct {
	ID   string `xml:"ID"`
	Name string `xml:"DisplayName"`
}
type s3ListedObject struct {
	Key          string         `xml:"Key"`
	Grouped      bool           `xml:"-"`
	Modified     string         `xml:"LastModified"`
	ETag         string         `xml:"ETag"`
	Size         int64          `xml:"Size"`
	StorageClass string         `xml:"StorageClass"`
	Owner        *s3ObjectOwner `xml:"Owner,omitempty"`
}

func (s *server) s3ListObjects(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket string, q url.Values) {
	if q.Has("fetch-owner") && q.Get("fetch-owner") != "true" && q.Get("fetch-owner") != "false" {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	if (q.Get("encoding-type") != "" && q.Get("encoding-type") != "url") || (q.Get("list-type") != "" && q.Get("list-type") != "2") {
		s3Error(w, r, 501, "NotImplemented")
		return
	}
	prefix, after, delimiter := q.Get("prefix"), q.Get("marker"), q.Get("delimiter")
	if !utf8.ValidString(prefix) || len(prefix) > 1024 || !utf8.ValidString(delimiter) || len(delimiter) > 1024 {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	if q.Get("start-after") != "" {
		after = q.Get("start-after")
	}
	if q.Get("continuation-token") != "" {
		b, e := base64.RawURLEncoding.DecodeString(q.Get("continuation-token"))
		if e != nil || len(b) > 1024 {
			s3Error(w, r, 400, "InvalidToken")
			return
		}
		after = string(b)
	}
	if !utf8.ValidString(after) || len(after) > 1024 {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	limit := 1000
	if q.Has("max-keys") {
		n, e := strconv.Atoi(q.Get("max-keys"))
		if e != nil || n < 0 || n > 1000 {
			s3Error(w, r, 400, "InvalidArgument")
			return
		}
		limit = n
	}
	// Group before applying cursor and max-keys. A common prefix counts once;
	// filtering raw keys first would repeat a directory on every page or return
	// a prefix preceding StartAfter. SQL character lengths preserve UTF-8.
	rows, e := conn.Query(r.Context(), storageListingSQL, bucket, prefix, after, delimiter, limit+1)
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	defer rows.Close()
	items := []s3ListedObject{}
	for rows.Next() {
		var o s3ListedObject
		var modified time.Time
		var etag, contentType string
		if rows.Scan(&o.Key, &o.Grouped, &etag, &o.Size, &contentType, &modified) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		o.Modified = modified.UTC().Format(time.RFC3339)
		o.ETag = "\"" + etag + "\""
		o.StorageClass = "STANDARD"
		if q.Get("list-type") == "" || q.Get("fetch-owner") == "true" {
			// All private objects belong to the authorized branch catalogue.
			// Never expose Console/application user identities as S3 owners.
			o.Owner = &s3ObjectOwner{ID: p.BranchID, Name: p.BranchID}
		}
		items = append(items, o)
	}
	if rows.Err() != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	truncated := len(items) > limit
	next := ""
	if truncated && limit > 0 {
		items = items[:limit]
		next = items[len(items)-1].Key
	}
	if limit == 0 {
		items = []s3ListedObject{}
		truncated = false
	}
	type commonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	contents := []s3ListedObject{}
	prefixes := []commonPrefix{}
	for _, item := range items {
		if item.Grouped {
			prefixes = append(prefixes, commonPrefix{Prefix: item.Key})
		} else {
			contents = append(contents, item)
		}
	}
	out := struct {
		Delimiter      string           `xml:"Delimiter,omitempty"`
		CommonPrefixes []commonPrefix   `xml:"CommonPrefixes"`
		Continuation   string           `xml:"ContinuationToken,omitempty"`
		StartAfter     string           `xml:"StartAfter,omitempty"`
		Marker         string           `xml:"Marker,omitempty"`
		XMLName        xml.Name         `xml:"ListBucketResult"`
		XMLNS          string           `xml:"xmlns,attr"`
		Name           string           `xml:"Name"`
		Prefix         string           `xml:"Prefix"`
		Encoding       string           `xml:"EncodingType,omitempty"`
		MaxKeys        int              `xml:"MaxKeys"`
		KeyCount       int              `xml:"KeyCount"`
		Truncated      bool             `xml:"IsTruncated"`
		NextToken      string           `xml:"NextContinuationToken,omitempty"`
		NextMarker     string           `xml:"NextMarker,omitempty"`
		Contents       []s3ListedObject `xml:"Contents"`
	}{XMLNS: s3XMLNamespace, Name: bucket, Prefix: prefix, MaxKeys: limit, KeyCount: len(items), Truncated: truncated, Contents: contents, Delimiter: delimiter, CommonPrefixes: prefixes}
	if q.Get("encoding-type") == "url" {
		out.Encoding = "url"
		out.Prefix = s3protocol.Encode(prefix, false)
		out.Delimiter = s3protocol.Encode(delimiter, false)
		for i := range out.CommonPrefixes {
			out.CommonPrefixes[i].Prefix = s3protocol.Encode(out.CommonPrefixes[i].Prefix, false)
		}
		for i := range out.Contents {
			out.Contents[i].Key = s3protocol.Encode(out.Contents[i].Key, false)
		}
	}
	if q.Get("list-type") == "2" {
		out.NextToken = base64.RawURLEncoding.EncodeToString([]byte(next))
		out.Continuation = q.Get("continuation-token")
		out.StartAfter = q.Get("start-after")
		if out.Encoding == "url" {
			out.StartAfter = s3protocol.Encode(out.StartAfter, false)
		}
	} else {
		out.NextMarker = next
		out.Marker = q.Get("marker")
		if out.Encoding == "url" {
			out.NextMarker = s3protocol.Encode(out.NextMarker, false)
			out.Marker = s3protocol.Encode(out.Marker, false)
		}
	}
	s3XML(w, 200, out)
}
