package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var objectStorageSlots = make(chan struct{}, 8)
var storageBucketName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`)

func validStorageKey(key string) bool {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.HasPrefix(key, "/") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	for _, c := range key {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func (s *server) objectStorageRead(w http.ResponseWriter, r *http.Request) {
	var exists bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL)`, r.PathValue("branch"), r.PathValue("project")).Scan(&exists); err != nil || !exists {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	item, err := s.one(r.Context(), `SELECT branch_id,endpoint_id,generation,state,spec,updated_at FROM object_storage_instances WHERE branch_id=$1 AND project_id=$2`, r.PathValue("branch"), r.PathValue("project"))
	if isNoRows(err) {
		item = record{"branch_id": r.PathValue("branch"), "generation": int64(0), "state": "disabled", "spec": nil}
	} else if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read Object Storage")
		return
	}
	item["driver_enabled"] = s.storage != nil
	item["protocol"] = "neon-object-rest-v1"
	item["s3_compatible"] = false
	item["limits"] = record{"object_bytes": storageObjectLimit, "branch_bytes": storageBranchLimit, "buckets": 32, "objects": 1000}
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, item["generation"].(int64)))
	jsonResponse(w, 200, item)
}
func (s *server) acceptObjectStorage(w http.ResponseWriter, r *http.Request, id string) {
	op, err := s.operationRecord(r.Context(), id, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load Operation")
		return
	}
	if op["resource_id"] != r.PathValue("branch") || op["resource_type"] != "object_storage" {
		fail(w, r, 409, "idempotency_conflict", "Key belongs to another resource")
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+r.PathValue("project")+"/operations/"+id)
	jsonResponse(w, 202, record{"branch_id": r.PathValue("branch"), "operation": op})
}
func (s *server) replayObjectStorage(w http.ResponseWriter, r *http.Request, key, hash string) bool {
	var previous, id string
	err := s.db.QueryRow(r.Context(), `SELECT request_hash,operation_id FROM idempotency_keys WHERE actor_id=$1 AND key_hash=$2`, userFrom(r).ID, keyHash(key)).Scan(&previous, &id)
	if isNoRows(err) {
		return false
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check idempotency")
		return true
	}
	if previous != hash {
		fail(w, r, 409, "idempotency_conflict", "Key used with different input")
		return true
	}
	s.acceptObjectStorage(w, r, id)
	return true
}
func (s *server) objectStorageMutate(w http.ResponseWriter, r *http.Request) {
	if s.storage == nil || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "object_storage_driver_disabled", "Object Storage native Driver not configured")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	if err != nil || version < 0 {
		fail(w, r, 428, "version_required", "If-Match with the numeric service generation is required")
		return
	}
	p := objectStoragePayload{ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch"), Generation: version + 1}
	action := "enable_object_storage"
	if r.Method == http.MethodDelete {
		action = "disable_object_storage"
	} else if readJSON(r, &p.Spec) != nil || !validPGIdentifier(p.Spec.Database) {
		fail(w, r, 422, "invalid_storage_config", "A valid database is required")
		return
	}
	hash := s.createRequestHash(action+":"+p.ProjectID+":"+p.BranchID, record{"generation": version, "spec": p.Spec})
	if s.replayObjectStorage(w, r, key, hash) {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not admit Operation")
		return
	}
	defer tx.Rollback(r.Context())
	var projectState string
	if err = tx.QueryRow(r.Context(), `SELECT state FROM projects WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, p.ProjectID).Scan(&projectState); err != nil || projectState != "ready" {
		fail(w, r, 409, "project_not_ready", "Ready project required")
		return
	}
	var branchState, state string
	var generation int64
	var raw []byte
	if err = tx.QueryRow(r.Context(), `SELECT state FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, p.BranchID, p.ProjectID).Scan(&branchState); err != nil || branchState != "ready" {
		fail(w, r, 409, "branch_not_ready", "Ready branch required")
		return
	}
	err = tx.QueryRow(r.Context(), `SELECT generation,state,spec,endpoint_id FROM object_storage_instances WHERE branch_id=$1`, p.BranchID).Scan(&generation, &state, &raw, &p.EndpointID)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read service intent")
		return
	}
	if generation != version {
		fail(w, r, 412, "version_conflict", "Storage generation changed; refresh before retrying")
		return
	}
	var pending bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait'))`, p.ProjectID).Scan(&pending); err != nil || pending {
		fail(w, r, 409, "operation_conflict", "Wait for project Operations before changing storage")
		return
	}
	if action == "enable_object_storage" {
		if state != "" && state != "disabled" {
			fail(w, r, 409, "disable_before_reconfigure", "Disable the existing service before changing configuration")
			return
		}
		if generation > 0 {
			var original ObjectStorageSpec
			if json.Unmarshal(raw, &original) != nil || original.Database != p.Spec.Database {
				fail(w, r, 409, "storage_database_immutable", "Re-enable the original storage database")
				return
			}
		}
		if err = tx.QueryRow(r.Context(), `SELECT id FROM endpoints WHERE branch_id=$1 AND project_id=$2 AND endpoint_type='read_write' AND state='active' AND deleted_at IS NULL`, p.BranchID, p.ProjectID).Scan(&p.EndpointID); err != nil {
			fail(w, r, 409, "writer_required", "A ready branch writer is required")
			return
		}
		state = "provisioning"
	} else {
		if generation == 0 || state == "disabled" {
			fail(w, r, 409, "storage_not_enabled", "Object Storage is already disabled")
			return
		}
		if json.Unmarshal(raw, &p.Spec) != nil {
			fail(w, r, 503, "metadata_unavailable", "Invalid storage intent")
			return
		}
		state = "disabling"
	}
	spec, _ := json.Marshal(p.Spec)
	payload, _ := json.Marshal(p)
	id := newID("op_")
	_, err = tx.Exec(r.Context(), `INSERT INTO object_storage_instances(branch_id,project_id,endpoint_id,generation,state,spec) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(branch_id) DO UPDATE SET endpoint_id=$3,generation=$4,state=$5,spec=$6,updated_at=now()`, p.BranchID, p.ProjectID, p.EndpointID, p.Generation, state, spec)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'object_storage',$3,$4,'queued',$5,$6,$7)`, id, p.ProjectID, p.BranchID, action, userFrom(r).ID, requestID(r), payload)
	}
	if err == nil {
		err = addObjectStorageSteps(r.Context(), tx, id)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayObjectStorage(w, r, key, hash) {
			return
		}
		fail(w, r, 409, "operation_conflict", "Could not admit storage Operation")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, action, "object_storage", p.BranchID, "accepted", requestID(r))
	s.acceptObjectStorage(w, r, id)
}

func storageAdmit(w http.ResponseWriter, r *http.Request) (*http.Request, func(), bool) {
	select {
	case objectStorageSlots <- struct{}{}:
	default:
		fail(w, r, 429, "storage_capacity_exhausted", "Object request concurrency exhausted")
		return r, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	return r.WithContext(ctx), func() { cancel(); <-objectStorageSlots }, true
}
func (s *server) storageFailure(w http.ResponseWriter, r *http.Request, err error) {
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Storage resource not found")
	} else {
		fail(w, r, 503, "storage_unavailable", "Object Storage request unavailable; retain the request ID before retrying")
	}
}
func storageDirectoryLock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(794210044)`)
	return err
}
func (s *server) storageBuckets(w http.ResponseWriter, r *http.Request) {
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, p, close, err := s.openObjectStorage(r, r.PathValue("project"), r.PathValue("branch"))
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	defer close()
	if r.Method == http.MethodGet {
		rows, err := conn.Query(r.Context(), `SELECT b.name,b.access,b.created_at,count(o.key) AS object_count,COALESCE(sum(o.size),0)::bigint AS bytes FROM neon_storage.buckets b LEFT JOIN neon_storage.objects o ON o.bucket=b.name GROUP BY b.name ORDER BY b.name`)
		if err != nil {
			s.storageFailure(w, r, err)
			return
		}
		items, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			s.storageFailure(w, r, err)
			return
		}
		if items == nil {
			items = []map[string]any{}
		}
		jsonResponse(w, 200, record{"items": items, "branch_id": p.BranchID})
		return
	}
	var body struct {
		Name   string `json:"name"`
		Access string `json:"access"`
	}
	if readJSON(r, &body) != nil || !storageBucketName.MatchString(body.Name) || (body.Access != "private" && body.Access != "public_read") {
		fail(w, r, 422, "invalid_bucket", "Bucket name and private/public_read access are required")
		return
	}
	tx, err := conn.Begin(r.Context())
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err = storageDirectoryLock(r.Context(), tx); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM neon_storage.buckets`).Scan(&count); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	if count >= 32 {
		fail(w, r, 409, "bucket_quota_exhausted", "Branch bucket quota exhausted")
		return
	}
	tag, err := tx.Exec(r.Context(), `INSERT INTO neon_storage.buckets(name,access) VALUES($1,$2) ON CONFLICT DO NOTHING`, body.Name, body.Access)
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, r, 409, "bucket_exists", "Bucket already exists; inspect its access before retrying")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	close()
	s.audit(r.Context(), userFrom(r).ID, "create_bucket", "object_storage", p.BranchID, "succeeded", requestID(r))
	jsonResponse(w, 201, record{"name": body.Name, "access": body.Access})
}
func (s *server) storageBucketDelete(w http.ResponseWriter, r *http.Request) {
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, p, close, err := s.openObjectStorage(r, r.PathValue("project"), r.PathValue("branch"))
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	defer close()
	tx, err := conn.Begin(r.Context())
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err = storageDirectoryLock(r.Context(), tx); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	var occupied bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.objects WHERE bucket=$1)`, r.PathValue("bucket")).Scan(&occupied); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	if occupied {
		fail(w, r, 409, "bucket_not_empty", "Delete the branch object entries before deleting the bucket")
		return
	}
	tag, err := tx.Exec(r.Context(), `DELETE FROM neon_storage.buckets WHERE name=$1`, r.PathValue("bucket"))
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, r, 404, "not_found", "Bucket not found")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	close()
	s.audit(r.Context(), userFrom(r).ID, "delete_bucket", "object_storage", p.BranchID, "succeeded", requestID(r))
	jsonResponse(w, 200, record{"deleted": true, "physical_gc": "held_for_branch_and_pitr_references"})
}

func (s *server) storageObjects(w http.ResponseWriter, r *http.Request) {
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, p, close, err := s.openObjectStorage(r, r.PathValue("project"), r.PathValue("branch"))
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	defer close()
	bucket, key := r.PathValue("bucket"), r.URL.Query().Get("key")
	if !storageBucketName.MatchString(bucket) {
		fail(w, r, 422, "invalid_bucket", "Invalid bucket name")
		return
	}
	if r.Method == http.MethodGet && key == "" {
		prefix, after := r.URL.Query().Get("prefix"), r.URL.Query().Get("after")
		if len(prefix) > 1024 || len(after) > 1024 {
			fail(w, r, 422, "invalid_cursor", "Prefix and cursor must be at most 1024 bytes")
			return
		}
		var exists bool
		if err = conn.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, bucket).Scan(&exists); err != nil || !exists {
			s.storageFailure(w, r, pgx.ErrNoRows)
			return
		}
		rows, err := conn.Query(r.Context(), `SELECT key,sha256,size,content_type,updated_at FROM neon_storage.objects WHERE bucket=$1 AND starts_with(key,$2) AND key>$3 COLLATE "C" ORDER BY key COLLATE "C" LIMIT 101`, bucket, prefix, after)
		if err != nil {
			s.storageFailure(w, r, err)
			return
		}
		items, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			s.storageFailure(w, r, err)
			return
		}
		next := ""
		if len(items) > 100 {
			items = items[:100]
			next = stringVal(items[99]["key"])
		}
		if items == nil {
			items = []map[string]any{}
		}
		jsonResponse(w, 200, record{"items": items, "next_after": next, "branch_id": p.BranchID})
		return
	}
	if !validStorageKey(key) {
		fail(w, r, 422, "invalid_object_key", "Object key must be a valid relative UTF-8 key, at most 1024 bytes")
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.serveStorageObject(w, r, conn, p, bucket, key, false)
		return
	}
	tx, err := conn.Begin(r.Context())
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err = storageDirectoryLock(r.Context(), tx); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	var previous string
	var oldSize int64
	err = tx.QueryRow(r.Context(), `SELECT sha256,size FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, key).Scan(&previous, &oldSize)
	if err != nil && !isNoRows(err) {
		s.storageFailure(w, r, err)
		return
	}
	if !storageWriteCondition(w, r, previous) {
		return
	}
	if r.Method == http.MethodDelete {
		if _, err = tx.Exec(r.Context(), `DELETE FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, key); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			s.storageFailure(w, r, err)
			return
		}
		close()
		s.audit(r.Context(), userFrom(r).ID, "delete_object", "object_storage", p.BranchID, "succeeded", requestID(r))
		jsonResponse(w, 200, record{"deleted": true, "physical_gc": "held_for_branch_and_pitr_references"})
		return
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if len(contentType) > 256 {
		fail(w, r, 422, "invalid_content_type", "Content-Type too long")
		return
	}
	if _, _, err = mime.ParseMediaType(contentType); err != nil {
		fail(w, r, 422, "invalid_content_type", "Valid Content-Type required")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, storageObjectLimit))
	if err != nil {
		fail(w, r, 413, "object_too_large", "Objects are limited to 8 MiB")
		return
	}
	var exists bool
	var count int
	var total int64
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, bucket).Scan(&exists); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	if !exists {
		fail(w, r, 404, "not_found", "Bucket not found")
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT count(*),COALESCE(sum(size),0)::bigint FROM neon_storage.objects`).Scan(&count, &total); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	if (previous == "" && count >= 1000) || total-oldSize+int64(len(data)) > storageBranchLimit {
		fail(w, r, 409, "object_quota_exhausted", "Branch object quota exhausted")
		return
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	blob := p.ProjectID + "/" + newID("blob_")
	// Publish only after the immutable blob is durable. Interruption can leak
	// an unreferenced blob, but never a catalog entry pointing at absent bytes.
	// No immediate GC: other branches and historical LSNs may still reference it.
	if err = s.storage.put(r.Context(), blob, contentType, data); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(bucket,key) DO UPDATE SET blob_key=$3,sha256=$4,size=$5,content_type=$6,updated_at=now()`, bucket, key, blob, digest, len(data), contentType)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	close()
	s.audit(r.Context(), userFrom(r).ID, "put_object", "object_storage", p.BranchID, "succeeded", requestID(r))
	w.Header().Set("ETag", `"`+digest+`"`)
	jsonResponse(w, 200, record{"key": key, "sha256": digest, "size": len(data)})
}
func storageWriteCondition(w http.ResponseWriter, r *http.Request, previous string) bool {
	if r.Header.Get("If-None-Match") == "*" && r.Method == http.MethodPut {
		if previous == "" {
			return true
		}
		fail(w, r, 412, "object_version_conflict", "Object already exists")
		return false
	}
	if r.Header.Get("If-Match") == "" {
		fail(w, r, 428, "object_version_required", "Use If-None-Match: * for creation or the current quoted ETag in If-Match")
		return false
	}
	if previous == "" || r.Header.Get("If-Match") != `"`+previous+`"` {
		fail(w, r, 412, "object_version_conflict", "Object version changed; refresh before retrying")
		return false
	}
	return true
}
func (s *server) serveStorageObject(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket, key string, anonymous bool) {
	var blob, digest, contentType, access string
	var size int64
	var modified time.Time
	err := conn.QueryRow(r.Context(), `SELECT o.blob_key,o.sha256,o.size,o.content_type,o.updated_at,b.access FROM neon_storage.objects o JOIN neon_storage.buckets b ON b.name=o.bucket WHERE o.bucket=$1 AND o.key=$2`, bucket, key).Scan(&blob, &digest, &size, &contentType, &modified, &access)
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	if !strings.HasPrefix(blob, p.ProjectID+"/") {
		s.storageFailure(w, r, errors.New("object identity mismatch"))
		return
	}
	if anonymous && access != "public_read" && !s.validStorageDownload(r, p, bucket, key, blob) {
		fail(w, r, 404, "not_found", "Object not found")
		return
	}
	data, err := s.storage.read(r.Context(), blob, digest, size)
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": key[strings.LastIndex(key, "/")+1:]}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", modified, bytes.NewReader(data))
}

type storageDownload struct {
	Branch     string `json:"branch"`
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	Blob       string `json:"blob"`
	Actor      string `json:"actor"`
	Generation int64  `json:"generation"`
	Expires    int64  `json:"expires"`
}

func (s *server) signStorageDownload(v storageDownload) string {
	raw, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.idempotencyKey)
	_, _ = mac.Write([]byte("neon-object-download-v1:" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *server) decodeStorageDownload(token string) (storageDownload, bool) {
	var v storageDownload
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(token) > 4096 || len(s.idempotencyKey) < 32 {
		return v, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return v, false
	}
	mac := hmac.New(sha256.New, s.idempotencyKey)
	_, _ = mac.Write([]byte("neon-object-download-v1:" + parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return v, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(raw, &v) != nil || v.Expires <= time.Now().Unix() || v.Expires > time.Now().Add(15*time.Minute).Unix() {
		return v, false
	}
	return v, true
}
func (s *server) validStorageDownload(r *http.Request, p objectStoragePayload, bucket, key, blob string) bool {
	v, ok := s.decodeStorageDownload(r.URL.Query().Get("token"))
	if !ok || v.Branch != p.BranchID || v.Bucket != bucket || v.Key != key || v.Blob != blob || v.Generation != p.Generation {
		return false
	}
	var active bool
	if p.metadata == nil {
		return false
	}
	// Re-check membership and account revocation on the already-held metadata
	// transaction; no extra pool connection or credential cache is involved.
	err := p.metadata.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users u JOIN organization_members m ON m.user_id=u.id JOIN projects p ON p.org_id=m.org_id JOIN organizations o ON o.id=m.org_id AND o.state='active' LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id WHERE u.id=$1 AND NOT u.disabled AND p.id=$2 AND (m.role IN ('owner','admin','editor','developer','viewer') OR g.role IN ('admin','editor','viewer')))`, v.Actor, p.ProjectID).Scan(&active)
	return err == nil && active
}
func (s *server) storagePresign(w http.ResponseWriter, r *http.Request) {
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	var body struct {
		Key     string `json:"key"`
		Expires int64  `json:"expires_seconds"`
	}
	if readJSON(r, &body) != nil || !validStorageKey(body.Key) || body.Expires < 1 || body.Expires > 900 {
		fail(w, r, 422, "invalid_presign", "Object key and expiry of 1 to 900 seconds required")
		return
	}
	conn, p, close, err := s.openObjectStorage(r, r.PathValue("project"), r.PathValue("branch"))
	if err != nil {
		s.storageFailure(w, r, err)
		return
	}
	defer close()
	var blob string
	if err = conn.QueryRow(r.Context(), `SELECT blob_key FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, r.PathValue("bucket"), body.Key).Scan(&blob); err != nil {
		s.storageFailure(w, r, err)
		return
	}
	v := storageDownload{p.BranchID, r.PathValue("bucket"), body.Key, blob, userFrom(r).ID, p.Generation, time.Now().Unix() + body.Expires}
	q := url.Values{"key": {body.Key}, "token": {s.signStorageDownload(v)}}
	jsonResponse(w, 200, record{"url": "/storage/v1/" + p.BranchID + "/" + url.PathEscape(v.Bucket) + "?" + q.Encode(), "expires_at": time.Unix(v.Expires, 0).UTC(), "method": "GET", "protocol": "neon-object-rest-v1"})
}
func (s *server) storagePublic(w http.ResponseWriter, r *http.Request) {
	branch, bucket, key := r.PathValue("storageBranch"), r.PathValue("bucket"), r.URL.Query().Get("key")
	if !dataBranchID.MatchString(branch) || !storageBucketName.MatchString(bucket) || !validStorageKey(key) {
		http.NotFound(w, r)
		return
	}
	if token := r.URL.Query().Get("token"); token != "" {
		v, ok := s.decodeStorageDownload(token)
		if !ok || v.Branch != branch || v.Bucket != bucket || v.Key != key {
			http.NotFound(w, r)
			return
		}
	}
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, p, close, err := s.openObjectStorage(r, "", branch)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer close()
	s.serveStorageObject(w, r, conn, p, bucket, key, true)
}
