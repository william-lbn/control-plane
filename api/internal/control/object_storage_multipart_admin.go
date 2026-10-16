package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type multipartAdminIntent struct {
	ID, State string
	Response  []byte
	Status    int
}

func validMultipartReason(reason string) bool {
	count := utf8.RuneCountInString(reason)
	if !utf8.ValidString(reason) || count < 8 || count > 512 || strings.TrimSpace(reason) != reason {
		return false
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Commit the control-plane intent and audit BEFORE touching branch SQL. Never
// hold an additional pool connection during branch I/O. The branch abort itself
// is idempotent, so duplicate continuations can safely converge on one receipt.
func (s *server) admitMultipartAdmin(r *http.Request, key, reason string) (multipartAdminIntent, error) {
	var out multipartAdminIntent
	if len(s.idempotencyKey) < 32 {
		return out, errors.New("persistent idempotency unavailable")
	}
	actor, project, branch, bucket, id := userFrom(r).ID, r.PathValue("project"), r.PathValue("branch"), r.PathValue("bucket"), r.PathValue("upload")
	kh := s.createRequestHash("multipart-admin-key", key)
	hash := s.createRequestHash("abort-storage-multipart", record{"project": project, "branch": branch, "bucket": bucket, "upload": id, "reason": reason})
	tx, e := s.db.Begin(r.Context())
	if e != nil {
		return out, e
	}
	defer tx.Rollback(r.Context())
	if _, e = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,752211))`, actor+":"+kh); e != nil {
		return out, e
	}
	var old string
	e = tx.QueryRow(r.Context(), `SELECT id,state,response,status,request_hash FROM object_multipart_admin_requests WHERE actor_id=$1 AND key_hash=$2`, actor, kh).Scan(&out.ID, &out.State, &out.Response, &out.Status, &old)
	if e == nil {
		if old != hash {
			return out, multipartFault(409, "IdempotencyConflict")
		}
		return out, tx.Commit(r.Context())
	}
	if !isNoRows(e) {
		return out, e
	}
	var parent string
	e = tx.QueryRow(r.Context(), `SELECT id FROM projects WHERE id=$1 AND state='ready' AND deleted_at IS NULL FOR SHARE`, project).Scan(&parent)
	if isNoRows(e) {
		return out, multipartFault(404, "NoSuchUpload")
	}
	if e != nil {
		return out, e
	}
	e = tx.QueryRow(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND state='ready' AND deleted_at IS NULL FOR SHARE`, branch, project).Scan(&parent)
	if isNoRows(e) {
		return out, multipartFault(404, "NoSuchUpload")
	}
	if e != nil {
		return out, e
	}
	var count int
	if e = tx.QueryRow(r.Context(), `SELECT count(*) FROM object_multipart_admin_requests WHERE actor_id=$1 AND created_at>now()-interval '1 hour'`, actor).Scan(&count); e != nil {
		return out, e
	}
	if count >= 60 {
		return out, multipartFault(429, "AdminRateLimited")
	}
	out.ID = newID("mpa_")
	out.State = "admitted"
	_, e = tx.Exec(r.Context(), `INSERT INTO object_multipart_admin_requests(id,actor_id,project_id,branch_id,bucket,upload_id,reason,key_hash,request_hash,request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, out.ID, actor, project, branch, bucket, id, reason, kh, hash, requestID(r))
	if e != nil {
		return out, e
	}
	_, e = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'admit_multipart_admin_abort','object_multipart_upload',$2,'admitted',$3)`, actor, id, requestID(r))
	if e != nil {
		return out, e
	}
	return out, tx.Commit(r.Context())
}
func (s *server) confirmMultipartAdmin(r *http.Request, intent multipartAdminIntent, status int, value record) ([]byte, int, error) {
	tx, e := s.db.Begin(r.Context())
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback(r.Context())
	var state, original string
	var prior []byte
	var priorStatus int
	e = tx.QueryRow(r.Context(), `SELECT state,response,status,request_id FROM object_multipart_admin_requests WHERE id=$1 AND actor_id=$2 FOR UPDATE`, intent.ID, userFrom(r).ID).Scan(&state, &prior, &priorStatus, &original)
	if e != nil {
		return nil, 0, e
	}
	if state == "confirmed" {
		return prior, priorStatus, tx.Commit(r.Context())
	}
	value["intent_id"] = intent.ID
	value["request_id"] = original
	raw, e := json.Marshal(value)
	if e != nil {
		return nil, 0, e
	}
	// Return PostgreSQL's stored JSONB representation on the first response as
	// well as replays; exact durable receipts must not drift after round-trip.
	e = tx.QueryRow(r.Context(), `UPDATE object_multipart_admin_requests SET state='confirmed',response=$2,status=$3,finished_at=now() WHERE id=$1 RETURNING response`, intent.ID, raw, status).Scan(&raw)
	if e != nil {
		return nil, 0, e
	}
	outcome := "succeeded"
	if status != 200 {
		outcome = "rejected"
	}
	_, e = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'confirm_multipart_admin_abort','object_multipart_upload',$2,$3,$4)`, userFrom(r).ID, r.PathValue("upload"), outcome, original)
	if e != nil {
		return nil, 0, e
	}
	return raw, status, tx.Commit(r.Context())
}
func (s *server) storageMultipartAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if userFrom(r).APIKey || accessFrom(r).Level < 3 {
		fail(w, r, 403, "project_admin_session_required", "Current project administrator session required")
		return
	}
	bucket, id := r.PathValue("bucket"), r.PathValue("upload")
	if !storageBucketName.MatchString(bucket) || (id != "" && !multipartID.MatchString(id)) {
		multipartError(w, r, multipartFault(400, "InvalidArgument"), false)
		return
	}
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	var intent multipartAdminIntent
	var e error
	if r.Method == http.MethodPost {
		key, ok := creationKey(w, r)
		if !ok {
			return
		}
		var input struct {
			Reason string `json:"reason"`
		}
		if readJSON(r, &input) != nil || !validMultipartReason(input.Reason) {
			fail(w, r, 422, "abort_reason_required", "An explicit 8-512 character reason is required")
			return
		}
		intent, e = s.admitMultipartAdmin(r, key, input.Reason)
		if e != nil {
			multipartError(w, r, e, false)
			return
		}
		if intent.State == "confirmed" {
			w.Header().Set("Idempotency-Replayed", "true")
			jsonResponse(w, intent.Status, json.RawMessage(intent.Response))
			return
		}
		w.Header().Set("X-Neon-Multipart-Intent", intent.ID)
	}
	conn, p, close, e := s.openObjectStorage(r, r.PathValue("project"), r.PathValue("branch"))
	if e != nil {
		s.storageFailure(w, r, e)
		return
	}
	defer close()
	if r.Method == http.MethodGet {
		s.storageMultipartAdminList(w, r, conn, p, bucket)
		return
	}
	owner, e := abortMultipartAsAdmin(r.Context(), conn, p, bucket, id)
	close() // free lifecycle/pool admission before control-plane audit commit
	status := 200
	value := record{"upload_id": id, "state": "aborted", "initiator": owner, "physical_gc": "held_for_branch_and_pitr_references"}
	if e != nil {
		var fault *storageMutationFault
		if !errors.As(e, &fault) {
			multipartError(w, r, e, false)
			return
		}
		status = fault.Status
		value = record{"upload_id": id, "code": fault.Code, "message": fault.Code, "retryable": false, "state": "unchanged"}
	}
	raw, status, e := s.confirmMultipartAdmin(r, intent, status, value)
	if e != nil {
		fail(w, r, 503, "abort_outcome_unconfirmed", "Intent retained; inspect original upload and retry exact key/input")
		return
	}
	jsonResponse(w, status, json.RawMessage(raw))
}
func abortMultipartAsAdmin(ctx context.Context, conn *pgx.Conn, p objectStoragePayload, bucket, id string) (string, error) {
	tx, e := multipartTx(ctx, conn)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	if !multipartID.MatchString(id) {
		return "", multipartFault(404, "NoSuchUpload")
	}
	var principal, state string
	e = tx.QueryRow(ctx, `SELECT principal,state FROM neon_storage.multipart_uploads WHERE id=$1 AND branch_id=$2 AND bucket=$3 FOR UPDATE`, id, p.BranchID, bucket).Scan(&principal, &state)
	if isNoRows(e) {
		return "", multipartFault(404, "NoSuchUpload")
	}
	if e != nil {
		return "", e
	}
	if state == "completed" {
		return "", multipartFault(409, "UploadAlreadyCompleted")
	}
	if state != "aborted" {
		_, e = tx.Exec(ctx, `UPDATE neon_storage.multipart_uploads SET state='aborted',updated_at=now() WHERE id=$1`, id)
		if e != nil {
			return "", e
		}
	}
	return principal, tx.Commit(ctx)
}
func (s *server) storageMultipartAdminList(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket string) {
	id := r.URL.Query().Get("upload_id")
	if id != "" && !multipartID.MatchString(id) {
		multipartError(w, r, multipartFault(400, "InvalidArgument"), false)
		return
	}
	rows, e := conn.Query(r.Context(), `SELECT u.id,u.key,u.principal,u.state,u.created_at,u.updated_at,count(v.part_number),COALESCE(sum(v.size),0)::bigint FROM neon_storage.multipart_uploads u LEFT JOIN neon_storage.multipart_parts v ON v.upload_id=u.id WHERE u.branch_id=$1 AND u.bucket=$2 AND (($3='' AND u.state='uploading') OR u.id=$3) GROUP BY u.id ORDER BY u.created_at,u.id LIMIT 32`, p.BranchID, bucket, id)
	if e != nil {
		multipartError(w, r, e, false)
		return
	}
	defer rows.Close()
	items := []record{}
	for rows.Next() {
		var upload, key, principal, state string
		var created, updated time.Time
		var count int
		var bytes int64
		if e = rows.Scan(&upload, &key, &principal, &state, &created, &updated, &count, &bytes); e != nil {
			multipartError(w, r, e, false)
			return
		}
		items = append(items, record{"upload_id": upload, "key": key, "initiator": principal, "state": state, "created_at": created, "updated_at": updated, "part_count": count, "part_bytes": bytes})
	}
	if e = rows.Err(); e != nil {
		multipartError(w, r, e, false)
		return
	}
	jsonResponse(w, 200, record{"items": items, "limit": 32, "physical_gc": "held_for_branch_and_pitr_references"})
}
