package control

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type sqlHistorySharing struct {
	ID      string     `json:"history_id"`
	Shared  bool       `json:"is_shared"`
	Version string     `json:"version_token"`
	At      *time.Time `json:"shared_at"`
}

func (s *server) sqlHistorySharing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := userFrom(r)
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console login required")
		return
	}
	project, id := r.PathValue("project"), r.PathValue("history")
	if r.Method == http.MethodGet {
		value := sqlHistorySharing{ID: id}
		err := s.db.QueryRow(r.Context(), `SELECT shared_at IS NOT NULL,share_generation::text,shared_at FROM sql_query_history WHERE project_id=$1 AND owner_id=$2 AND id=$3`, project, u.ID, id).Scan(&value.Shared, &value.Version, &value.At)
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Personal history not found")
			return
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read sharing policy")
			return
		}
		w.Header().Set("ETag", `"`+value.Version+`"`)
		jsonResponse(w, 200, value)
		return
	}
	var body struct {
		Shared      *bool `json:"shared"`
		Acknowledge bool  `json:"acknowledge_sensitive_sql_sharing"`
	}
	if readJSON(r, &body) != nil || body.Shared == nil || (*body.Shared && !body.Acknowledge) {
		fail(w, r, 422, "sharing_acknowledgement_required", "Explicit sharing flag and sensitive SQL acknowledgement required")
		return
	}
	if *body.Shared && !sqlHistoryEnabled() {
		fail(w, r, 503, "history_disabled", "SQL history is disabled by the operator")
		return
	}
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "idempotency_unavailable", "Durable sharing admission unavailable")
		return
	}
	key, tag := r.Header.Get("Idempotency-Key"), r.Header.Get("If-Match")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, "\r\n") {
		fail(w, r, 422, "invalid_idempotency_key", "Durable Idempotency-Key required")
		return
	}
	if tag == "" {
		fail(w, r, 428, "precondition_required", "Current quoted sharing version required")
		return
	}
	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		fail(w, r, 422, "invalid_version", "Quoted sharing version required")
		return
	}
	version, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
	if err != nil || version < 0 || version == 9223372036854775807 {
		fail(w, r, 422, "invalid_version", "Invalid sharing version")
		return
	}
	keyHash, hash := s.createRequestHash("history-sharing-key", key), s.createRequestHash("history-sharing-input", record{"project": project, "history": id, "version": version, "body": body})
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not update sharing")
		return
	}
	defer tx.Rollback(r.Context())
	var current int64
	var outcome string
	var finished bool
	err = tx.QueryRow(r.Context(), `SELECT share_generation,outcome,finished_at IS NOT NULL FROM sql_query_history WHERE project_id=$1 AND owner_id=$2 AND id=$3 FOR UPDATE`, project, u.ID, id).Scan(&current, &outcome, &finished)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Personal history not found")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock personal history")
		return
	}
	var previous string
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM sql_history_sharing_requests WHERE owner_id=$1 AND history_id=$2 AND key_hash=$3`, u.ID, id, keyHash).Scan(&previous, &raw)
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key used with different sharing input")
			return
		}
		var value sqlHistorySharing
		if json.Unmarshal(raw, &value) != nil {
			fail(w, r, 503, "metadata_unavailable", "Invalid sharing receipt")
			return
		}
		w.Header().Set("ETag", `"`+value.Version+`"`)
		jsonResponse(w, 200, value)
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read sharing receipt")
		return
	}
	if current != version {
		fail(w, r, 412, "sharing_version_conflict", "Reload the current sharing policy")
		return
	}
	if *body.Shared && (!finished || (outcome != "completed" && outcome != "failed" && outcome != "not_submitted")) {
		fail(w, r, 409, "history_outcome_unconfirmed", "Only confirmed terminal history can be shared")
		return
	}
	var count int
	if tx.QueryRow(r.Context(), `SELECT count(*) FROM sql_history_sharing_requests WHERE owner_id=$1 AND history_id=$2 AND created_at>now()-interval '1 hour'`, u.ID, id).Scan(&count) != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check sharing rate")
		return
	}
	if count >= 60 {
		w.Header().Set("Retry-After", "3600")
		fail(w, r, 429, "sharing_rate_limited", "Retry later")
		return
	}
	value := sqlHistorySharing{ID: id}
	err = tx.QueryRow(r.Context(), `UPDATE sql_query_history SET shared_at=CASE WHEN $4 THEN now() ELSE NULL END,share_generation=share_generation+1 WHERE project_id=$1 AND owner_id=$2 AND id=$3 RETURNING shared_at IS NOT NULL,share_generation::text,shared_at`, project, u.ID, id, *body.Shared).Scan(&value.Shared, &value.Version, &value.At)
	raw, _ = json.Marshal(value)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO sql_history_sharing_requests(owner_id,history_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4,$5)`, u.ID, id, keyHash, hash, raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,$2,'sql_history',$3,'succeeded',$4)`, u.ID, map[bool]string{true: "share_sql_history", false: "revoke_sql_history_sharing"}[*body.Shared], id, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "sharing_outcome_unconfirmed", "Sharing outcome unconfirmed; reload policy or replay the exact key/input")
		return
	}
	w.Header().Set("ETag", `"`+value.Version+`"`)
	jsonResponse(w, 200, value)
}

// The owner's current project membership/grant is checked in the same database
// statement as every shared read. Global account roles grant no project access.
const sharedHistoryFrom = ` FROM sql_query_history h JOIN projects p ON p.id=h.project_id
 JOIN users u ON u.id=h.owner_id AND NOT u.disabled
 JOIN organization_members m ON m.org_id=p.org_id AND m.user_id=h.owner_id
 LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=h.owner_id
 WHERE h.project_id=$1 AND h.shared_at IS NOT NULL AND h.finished_at IS NOT NULL AND h.outcome IN ('completed','failed','not_submitted')
 AND (m.role IN ('owner','admin','editor','developer') OR g.role IN ('admin','editor'))`
const sharedHistoryColumns = `h.id,h.project_id,h.owner_id,u.username AS owner_username,h.endpoint_id,h.branch_id,h.mode,h.database_name,h.role_name,h.original_bytes,h.truncated,h.outcome,h.sqlstate,h.started_at,h.finished_at,h.deadline_at,h.duration_ms,false AS completion_unconfirmed,h.shared_at,h.share_generation::text AS sharing_version_token`

func (s *server) sharedSQLHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := userFrom(r)
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console login required")
		return
	}
	project, id := r.PathValue("project"), r.PathValue("history")
	if !sqlHistoryEnabled() {
		if id != "" {
			fail(w, r, 404, "not_found", "Shared history not found")
		} else {
			jsonResponse(w, 200, record{"enabled": false, "items": []record{}, "limit": 25, "next_cursor": nil})
		}
		return
	}
	if id != "" {
		item, err := s.one(r.Context(), `SELECT `+sharedHistoryColumns+`,h.key_version,h.sealed_sql`+sharedHistoryFrom+` AND h.id=$2`, project, id)
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Shared history not found")
			return
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read shared history")
			return
		}
		if s.backendKeys == nil {
			fail(w, r, 503, "history_key_unavailable", "SQL history key unavailable")
			return
		}
		text, err := s.openSavedSQL(stringVal(item["key_version"]), item["sealed_sql"].([]byte), sqlHistoryAAD(project, stringVal(item["owner_id"]), id))
		if err != nil {
			fail(w, r, 503, "history_key_unavailable", "Shared SQL cannot be decrypted")
			return
		}
		delete(item, "key_version")
		delete(item, "sealed_sql")
		item["sql"] = text
		jsonResponse(w, 200, item)
		return
	}
	cursor := sqlHistoryCursor{Project: project, Owner: u.ID, At: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: "~"}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if len(raw) > 1024 || err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Project != project || cursor.Owner != u.ID || cursor.At.IsZero() || cursor.ID == "" {
			fail(w, r, 422, "invalid_cursor", "Shared cursor does not belong to this reader/project")
			return
		}
	}
	items, err := s.many(r.Context(), `SELECT `+sharedHistoryColumns+sharedHistoryFrom+` AND (h.started_at,h.id)<($2,$3) ORDER BY h.started_at DESC,h.id DESC LIMIT 26`, project, cursor.At, cursor.ID)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not list shared history")
		return
	}
	var next any
	if len(items) > 25 {
		items = items[:25]
		last := items[24]
		raw, _ := json.Marshal(sqlHistoryCursor{project, u.ID, last["started_at"].(time.Time), stringVal(last["id"])})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	jsonResponse(w, 200, record{"enabled": true, "items": items, "limit": 25, "next_cursor": next})
}
