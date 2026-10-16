package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"
	"unicode/utf8"
)

const sqlHistoryLimit = 9 * 1024

func sqlHistoryEnabled() bool { return os.Getenv("NEON_V2_SQL_HISTORY_ENABLED") == "true" }
func sqlHistoryText(sql string) (string, bool) {
	if len(sql) <= sqlHistoryLimit {
		return sql, false
	}
	prefix := "-- QUERY TRUNCATED\n"
	text := sql[:sqlHistoryLimit-len(prefix)]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return prefix + text, true
}
func sqlHistoryAAD(project, owner, id string) []byte {
	value, _ := json.Marshal([]string{"sql-query-history-v1", project, owner, id})
	return value
}

type sqlHistoryAdmission struct {
	s     *server
	id    string
	start time.Time
}

// Admission is durable before any user SQL can be sent. An interrupted request
// remains admitted/unconfirmed; this journal never authorizes a SQL replay.
func (s *server) beginSQLHistory(r *http.Request, item record, mode, role, database, sql string) (*sqlHistoryAdmission, error) {
	if !sqlHistoryEnabled() {
		return nil, nil
	}
	preference, policyErr := s.readSQLHistoryPreference(r.Context(), userFrom(r).ID)
	if policyErr != nil {
		return nil, policyErr
	}
	if !preference.Capture {
		return nil, nil
	}
	if s.backendKeys == nil || s.backendKeys.decoded[s.backendKeys.Active] == nil {
		return nil, errors.New("SQL history encryption unavailable")
	}
	project, owner, id := r.PathValue("project"), userFrom(r).ID, newID("qh_")
	text, truncated := sqlHistoryText(sql)
	version := s.backendKeys.Active
	// Reuse the authenticated SQL envelope primitive, with disjoint history AAD.
	sealed, e := s.sealSavedSQL(version, text, sqlHistoryAAD(project, owner, id))
	if e != nil {
		return nil, e
	}
	start := time.Now().UTC()
	_, e = s.db.Exec(r.Context(), `INSERT INTO sql_query_history(id,project_id,owner_id,endpoint_id,branch_id,mode,database_name,role_name,key_version,sealed_sql,original_bytes,truncated,outcome,started_at,deadline_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'admitted',$13,$14)`, id, project, owner, r.PathValue("endpoint"), stringVal(item["branch_id"]), mode, database, role, version, sealed, len(sql), truncated, start, start.Add(sqlTotalTimeout+15*time.Second))
	if e != nil {
		return nil, e
	}
	return &sqlHistoryAdmission{s, id, start}, nil
}
func (h *sqlHistoryAdmission) finish(w http.ResponseWriter, outcome, sqlstate string) {
	if h == nil {
		return
	}
	if !safeSQLState.MatchString(sqlstate) {
		sqlstate = ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, e := h.s.db.Exec(ctx, `UPDATE sql_query_history SET outcome=$2,sqlstate=$3,finished_at=now(),duration_ms=$4 WHERE id=$1 AND outcome='admitted'`, h.id, outcome, sqlstate, time.Since(h.start).Milliseconds())
	w.Header().Set("X-Neon-SQL-History-ID", h.id)
	if e != nil || tag.RowsAffected() != 1 {
		w.Header().Set("X-Neon-SQL-History-State", "unconfirmed")
	} else {
		w.Header().Set("X-Neon-SQL-History-State", outcome)
	}
}
func sqlHistoryFailure(err error) (string, string) {
	f := classifySQLFailure(err)
	if f.stage == "connect" || f.stage == "session" || f.stage == "configuration" {
		return "not_submitted", f.sqlState
	}
	if f.sqlState != "" && f.class == "sql_error" {
		return "failed", f.sqlState
	}
	return "outcome_unknown", f.sqlState
}

type sqlHistoryCursor struct {
	Project string    `json:"project"`
	Owner   string    `json:"owner"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
}

const sqlHistoryColumns = `id,project_id,owner_id,endpoint_id,branch_id,mode,database_name,role_name,original_bytes,truncated,outcome,sqlstate,started_at,finished_at,deadline_at,duration_ms,(finished_at IS NULL AND deadline_at<now()) AS completion_unconfirmed,shared_at,share_generation::text AS sharing_version_token,name_generation::text AS name_version_token,name_updated_at,name_key_version,sealed_name`

// Personal SQL text remains owner-only, including against another project admin.
// Metadata reads have no data-plane/SQL connection and cannot wake Compute.
func (s *server) sqlHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if userFrom(r).APIKey {
		fail(w, r, 403, "console_session_required", "Console session required for personal SQL history")
		return
	}
	project, owner, id := r.PathValue("project"), userFrom(r).ID, r.PathValue("history")
	if !sqlHistoryEnabled() {
		if id != "" {
			http.NotFound(w, r)
		} else {
			jsonResponse(w, 200, record{"enabled": false, "items": []record{}, "limit": 25, "next_cursor": nil})
		}
		return
	}
	if s.backendKeys == nil {
		fail(w, r, 503, "history_key_unavailable", "SQL history encryption unavailable")
		return
	}
	if id != "" {
		item, e := s.one(r.Context(), `SELECT `+sqlHistoryColumns+`,key_version,sealed_sql FROM sql_query_history WHERE project_id=$1 AND owner_id=$2 AND id=$3`, project, owner, id)
		if e != nil {
			if isNoRows(e) {
				fail(w, r, 404, "not_found", "SQL history not found")
			} else {
				fail(w, r, 503, "metadata_unavailable", "Could not read SQL history")
			}
			return
		}
		text, e := s.openSavedSQL(stringVal(item["key_version"]), item["sealed_sql"].([]byte), sqlHistoryAAD(project, owner, id))
		if e != nil {
			fail(w, r, 503, "history_key_unavailable", "SQL history cannot be decrypted")
			return
		}
		delete(item, "key_version")
		delete(item, "sealed_sql")
		if s.openHistoryName(item, project, owner) != nil {
			fail(w, r, 503, "history_key_unavailable", "Personal history name cannot be decrypted")
			return
		}
		item["sql"] = text
		jsonResponse(w, 200, item)
		return
	}
	cursor := sqlHistoryCursor{Project: project, Owner: owner, At: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: "~"}
	raw := r.URL.Query().Get("cursor")
	if raw != "" {
		data, e := base64.RawURLEncoding.DecodeString(raw)
		if len(raw) > 1024 || e != nil || json.Unmarshal(data, &cursor) != nil || cursor.Project != project || cursor.Owner != owner || cursor.At.IsZero() || cursor.ID == "" {
			fail(w, r, 422, "invalid_cursor", "History cursor does not belong to this owner/project")
			return
		}
	}
	items, e := s.many(r.Context(), `SELECT `+sqlHistoryColumns+` FROM sql_query_history WHERE project_id=$1 AND owner_id=$2 AND (started_at,id)<($3,$4) ORDER BY started_at DESC,id DESC LIMIT 26`, project, owner, cursor.At, cursor.ID)
	if e != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not list SQL history")
		return
	}
	var next any
	if len(items) > 25 {
		items = items[:25]
		last := items[24]
		value, _ := json.Marshal(sqlHistoryCursor{project, owner, last["started_at"].(time.Time), stringVal(last["id"])})
		next = base64.RawURLEncoding.EncodeToString(value)
	}
	for _, item := range items {
		if s.openHistoryName(item, project, owner) != nil {
			fail(w, r, 503, "history_key_unavailable", "Personal history names cannot be decrypted")
			return
		}
	}
	jsonResponse(w, 200, record{"enabled": true, "items": items, "limit": 25, "next_cursor": next})
}
