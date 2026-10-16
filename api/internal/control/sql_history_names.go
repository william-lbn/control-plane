package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type sqlHistoryName struct {
	ID      string     `json:"history_id"`
	Name    *string    `json:"name"`
	Version string     `json:"version_token"`
	Updated *time.Time `json:"updated_at"`
}
type sqlHistoryNameReceipt struct {
	ID      string    `json:"history_id"`
	HasName bool      `json:"has_name"`
	Version string    `json:"version_token"`
	Updated time.Time `json:"updated_at"`
}

func validSQLHistoryName(name string) bool {
	if name == "" {
		return true
	} // Empty input explicitly clears the annotation.
	if !utf8.ValidString(name) || name != strings.TrimSpace(name) || len(name) > 240 || utf8.RuneCountInString(name) > 80 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}
func sqlHistoryNameAAD(project, owner, id string) []byte {
	raw, _ := json.Marshal([]string{"sql-history-name-v1", project, owner, id})
	return raw
}

// Private names use disjoint AEAD associated data. They never enter shared
// history views, audit payloads or durable idempotency receipts as plaintext.
func (s *server) openHistoryName(item record, project, owner string) error {
	var name any
	if raw, ok := item["sealed_name"].([]byte); ok && len(raw) > 0 {
		value, err := s.openSavedSQL(stringVal(item["name_key_version"]), raw, sqlHistoryNameAAD(project, owner, stringVal(item["id"])))
		if err != nil {
			return err
		}
		if !validSQLHistoryName(value) || value == "" {
			return errors.New("invalid encrypted history name")
		}
		name = value
	}
	delete(item, "name_key_version")
	delete(item, "sealed_name")
	item["name"] = name
	return nil
}

func (s *server) sqlHistoryName(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := userFrom(r)
	project, id := r.PathValue("project"), r.PathValue("history")
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console session required for personal annotations")
		return
	}
	if !sqlHistoryEnabled() {
		fail(w, r, 503, "history_disabled", "SQL history disabled by operator")
		return
	}
	if r.Method == http.MethodGet {
		item, err := s.one(r.Context(), `SELECT id,name_generation::text AS version_token,name_updated_at AS updated_at,name_key_version,sealed_name FROM sql_query_history WHERE project_id=$1 AND owner_id=$2 AND id=$3`, project, u.ID, id)
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Personal history not found")
			return
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read history name")
			return
		}
		if s.openHistoryName(item, project, u.ID) != nil {
			fail(w, r, 503, "history_key_unavailable", "Personal history name cannot be decrypted")
			return
		}
		item["history_id"] = item["id"]
		delete(item, "id")
		w.Header().Set("ETag", `"`+stringVal(item["version_token"])+`"`)
		jsonResponse(w, 200, item)
		return
	}
	var body struct {
		Name *string `json:"name"`
	}
	if readJSON(r, &body) != nil || body.Name == nil || !validSQLHistoryName(*body.Name) {
		fail(w, r, 422, "invalid_history_name", "Name must be empty or a trimmed single line of at most 80 characters and 240 UTF-8 bytes")
		return
	}
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "idempotency_unavailable", "Durable name admission unavailable")
		return
	}
	key, tag := r.Header.Get("Idempotency-Key"), r.Header.Get("If-Match")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, "\r\n") {
		fail(w, r, 422, "invalid_idempotency_key", "Durable Idempotency-Key required")
		return
	}
	if tag == "" {
		fail(w, r, 428, "precondition_required", "Current quoted name version required")
		return
	}
	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		fail(w, r, 422, "invalid_version", "Quoted name generation required")
		return
	}
	version, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
	if err != nil || version < 0 || version == 9223372036854775807 || strconv.FormatInt(version, 10) != tag[1:len(tag)-1] {
		fail(w, r, 422, "invalid_version", "Invalid name generation")
		return
	}
	keyHash, hash := s.createRequestHash("history-name-key", key), s.createRequestHash("history-name-input", record{"project": project, "history": id, "version": version, "name": *body.Name})
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Name transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var current int64
	var outcome string
	var finished bool
	err = tx.QueryRow(r.Context(), `SELECT h.name_generation,h.outcome,h.finished_at IS NOT NULL FROM sql_query_history h
 JOIN projects p ON p.id=h.project_id AND p.state='ready' AND p.deleted_at IS NULL
 JOIN organizations o ON o.id=p.org_id AND o.state='active'
 JOIN users u ON u.id=h.owner_id AND NOT u.disabled
 JOIN organization_members m ON m.org_id=p.org_id AND m.user_id=u.id
 LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=u.id
 WHERE h.project_id=$1 AND h.owner_id=$2 AND h.id=$3
 AND (m.role IN ('owner','admin','editor','developer') OR g.role IN ('owner','admin','editor','developer')) FOR UPDATE OF h`, project, u.ID, id).Scan(&current, &outcome, &finished)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Current personal history not found")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock personal history")
		return
	}
	var previous string
	var receipt sqlHistoryNameReceipt
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM sql_history_name_requests WHERE owner_id=$1 AND history_id=$2 AND key_hash=$3`, u.ID, id, keyHash).Scan(&previous, &raw)
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key used with a different name input")
			return
		}
		if json.Unmarshal(raw, &receipt) != nil {
			fail(w, r, 503, "metadata_unavailable", "Invalid name receipt")
			return
		}
		w.Header().Set("ETag", `"`+receipt.Version+`"`)
		jsonResponse(w, 200, receipt)
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read name receipt")
		return
	}
	if current != version {
		fail(w, r, 412, "name_version_conflict", "Reload the current personal name")
		return
	}
	if !finished || (outcome != "completed" && outcome != "failed" && outcome != "not_submitted") {
		fail(w, r, 409, "history_outcome_unconfirmed", "Unconfirmed execution history must remain unmodified")
		return
	}
	var count int
	if tx.QueryRow(r.Context(), `SELECT count(*) FROM sql_history_name_requests WHERE owner_id=$1 AND history_id=$2 AND created_at>now()-interval '1 hour'`, u.ID, id).Scan(&count) != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check name rate")
		return
	}
	if count >= 60 {
		w.Header().Set("Retry-After", "3600")
		fail(w, r, 429, "name_rate_limited", "Retry later")
		return
	}
	var sealed []byte
	var keyVersion any
	if *body.Name != "" {
		if s.backendKeys == nil {
			fail(w, r, 503, "history_key_unavailable", "Name encryption unavailable")
			return
		}
		keyVersion = s.backendKeys.Active
		sealed, err = s.sealSavedSQL(s.backendKeys.Active, *body.Name, sqlHistoryNameAAD(project, u.ID, id))
		if err != nil {
			fail(w, r, 503, "history_key_unavailable", "Name encryption unavailable")
			return
		}
	}
	receipt.ID = id
	receipt.HasName = *body.Name != ""
	err = tx.QueryRow(r.Context(), `UPDATE sql_query_history SET name_generation=name_generation+1,name_key_version=$4,sealed_name=$5,name_updated_at=clock_timestamp() WHERE project_id=$1 AND owner_id=$2 AND id=$3 RETURNING name_generation::text,name_updated_at`, project, u.ID, id, keyVersion, sealed).Scan(&receipt.Version, &receipt.Updated)
	raw, _ = json.Marshal(receipt)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO sql_history_name_requests(owner_id,history_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4,$5)`, u.ID, id, keyHash, hash, raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'name_sql_history','sql_history',$2,'succeeded',$3)`, u.ID, id, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "name_outcome_unconfirmed", "Name outcome unconfirmed; reload or replay the exact key/input. Do not replay SQL")
		return
	}
	w.Header().Set("ETag", `"`+receipt.Version+`"`)
	jsonResponse(w, 200, receipt)
}
