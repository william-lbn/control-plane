package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type sqlHistoryPreference struct {
	Capture bool   `json:"capture_enabled"`
	Days    int    `json:"retain_days"`
	Version string `json:"version_token"`
}

func defaultSQLHistoryPreference() sqlHistoryPreference { return sqlHistoryPreference{true, 0, "0"} }
func (s *server) readSQLHistoryPreference(ctx context.Context, owner string) (sqlHistoryPreference, error) {
	p := defaultSQLHistoryPreference()
	err := s.db.QueryRow(ctx, `SELECT capture_enabled,retain_days,generation::text FROM sql_history_preferences WHERE owner_id=$1`, owner).Scan(&p.Capture, &p.Days, &p.Version)
	if isNoRows(err) {
		err = nil
	}
	return p, err
}

func (s *server) sqlHistoryPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := userFrom(r)
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console login required")
		return
	}
	if r.Method == http.MethodGet {
		p, err := s.readSQLHistoryPreference(r.Context(), u.ID)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read history policy")
			return
		}
		w.Header().Set("ETag", `"`+p.Version+`"`)
		jsonResponse(w, 200, p)
		return
	}
	var body struct {
		Capture     *bool `json:"capture_enabled"`
		Days        *int  `json:"retain_days"`
		Acknowledge bool  `json:"acknowledge_expired_history_deletion"`
	}
	if readJSON(r, &body) != nil || body.Capture == nil || body.Days == nil || (*body.Days != 0 && *body.Days != 7 && *body.Days != 30 && *body.Days != 90) || (*body.Days > 0 && !body.Acknowledge) {
		fail(w, r, 422, "invalid_history_policy", "Explicit capture flag, retention 0/7/30/90 and deletion acknowledgement required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 || strings.ContainsAny(key, "\r\n") || len(s.idempotencyKey) < 32 {
		fail(w, r, 422, "invalid_idempotency_key", "Durable Idempotency-Key required")
		return
	}
	tag := r.Header.Get("If-Match")
	if tag == "" {
		fail(w, r, 428, "precondition_required", "If-Match with current policy version required")
		return
	}
	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		fail(w, r, 422, "invalid_version", "Quoted policy generation required")
		return
	}
	version, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
	if err != nil || version < 0 || version == 9223372036854775807 {
		fail(w, r, 422, "invalid_version", "Invalid policy generation")
		return
	}
	keyHash := s.createRequestHash("history-policy-idempotency", key)
	hash := s.createRequestHash("history-policy-request", record{"version": version, "body": body})
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not update history policy")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,771842))`, u.ID); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock history policy")
		return
	}
	var previous string
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM sql_history_policy_requests WHERE owner_id=$1 AND key_hash=$2`, u.ID, keyHash).Scan(&previous, &raw)
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key used with different policy input")
			return
		}
		var value sqlHistoryPreference
		if json.Unmarshal(raw, &value) != nil {
			fail(w, r, 503, "metadata_unavailable", "Invalid policy receipt")
			return
		}
		w.Header().Set("ETag", `"`+value.Version+`"`)
		jsonResponse(w, 200, value)
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read policy receipt")
		return
	}
	var current int64
	err = tx.QueryRow(r.Context(), `SELECT generation FROM sql_history_preferences WHERE owner_id=$1`, u.ID).Scan(&current)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not inspect policy")
		return
	}
	if current != version {
		fail(w, r, 412, "policy_version_conflict", "Reload the latest history policy")
		return
	}
	var count int
	if tx.QueryRow(r.Context(), `SELECT count(*) FROM sql_history_policy_requests WHERE owner_id=$1 AND created_at>now()-interval '1 hour'`, u.ID).Scan(&count) != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check policy rate")
		return
	}
	if count >= 60 {
		w.Header().Set("Retry-After", "3600")
		fail(w, r, 429, "history_policy_rate_limited", "Retry later")
		return
	}
	value := sqlHistoryPreference{*body.Capture, *body.Days, strconv.FormatInt(version+1, 10)}
	_, err = tx.Exec(r.Context(), `INSERT INTO sql_history_preferences(owner_id,capture_enabled,retain_days,generation) VALUES($1,$2,$3,$4) ON CONFLICT(owner_id) DO UPDATE SET capture_enabled=$2,retain_days=$3,generation=$4,updated_at=now()`, u.ID, value.Capture, value.Days, version+1)
	raw, _ = json.Marshal(value)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO sql_history_policy_requests(owner_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4)`, u.ID, keyHash, hash, raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'update_sql_history_policy','account_history',$1,'succeeded',$2)`, u.ID, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "policy_outcome_unconfirmed", "Policy outcome unconfirmed; reload policy or replay the original key")
		return
	}
	w.Header().Set("ETag", `"`+value.Version+`"`)
	jsonResponse(w, 200, value)
}

// No default retention, no in-flight/unknown deletion, bounded batches and row
// locks. Physical removal and aggregate per-owner audit commit together.
func (s *server) pruneSQLHistory(ctx context.Context, owner, requestID string) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `WITH selected AS (
 SELECT h.id FROM sql_query_history h JOIN sql_history_preferences p ON p.owner_id=h.owner_id
 WHERE ($1='' OR h.owner_id=$1) AND p.retain_days>0
 AND h.started_at<now()-make_interval(days=>p.retain_days)
 AND h.finished_at IS NOT NULL AND h.outcome IN ('completed','failed','not_submitted')
 ORDER BY h.started_at,h.id FOR UPDATE OF h,p SKIP LOCKED LIMIT 1000
 ) DELETE FROM sql_query_history h USING selected s WHERE h.id=s.id RETURNING h.owner_id`, owner)
	if err != nil {
		return 0, err
	}
	owners := map[string]int{}
	total := 0
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		owners[id]++
		total++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for actor := range owners {
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'prune_sql_history','account_history',$1,'succeeded',$2)`, actor, requestID); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return total, nil
}
func (s *server) sqlHistoryCleanup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if userFrom(r).APIKey {
		fail(w, r, 403, "console_session_required", "Console login required")
		return
	}
	var body struct {
		Acknowledge bool `json:"acknowledge_expired_history_deletion"`
	}
	if readJSON(r, &body) != nil || !body.Acknowledge {
		fail(w, r, 422, "deletion_acknowledgement_required", "Explicit history deletion acknowledgement required")
		return
	}
	count, err := s.pruneSQLHistory(r.Context(), userFrom(r).ID, requestID(r))
	if err != nil {
		fail(w, r, 503, "history_cleanup_unconfirmed", "Cleanup outcome unconfirmed; reload personal history")
		return
	}
	jsonResponse(w, 200, record{"deleted_count": count, "batch_limit": 1000, "more_possible": count == 1000})
}

func (s *server) maintainSQLHistory(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := s.pruneSQLHistory(ctx, "", "history-retention-"+newID("")); err != nil && s.logger != nil {
		s.logger.Warn("SQL history retention unavailable")
	}
}
