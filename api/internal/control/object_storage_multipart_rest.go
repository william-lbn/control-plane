package control

import (
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Console operations use the existing session/CSRF/project authorization and
// admission/lifecycle gates. Uploads are private to the current user; S3 uploads
// are separately bound to their credential ID, never a shared bucket password.
func (s *server) storageMultipart(w http.ResponseWriter, r *http.Request) {
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
	bucket, id, principal := r.PathValue("bucket"), r.PathValue("upload"), "user:"+userFrom(r).ID
	if !storageBucketName.MatchString(bucket) {
		multipartError(w, r, multipartFault(400, "InvalidArgument"), false)
		return
	}
	if id == "" {
		if r.Method == http.MethodGet {
			s.storageMultipartList(w, r, conn, p, principal, bucket)
			return
		}
		requestKey, ok := creationKey(w, r)
		if !ok {
			return
		}
		var input multipartCreate
		if readJSON(r, &input) != nil || !validStorageDigest(input.SHA256) || input.Size < 0 {
			multipartError(w, r, multipartFault(400, "InvalidArgument"), false)
			return
		}
		u, e := s.createMultipart(r.Context(), conn, p, principal, bucket, requestKey, input)
		if e != nil {
			multipartError(w, r, e, false)
			return
		}
		jsonResponse(w, 201, u)
		return
	}
	if r.PathValue("part") != "" {
		n, e := strconv.Atoi(r.PathValue("part"))
		if e != nil {
			multipartError(w, r, multipartFault(400, "InvalidArgument"), false)
			return
		}
		data, e := io.ReadAll(http.MaxBytesReader(w, r.Body, storagePartLimit))
		if e != nil {
			multipartError(w, r, multipartFault(413, "EntityTooLarge"), false)
			return
		}
		part, e := s.putMultipartPart(r.Context(), conn, p, principal, bucket, "", id, n, data, r.Header.Get("Content-MD5"), r.Header)
		if e != nil {
			multipartError(w, r, e, false)
			return
		}
		jsonResponse(w, 200, part)
		return
	}
	if r.Method == http.MethodDelete {
		if e := abortMultipart(r.Context(), conn, p, principal, bucket, "", id); e != nil {
			multipartError(w, r, e, false)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.Method == http.MethodPost {
		if r.Header.Get("If-Match") == "" && r.Header.Get("If-None-Match") != "*" {
			fail(w, r, 428, "object_version_required", "Use If-None-Match: * or the current target ETag")
			return
		}
		var input struct {
			Parts []multipartSelection `json:"parts"`
			multipartCompletionChecksums
		}
		if readJSON(r, &input) != nil {
			multipartError(w, r, multipartFault(400, "InvalidArgument"), false)
			return
		}
		opt, e := multipartCompletionHeaders(r.Header)
		if e != nil {
			multipartError(w, r, e, false)
			return
		}
		if input.Type != "" || len(input.Values) != 0 || input.Size != nil {
			if opt.Type != "" || len(opt.Values) != 0 || opt.Size != nil {
				multipartError(w, r, multipartFault(400, "InvalidArgument"), false)
				return
			}
			opt = input.multipartCompletionChecksums
		}
		u, e := s.completeMultipart(r.Context(), conn, p, principal, bucket, "", id, input.Parts, r.Header.Get("If-Match"), r.Header.Get("If-None-Match"), opt)
		if e != nil {
			multipartError(w, r, e, false)
			return
		}
		jsonResponse(w, 200, u)
		return
	}
	tx, e := conn.Begin(r.Context())
	if e != nil {
		multipartError(w, r, e, false)
		return
	}
	defer tx.Rollback(r.Context())
	u, e := loadMultipart(r.Context(), tx, p, principal, bucket, "", id)
	if e != nil {
		multipartError(w, r, e, false)
		return
	}
	parts, e := listMultipartParts(r.Context(), tx, id)
	if e != nil {
		multipartError(w, r, e, false)
		return
	}
	jsonResponse(w, 200, record{"upload": u, "parts": parts, "physical_gc": "held_for_branch_and_pitr_references"})
}
func (s *server) storageMultipartList(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, principal, bucket string) {
	rows, e := conn.Query(r.Context(), `SELECT id,key,content_type,metadata,expected_sha256,expected_size,state,created_at,result_sha256,result_size,checksum_algorithm,checksum_type FROM neon_storage.multipart_uploads WHERE branch_id=$1 AND principal=$2 AND bucket=$3 AND state='uploading' ORDER BY created_at,id LIMIT 32`, p.BranchID, principal, bucket)
	if e != nil {
		multipartError(w, r, e, false)
		return
	}
	defer rows.Close()
	items := []multipartUpload{}
	for rows.Next() {
		var u multipartUpload
		if e = rows.Scan(&u.ID, &u.Key, &u.ContentType, &u.Metadata, &u.ExpectedSHA256, &u.ExpectedSize, &u.State, &u.Created, &u.SHA256, &u.Size, &u.ChecksumAlgorithm, &u.ChecksumType); e != nil {
			multipartError(w, r, e, false)
			return
		}
		items = append(items, u)
	}
	if e = rows.Err(); e != nil {
		multipartError(w, r, e, false)
		return
	}
	jsonResponse(w, 200, record{"items": items, "object_bytes": storageObjectAdmissionLimit(), "part_bytes": multipartMinimumPart, "active_limit": multipartActiveLimit})
}
