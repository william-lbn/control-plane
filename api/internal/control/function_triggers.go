package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/william-lbn/control-plane/api/internal/functions"
	"github.com/william-lbn/control-plane/api/internal/triggers"
)

type functionTriggerSchedule struct {
	Cron string `json:"cron"`
}

var functionTriggerID = regexp.MustCompile(`^ftr_[a-f0-9]{24}$`)

type functionTriggerView struct {
	ID        string                  `json:"trigger_id"`
	Type      string                  `json:"type"`
	Name      string                  `json:"name"`
	Slug      string                  `json:"function_slug"`
	Path      string                  `json:"function_path"`
	Schedule  functionTriggerSchedule `json:"schedule"`
	Enabled   bool                    `json:"enabled"`
	Version   string                  `json:"version_token"`
	Next      *time.Time              `json:"next_run_at"`
	Inherited bool                    `json:"inherited"`
	Created   time.Time               `json:"created_at"`
	Updated   time.Time               `json:"updated_at"`
}
type functionTriggerInput struct {
	Type     string                   `json:"type"`
	Name     *string                  `json:"name,omitempty"`
	Slug     *string                  `json:"function_slug,omitempty"`
	Path     *string                  `json:"function_path,omitempty"`
	Schedule *functionTriggerSchedule `json:"schedule,omitempty"`
	Enabled  *bool                    `json:"enabled,omitempty"`
}

func (input *functionTriggerInput) UnmarshalJSON(raw []byte) error {
	type wire functionTriggerInput
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, field := range fields {
		if bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return errors.New("trigger fields cannot be null")
		}
	}
	*input = functionTriggerInput(value)
	return nil
}

type triggerMutationReceipt struct {
	Status  int                  `json:"status"`
	Version string               `json:"version_token"`
	Trigger *functionTriggerView `json:"trigger,omitempty"`
}

const functionTriggerColumns = `t.id,t.type,t.name,f.slug,t.function_path,t.cron,t.enabled,t.generation::text,t.next_run_at,(t.origin_branch_id<>t.branch_id),t.created_at,t.updated_at`
const functionTriggerFrom = ` FROM function_triggers t JOIN function_definitions f ON f.id=t.function_id AND f.project_id=t.project_id AND f.branch_id=t.branch_id WHERE t.project_id=$1 AND t.branch_id=$2`

func scanFunctionTrigger(row pgx.Row) (functionTriggerView, error) {
	var value functionTriggerView
	err := row.Scan(&value.ID, &value.Type, &value.Name, &value.Slug, &value.Path, &value.Schedule.Cron, &value.Enabled, &value.Version, &value.Next, &value.Inherited, &value.Created, &value.Updated)
	return value, err
}
func (s *server) functionSchedulesEnabled() bool {
	return s.functions != nil && functionPublicOrigin() != "" && os.Getenv("NEON_FUNCTION_SCHEDULES_ENABLED") == "true"
}

func (s *server) functionTriggers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.functionReadBranch(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.mutateFunctionTrigger(w, r)
		return
	}
	project, branch, id := r.PathValue("project"), r.PathValue("branch"), r.PathValue("trigger")
	if id != "" {
		value, err := scanFunctionTrigger(s.db.QueryRow(r.Context(), `SELECT `+functionTriggerColumns+functionTriggerFrom+` AND t.id=$3 AND t.state='ready'`, project, branch, id))
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Trigger not found")
			return
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read trigger")
			return
		}
		w.Header().Set("ETag", `"`+value.Version+`"`)
		jsonResponse(w, 200, record{"trigger": value})
		return
	}
	limit, ok := functionReadLimit(w, r)
	if !ok {
		return
	}
	after := r.URL.Query().Get("after")
	if after != "" && !functionTriggerID.MatchString(after) {
		fail(w, r, 422, "invalid_cursor", "Invalid trigger cursor")
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT `+functionTriggerColumns+functionTriggerFrom+` AND t.state='ready' AND t.id>$3 ORDER BY t.id LIMIT $4`, project, branch, after, limit+1)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not list triggers")
		return
	}
	defer rows.Close()
	items := []functionTriggerView{}
	for rows.Next() {
		value, e := scanFunctionTrigger(rows)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Invalid trigger metadata")
			return
		}
		items = append(items, value)
	}
	if rows.Err() != nil {
		fail(w, r, 503, "metadata_unavailable", "Trigger list incomplete")
		return
	}
	var next any
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	jsonResponse(w, 200, record{"items": items, "next_after": next, "limit": limit, "scheduling_enabled": s.functionSchedulesEnabled()})
}

func writeTriggerReceipt(w http.ResponseWriter, value triggerMutationReceipt) {
	w.Header().Set("ETag", `"`+value.Version+`"`)
	if value.Status == 204 {
		w.WriteHeader(204)
		return
	}
	jsonResponse(w, value.Status, record{"trigger": value.Trigger})
}

func triggerVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	tag := r.Header.Get("If-Match")
	if tag == "" {
		fail(w, r, 428, "precondition_required", "Current quoted trigger version required")
		return 0, false
	}
	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		fail(w, r, 422, "invalid_version", "Quoted trigger generation required")
		return 0, false
	}
	value, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
	if err != nil || value < 1 || value == 9223372036854775807 || strconv.FormatInt(value, 10) != tag[1:len(tag)-1] {
		fail(w, r, 422, "invalid_version", "Invalid trigger generation")
		return 0, false
	}
	return value, true
}

// A row lock serializes API configuration and the scheduler's occurrence
// snapshot. The immutable queued snapshot is not rewritten by a later PATCH.
func (s *server) mutateFunctionTrigger(w http.ResponseWriter, r *http.Request) {
	project, branch, id, actor := r.PathValue("project"), r.PathValue("branch"), r.PathValue("trigger"), userFrom(r).ID
	var input functionTriggerInput
	if r.Method != http.MethodDelete && (readJSON(r, &input) != nil || input.Type != "schedule") {
		fail(w, r, 422, "invalid_trigger", "Strict schedule trigger input required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "idempotency_unavailable", "Durable trigger admission unavailable")
		return
	}
	expected := int64(0)
	if id != "" {
		if !functionTriggerID.MatchString(id) {
			fail(w, r, 404, "not_found", "Trigger not found")
			return
		}
		expected, ok = triggerVersion(w, r)
		if !ok {
			return
		}
	}
	hash := s.createRequestHash("function-trigger-input", record{"method": r.Method, "project": project, "branch": branch, "id": id, "version": expected, "input": input})
	keyHash := s.createRequestHash("function-trigger-key", key)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Trigger transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "function-trigger:"+actor+":"+project+":"+branch+":"+keyHash)
	var org string
	var now time.Time
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT clock_timestamp()`).Scan(&now)
	}
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT p.org_id FROM branches b JOIN projects p ON p.id=b.project_id AND p.state='ready' AND p.deleted_at IS NULL WHERE b.id=$1 AND p.id=$2 AND b.state='ready' AND b.deleted_at IS NULL FOR UPDATE OF b`, branch, project).Scan(&org)
	}
	if isNoRows(err) {
		fail(w, r, 409, "branch_not_ready", "Ready branch required")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock trigger branch")
		return
	}
	var previous string
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM function_trigger_requests WHERE actor_id=$1 AND project_id=$2 AND branch_id=$3 AND key_hash=$4`, actor, project, branch, keyHash).Scan(&previous, &raw)
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key used with different trigger input")
			return
		}
		var receipt triggerMutationReceipt
		if json.Unmarshal(raw, &receipt) != nil {
			fail(w, r, 503, "metadata_unavailable", "Invalid trigger receipt")
			return
		}
		writeTriggerReceipt(w, receipt)
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read trigger receipt")
		return
	}
	value := functionTriggerView{Type: "schedule", Path: "/", Enabled: true}
	var functionID string
	if id != "" {
		value, err = scanFunctionTrigger(tx.QueryRow(r.Context(), `SELECT `+functionTriggerColumns+functionTriggerFrom+` AND t.id=$3 AND t.state='ready' FOR UPDATE OF t`, project, branch, id))
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Trigger not found")
			return
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not lock trigger")
			return
		}
		if value.Version != strconv.FormatInt(expected, 10) {
			fail(w, r, 412, "trigger_version_conflict", "Reload current trigger configuration")
			return
		}
	}
	originalSlug := value.Slug
	if input.Name != nil {
		value.Name = *input.Name
	}
	if input.Slug != nil {
		value.Slug = *input.Slug
	}
	if input.Path != nil {
		value.Path = *input.Path
	}
	if input.Enabled != nil {
		value.Enabled = *input.Enabled
	}
	if input.Schedule != nil {
		value.Schedule = *input.Schedule
	}
	if r.Method != http.MethodDelete {
		if !triggers.ValidName(value.Name) || !functions.ValidSlug(value.Slug) || !triggers.ValidPath(value.Path) {
			fail(w, r, 422, "invalid_trigger", "Valid name, target slug and application path required")
			return
		}
		if _, err = triggers.ParseCron(value.Schedule.Cron); err != nil {
			fail(w, r, 422, "invalid_cron", "Five-field numeric UTC cron required")
			return
		}
		value.Schedule.Cron = strings.Join(strings.Fields(value.Schedule.Cron), " ")
		if value.Enabled && !s.functionSchedulesEnabled() {
			fail(w, r, 503, "function_schedules_disabled", "Operator must enable schedule execution")
			return
		}
		// Disabling an existing unchanged target must remain possible while its
		// runtime is degraded. Creation/retarget/enable still require deployment.
		requireDeployed := id == "" || value.Enabled || value.Slug != originalSlug
		err = tx.QueryRow(r.Context(), `SELECT id FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state NOT IN ('deleting','deleted') AND (NOT $4 OR (state IN ('active','suspended') AND active_deployment_id IS NOT NULL))`, project, branch, value.Slug, requireDeployed).Scan(&functionID)
		if isNoRows(err) {
			fail(w, r, 409, "function_not_deployed", "Deploy target Function before scheduling")
			return
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not check trigger target")
			return
		}
		if value.Enabled {
			identity, e := readFunctionHTTPIdentity(r.Context(), tx, branch, value.Slug)
			if e != nil || identity.ID != functionID {
				fail(w, r, 409, "function_not_exposed", "Enabled schedule requires a currently authorized HTTP Function")
				return
			}
		}
	}
	var updates int
	if tx.QueryRow(r.Context(), `SELECT count(*) FROM function_trigger_requests WHERE actor_id=$1 AND project_id=$2 AND branch_id=$3 AND created_at>now()-interval '1 hour'`, actor, project, branch).Scan(&updates) != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check trigger rate")
		return
	}
	if updates >= 100 {
		w.Header().Set("Retry-After", "3600")
		fail(w, r, 429, "trigger_rate_limited", "Retry later")
		return
	}
	receipt := triggerMutationReceipt{Status: 200}
	if id == "" {
		if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(794210063)`); err == nil {
			err = tx.QueryRow(r.Context(), `SELECT count(*) FROM function_triggers WHERE state='ready'`).Scan(&updates)
		}
		var branchCount int
		if err == nil {
			err = tx.QueryRow(r.Context(), `SELECT count(*) FROM function_triggers WHERE branch_id=$1 AND state='ready'`, branch).Scan(&branchCount)
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not check trigger budgets")
			return
		}
		if updates >= 128 || branchCount >= 16 {
			fail(w, r, 409, "trigger_budget_exhausted", "Preview trigger budget exhausted")
			return
		}
		id = "ftr_" + randomToken(12)
		_, err = tx.Exec(r.Context(), `INSERT INTO function_triggers(branch_id,id,project_id,org_id,function_id,actor_id,origin_branch_id,type,name,function_path,cron,enabled,next_run_at) VALUES($1,$2,$3,$4,$5,$6,$1,'schedule',$7,$8,$9,$10,$11)`, branch, id, project, org, functionID, actor, value.Name, value.Path, value.Schedule.Cron, value.Enabled, triggerNext(value, now))
		receipt.Status = 201
	} else if r.Method == http.MethodDelete {
		err = tx.QueryRow(r.Context(), `UPDATE function_triggers SET state='deleted',enabled=false,next_run_at=NULL,generation=generation+1,deleted_at=clock_timestamp(),updated_at=clock_timestamp() WHERE branch_id=$1 AND id=$2 RETURNING generation::text`, branch, id).Scan(&receipt.Version)
		receipt.Status = 204
	} else {
		_, err = tx.Exec(r.Context(), `UPDATE function_triggers SET function_id=$3,actor_id=$4,origin_branch_id=branch_id,name=$5,function_path=$6,cron=$7,enabled=$8,next_run_at=$9,generation=generation+1,updated_at=clock_timestamp() WHERE branch_id=$1 AND id=$2`, branch, id, functionID, actor, value.Name, value.Path, value.Schedule.Cron, value.Enabled, triggerNext(value, now))
	}
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			fail(w, r, 409, "trigger_name_conflict", "Trigger name already exists on this branch")
		} else {
			fail(w, r, 503, "trigger_outcome_unconfirmed", "Trigger change unconfirmed; inspect current state or replay original key/input")
		}
		return
	}
	if receipt.Status != 204 {
		value, err = scanFunctionTrigger(tx.QueryRow(r.Context(), `SELECT `+functionTriggerColumns+functionTriggerFrom+` AND t.id=$3`, project, branch, id))
		receipt.Trigger = &value
		receipt.Version = value.Version
	}
	raw, _ = json.Marshal(receipt)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO function_trigger_requests(actor_id,project_id,branch_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4,$5,$6)`, actor, project, branch, keyHash, hash, raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,$2,'function_trigger',$3,'succeeded',$4)`, actor, map[string]string{http.MethodPost: "create_function_trigger", http.MethodPatch: "update_function_trigger", http.MethodDelete: "delete_function_trigger"}[r.Method], id, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "trigger_outcome_unconfirmed", "Trigger change unconfirmed; inspect current state or replay original key/input")
		return
	}
	writeTriggerReceipt(w, receipt)
}

func triggerNext(value functionTriggerView, now time.Time) *time.Time {
	if !value.Enabled {
		return nil
	}
	cron, err := triggers.ParseCron(value.Schedule.Cron)
	if err != nil {
		return nil
	}
	next, err := cron.Next(now)
	if err != nil {
		return nil
	}
	return &next
}

type triggerInvocationCursor struct {
	Branch  string    `json:"branch"`
	Trigger string    `json:"trigger"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
}

func (s *server) functionTriggerInvocations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.functionReadBranch(w, r) {
		return
	}
	project, branch, id := r.PathValue("project"), r.PathValue("branch"), r.PathValue("trigger")
	var exists bool
	if s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM function_triggers WHERE project_id=$1 AND branch_id=$2 AND id=$3)`, project, branch, id).Scan(&exists) != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read trigger")
		return
	}
	if !exists {
		fail(w, r, 404, "not_found", "Trigger not found")
		return
	}
	limit, ok := functionReadLimit(w, r)
	if !ok {
		return
	}
	cursor := triggerInvocationCursor{Branch: branch, Trigger: id, At: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: "~"}
	if raw := r.URL.Query().Get("after"); raw != "" {
		body, err := base64.RawURLEncoding.DecodeString(raw)
		if len(raw) > 1024 || err != nil || json.Unmarshal(body, &cursor) != nil || cursor.Branch != branch || cursor.Trigger != id || cursor.At.IsZero() || !functions.ValidTriggerInvocationID(cursor.ID) {
			fail(w, r, 422, "invalid_cursor", "Invocation cursor belongs to another trigger")
			return
		}
	}
	items, err := s.many(r.Context(), `SELECT id,trigger_id,trigger_generation::text AS trigger_version_token,scheduled_at,state,attempts,request_event_id,application_status,response_bytes,error_code,admitted_at,started_at,finished_at FROM function_trigger_invocations WHERE project_id=$1 AND branch_id=$2 AND trigger_id=$3 AND (scheduled_at,id)<($4,$5) ORDER BY scheduled_at DESC,id DESC LIMIT $6`, project, branch, id, cursor.At, cursor.ID, limit+1)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Invocation journal unavailable")
		return
	}
	var next any
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		raw, _ := json.Marshal(triggerInvocationCursor{Branch: branch, Trigger: id, At: last["scheduled_at"].(time.Time), ID: stringVal(last["id"])})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	jsonResponse(w, 200, record{"items": items, "next_after": next, "limit": limit, "capture": "occurrence_and_dispatch_metadata_only", "retention": "held_until_operator_policy"})
}
