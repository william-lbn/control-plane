package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/william-lbn/control-plane/api/internal/functions"
	"net/http"
	"strings"
	"time"
)

type functionEvent struct {
	ID      string
	Started time.Time
}

func (s *server) beginFunctionEvent(ctx context.Context, r *http.Request, p functionInstanceTarget, instanceID, channel, method string) (functionEvent, error) {
	event := functionEvent{newID("fre_"), time.Now()}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = event.Started.Add(15 * time.Minute)
	}
	_, e := s.db.Exec(ctx, `INSERT INTO function_request_events(id,request_id,function_id,project_id,branch_id,deployment_id,instance_id,channel,method,outcome,started_at,deadline_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'in_progress',$10,$11)`, event.ID, requestID(r), p.FunctionID, p.ProjectID, p.BranchID, p.DeploymentID, instanceID, channel, method, event.Started, deadline)
	return event, e
}
func (s *server) finishFunctionEvent(event functionEvent, status int, bytes int64, outcome string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var applicationStatus any
	if status >= 100 && status <= 599 {
		applicationStatus = status
	}
	// A failed completion update intentionally leaves the durable admission as
	// in_progress. Reads expose its elapsed deadline as unconfirmed, not success.
	s.db.Exec(ctx, `UPDATE function_request_events SET outcome=$2,application_status=$3,response_bytes=$4,duration_ms=$5,finished_at=now() WHERE id=$1 AND outcome='in_progress'`, event.ID, outcome, applicationStatus, bytes, max(int64(0), time.Since(event.Started).Milliseconds()))
}

type functionLogCursor struct {
	Started  time.Time `json:"started"`
	ID       string    `json:"id"`
	Function string    `json:"function"`
}

func encodeFunctionLogCursor(cursor functionLogCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func decodeFunctionLogCursor(raw, functionID string) (functionLogCursor, error) {
	var out functionLogCursor
	if len(raw) > 512 {
		return out, errors.New("cursor too long")
	}
	data, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return out, e
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil || !strings.HasPrefix(out.ID, "fre_") || out.Started.IsZero() || out.Function != functionID {
		return out, errors.New("invalid cursor")
	}
	return out, nil
}
func (s *server) functionRequestLogs(w http.ResponseWriter, r *http.Request) {
	if !s.functionReadBranch(w, r) {
		return
	}
	if !functions.ValidSlug(r.PathValue("slug")) {
		fail(w, r, 422, "invalid_function_slug", "Valid slug required")
		return
	}
	limit, ok := functionReadLimit(w, r)
	if !ok {
		return
	}
	var id string
	if e := s.db.QueryRow(r.Context(), `SELECT id FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted'`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug")).Scan(&id); e != nil {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	cursor := functionLogCursor{Started: time.Now().Add(time.Hour), ID: "~", Function: id}
	if raw := r.URL.Query().Get("after"); raw != "" {
		var e error
		cursor, e = decodeFunctionLogCursor(raw, id)
		if e != nil {
			fail(w, r, 422, "invalid_page", "Cursor must belong to this function")
			return
		}
	}
	items, e := s.many(r.Context(), `SELECT id,request_id,channel,method,outcome,application_status,response_bytes,duration_ms,started_at,finished_at,(outcome='in_progress' AND deadline_at<now()) AS completion_unconfirmed FROM function_request_events WHERE function_id=$1 AND project_id=$2 AND branch_id=$3 AND (started_at,id)<($4,$5) ORDER BY started_at DESC,id DESC LIMIT $6`, id, r.PathValue("project"), r.PathValue("branch"), cursor.Started, cursor.ID, limit+1)
	if e != nil {
		fail(w, r, 503, "metadata_unavailable", "Request journal unavailable")
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeFunctionLogCursor(functionLogCursor{last["started_at"].(time.Time), stringVal(last["id"]), id})
	}
	if items == nil {
		items = []record{}
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 200, record{"items": items, "next_after": next, "function_id": id, "capture": "structured_status_only_no_headers_paths_queries_or_payloads", "retention": "held_until_operator_policy"})
}
