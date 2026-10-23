package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Upload capabilities are domain-separated from read tokens. They authorize
// only creation of exactly these bytes, never overwrite, delete or COPY.
var storageSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type storageUpload struct {
	Branch      string `json:"branch"`
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Actor       string `json:"actor"`
	Generation  int64  `json:"generation"`
	Expires     int64  `json:"expires"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

func (s *server) signStorageUpload(v storageUpload) string {
	raw, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.idempotencyKey)
	mac.Write([]byte("neon-object-upload-v1:" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *server) decodeStorageUpload(token string) (storageUpload, bool) {
	var v storageUpload
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(token) > 4096 || len(s.idempotencyKey) < 32 {
		return v, false
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return v, false
	}
	mac := hmac.New(sha256.New, s.idempotencyKey)
	mac.Write([]byte("neon-object-upload-v1:" + parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return v, false
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil || json.Unmarshal(raw, &v) != nil || v.Expires <= time.Now().Unix() || v.Expires > time.Now().Add(15*time.Minute).Unix() || !validStorageKey(v.Key) || !storageBucketName.MatchString(v.Bucket) || !dataBranchID.MatchString(v.Branch) || v.Actor == "" || v.Generation < 1 || !storageSHA256.MatchString(v.SHA256) || v.Size < 0 || v.Size > storageObjectLimit {
		return v, false
	}
	_, _, e = mime.ParseMediaType(v.ContentType)
	return v, e == nil && len(v.ContentType) <= 256
}
func (s *server) storagePresignUpload(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Key         string `json:"key"`
		Expires     int64  `json:"expires_seconds"`
		SHA256      string `json:"sha256"`
		Size        int64  `json:"size"`
		ContentType string `json:"content_type"`
	}
	if readJSON(r, &input) != nil || !validStorageKey(input.Key) || input.Expires < 1 || input.Expires > 900 || !storageSHA256.MatchString(input.SHA256) || input.Size < 0 || input.Size > storageObjectLimit || len(input.ContentType) > 256 {
		fail(w, r, 422, "invalid_presign", "Valid key, SHA-256, size, Content-Type and expiry 1 to 900 required")
		return
	}
	if _, _, e := mime.ParseMediaType(input.ContentType); e != nil {
		fail(w, r, 422, "invalid_content_type", "Valid Content-Type required")
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
	bucket := r.PathValue("bucket")
	var exists, occupied bool
	if e = conn.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1),EXISTS(SELECT 1 FROM neon_storage.objects WHERE bucket=$1 AND key=$2)`, bucket, input.Key).Scan(&exists, &occupied); e != nil {
		s.storageFailure(w, r, e)
		return
	}
	if !exists {
		fail(w, r, 404, "not_found", "Bucket not found")
		return
	}
	if occupied {
		fail(w, r, 412, "object_version_conflict", "Object already exists")
		return
	}
	v := storageUpload{p.BranchID, bucket, input.Key, userFrom(r).ID, p.Generation, time.Now().Unix() + input.Expires, input.SHA256, input.Size, input.ContentType}
	q := url.Values{"key": {v.Key}, "token": {s.signStorageUpload(v)}}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 200, record{"url": "/storage/v1/" + p.BranchID + "/" + url.PathEscape(bucket) + "?" + q.Encode(), "method": "PUT", "expires_at": time.Unix(v.Expires, 0).UTC(), "headers": record{"Content-Type": v.ContentType, "If-None-Match": "*"}, "protocol": "neon-object-rest-v1", "sha256": v.SHA256, "size": v.Size})
}
func (s *server) storagePublicUpload(w http.ResponseWriter, r *http.Request) {
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") || strings.EqualFold(name, "X-Neon-Object-Metadata") || strings.EqualFold(name, "X-Amz-Tagging") {
			fail(w, r, 403, "invalid_upload_capability", "Upload capability does not authorize metadata")
			return
		}
	}
	v, ok := s.decodeStorageUpload(r.URL.Query().Get("token"))
	if !ok || v.Branch != r.PathValue("storageBranch") || v.Bucket != r.PathValue("bucket") || v.Key != r.URL.Query().Get("key") || r.Header.Get("Content-Type") != v.ContentType || r.Header.Get("If-None-Match") != "*" {
		fail(w, r, 403, "invalid_upload_capability", "Upload capability rejected")
		return
	}
	// Invalid or changed bytes are rejected before opening/waking a compute.
	data, e := readStoragePayload(r.Context(), r.Body, storageObjectLimit)
	if e != nil {
		s.storagePayloadFailure(w, r, e, false)
		return
	}
	defer data.Close()
	if data.Size != v.Size || data.SHA256 != v.SHA256 {
		fail(w, r, 400, "upload_digest_mismatch", "Upload bytes differ from the authorized file")
		return
	}
	if e = validateStoragePayloadChecksums(r.Header, data); e != nil {
		s.storagePayloadFailure(w, r, e, false)
		return
	}
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, p, close, e := s.openObjectStorage(r, "", v.Branch)
	if e != nil {
		s.storageFailure(w, r, e)
		return
	}
	defer close()
	var active bool
	if p.Generation != v.Generation || p.metadata == nil {
		fail(w, r, 403, "invalid_upload_capability", "Storage generation changed")
		return
	}
	e = p.metadata.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users u JOIN organization_members m ON m.user_id=u.id JOIN projects p ON p.org_id=m.org_id JOIN organizations o ON o.id=m.org_id AND o.state='active' LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id WHERE u.id=$1 AND NOT u.disabled AND p.id=$2 AND (m.role IN ('owner','admin','editor','developer') OR g.role IN ('admin','editor')))`, v.Actor, p.ProjectID).Scan(&active)
	if e != nil || !active {
		fail(w, r, 403, "upload_permission_revoked", "Upload permission is no longer active")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), userKey, user{ID: v.Actor}))
	r = r.Clone(r.Context())
	r.Header.Del("Content-Disposition")
	r.Header.Del("Cache-Control")
	if s.s3WritePayload(w, r, conn, p, v.Bucket, v.Key, data) {
		close()
		s.audit(r.Context(), v.Actor, "presigned_put_object", "object_storage", v.Branch, "succeeded", requestID(r))
	}
}
