package control

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
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
		s.s3Invoke(w, r)
	})
}
func (s *server) s3Invoke(w http.ResponseWriter, r *http.Request) {
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
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
	data, e := io.ReadAll(io.LimitReader(r.Body, storageObjectLimit+1))
	if e != nil || len(data) > storageObjectLimit {
		s3Error(w, r, 413, "EntityTooLarge")
		return
	}
	if signature.Verify(r, data, storageSigningSecret(token), s3Region()) != nil {
		s3Error(w, r, 403, "SignatureDoesNotMatch")
		return
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
	query, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	// Reject unsupported subresources before opening SQL or changing an object.
	for name := range query {
		if strings.HasPrefix(name, "X-Amz-") {
			continue
		}
		switch name {
		case "list-type", "prefix", "marker", "max-keys", "continuation-token", "start-after", "delimiter", "encoding-type", "location":
		default:
			s3Error(w, r, 501, "NotImplemented")
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
	if !s.s3WriteObject(w, r, conn, p, bucket, key, data) {
		return
	}
}
func (s *server) serveS3Object(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket, key string) {
	var blob, digest, contentType string
	var size int64
	var modified time.Time
	e := conn.QueryRow(r.Context(), `SELECT blob_key,sha256,size,content_type,updated_at FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, key).Scan(&blob, &digest, &size, &contentType, &modified)
	if isNoRows(e) {
		s3Error(w, r, 404, "NoSuchKey")
		return
	}
	if e != nil || !strings.HasPrefix(blob, p.ProjectID+"/") {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	data, e := s.storage.read(r.Context(), blob, digest, size)
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	w.Header().Set("ETag", "\""+digest+"\"")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": key[strings.LastIndex(key, "/")+1:]}))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", modified, bytes.NewReader(data))
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
		if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.objects WHERE bucket=$1)`, bucket).Scan(&occupied) != nil {
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
	if r.Header.Get("X-Amz-Copy-Source") != "" || r.Header.Get("X-Amz-Server-Side-Encryption") != "" {
		s3Error(w, r, 501, "NotImplemented")
		return false
	}
	if v := r.Header.Get("Content-MD5"); v != "" {
		sum := md5.Sum(data)
		if v != base64.StdEncoding.EncodeToString(sum[:]) {
			s3Error(w, r, 400, "BadDigest")
			return false
		}
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
		if previous == "" && count >= 1000 || total-oldSize+int64(len(data)) > storageBranchLimit {
			s3Error(w, r, 429, "QuotaExceeded")
			return false
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		blob := p.ProjectID + "/" + newID("blob_")
		if s.storage.put(r.Context(), blob, contentType, data) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return false
		}
		_, e = tx.Exec(r.Context(), `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(bucket,key) DO UPDATE SET blob_key=$3,sha256=$4,size=$5,content_type=$6,updated_at=now()`, bucket, key, blob, digest, len(data), contentType)
		w.Header().Set("ETag", "\""+digest+"\"")
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

type s3ListedObject struct {
	Key          string `xml:"Key"`
	Modified     string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

func (s *server) s3ListObjects(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket string, q url.Values) {
	if q.Get("delimiter") != "" || (q.Get("encoding-type") != "" && q.Get("encoding-type") != "url") || (q.Get("list-type") != "" && q.Get("list-type") != "2") {
		s3Error(w, r, 501, "NotImplemented")
		return
	}
	prefix, after := q.Get("prefix"), q.Get("marker")
	if !utf8.ValidString(prefix) || len(prefix) > 1024 {
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
	limit := 1000
	if q.Has("max-keys") {
		n, e := strconv.Atoi(q.Get("max-keys"))
		if e != nil || n < 0 || n > 1000 {
			s3Error(w, r, 400, "InvalidArgument")
			return
		}
		limit = n
	}
	rows, e := conn.Query(r.Context(), `SELECT key,sha256,size,updated_at FROM neon_storage.objects WHERE bucket=$1 AND starts_with(key,$2) AND key>$3 COLLATE "C" ORDER BY key COLLATE "C" LIMIT $4`, bucket, prefix, after, limit+1)
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	defer rows.Close()
	items := []s3ListedObject{}
	for rows.Next() {
		var o s3ListedObject
		var modified time.Time
		var etag string
		if rows.Scan(&o.Key, &etag, &o.Size, &modified) != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		o.Modified = modified.UTC().Format(time.RFC3339)
		o.ETag = "\"" + etag + "\""
		o.StorageClass = "STANDARD"
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
	out := struct {
		XMLName    xml.Name         `xml:"ListBucketResult"`
		XMLNS      string           `xml:"xmlns,attr"`
		Name       string           `xml:"Name"`
		Prefix     string           `xml:"Prefix"`
		Encoding   string           `xml:"EncodingType,omitempty"`
		MaxKeys    int              `xml:"MaxKeys"`
		KeyCount   int              `xml:"KeyCount"`
		Truncated  bool             `xml:"IsTruncated"`
		NextToken  string           `xml:"NextContinuationToken,omitempty"`
		NextMarker string           `xml:"NextMarker,omitempty"`
		Contents   []s3ListedObject `xml:"Contents"`
	}{XMLNS: s3XMLNamespace, Name: bucket, Prefix: prefix, MaxKeys: limit, KeyCount: len(items), Truncated: truncated, Contents: items}
	if q.Get("encoding-type") == "url" {
		out.Encoding = "url"
		out.Prefix = s3protocol.Encode(prefix, false)
		for i := range out.Contents {
			out.Contents[i].Key = s3protocol.Encode(out.Contents[i].Key, false)
		}
	}
	if q.Get("list-type") == "2" {
		out.NextToken = base64.RawURLEncoding.EncodeToString([]byte(next))
	} else {
		out.NextMarker = next
	}
	s3XML(w, 200, out)
}
