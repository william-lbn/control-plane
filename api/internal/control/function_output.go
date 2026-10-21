package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"
)

func (s *server) functionOutputEnabled() bool {
	return s.functions != nil && os.Getenv("NEON_FUNCTION_OUTPUT_ENABLED") == "true"
}

type functionOutputPolicy struct {
	Enabled   bool       `json:"enabled"`
	Version   string     `json:"version_token"`
	Since     *time.Time `json:"capture_since"`
	Retention int        `json:"retention_days"`
	LastError string     `json:"last_error_code,omitempty"`
	Checked   *time.Time `json:"last_checked_at,omitempty"`
}

func (s *server) outputFunctionID(ctx context.Context, project, branch, slug string) (string, error) {
	var id string
	err := s.db.QueryRow(ctx, `SELECT id FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted'`, project, branch, slug).Scan(&id)
	return id, err
}
func (s *server) readOutputPolicy(ctx context.Context, id string) (functionOutputPolicy, error) {
	out := functionOutputPolicy{Version: "0", Retention: 3}
	err := s.db.QueryRow(ctx, `SELECT enabled,version::text,capture_since,COALESCE(last_error_code,''),last_checked_at FROM function_output_policies WHERE function_id=$1`, id).Scan(&out.Enabled, &out.Version, &out.Since, &out.LastError, &out.Checked)
	if isNoRows(err) {
		err = nil
	}
	return out, err
}
func (s *server) functionOutputPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.functionReadBranch(w, r) {
		return
	}
	project, branch, slug := r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug")
	id, err := s.outputFunctionID(r.Context(), project, branch, slug)
	if err != nil {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	if r.Method == http.MethodGet {
		out, e := s.readOutputPolicy(r.Context(), id)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Output policy unavailable")
			return
		}
		w.Header().Set("ETag", "\""+out.Version+"\"")
		jsonResponse(w, 200, record{"policy": out, "collection_enabled": s.functionOutputEnabled(), "capture": "explicit_acknowledged_customer_output", "limits": record{"line_bytes": 4096, "guest_records": 512, "function_records": 10000}})
		return
	}
	var input struct {
		Enabled     *bool `json:"enabled"`
		Acknowledge bool  `json:"acknowledge_sensitive_output"`
	}
	if readJSON(r, &input) != nil || input.Enabled == nil || (*input.Enabled && !input.Acknowledge) {
		fail(w, r, 422, "output_acknowledgment_required", "Output may contain sensitive application data; explicitly acknowledge shared project access and three-day retention")
		return
	}
	if *input.Enabled && !s.functionOutputEnabled() {
		fail(w, r, 503, "function_output_disabled", "Operator must enable customer output collection")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	expected := r.Header.Get("If-Match")
	if expected == "" {
		fail(w, r, 428, "version_required", "Current quoted output policy version required")
		return
	}
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "idempotency_unavailable", "Durable output admission unavailable")
		return
	}
	actor := userFrom(r).ID
	hash := s.createRequestHash("function-output-policy", record{"function": id, "input": input, "version": expected})
	keyHash := s.createRequestHash("function-output-policy-key", key)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Output admission unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var locked string
	if tx.QueryRow(r.Context(), `SELECT id FROM function_definitions WHERE id=$1 AND state NOT IN ('deleted','deleting') FOR UPDATE`, id).Scan(&locked) != nil {
		fail(w, r, 409, "function_not_ready", "Function unavailable for configuration")
		return
	}
	var previous string
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM function_output_policy_requests WHERE actor_id=$1 AND function_id=$2 AND key_hash=$3`, actor, id, keyHash).Scan(&previous, &raw)
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key belongs to another policy change")
			return
		}
		var out functionOutputPolicy
		if json.Unmarshal(raw, &out) != nil {
			fail(w, r, 503, "metadata_unavailable", "Retained policy receipt invalid")
			return
		}
		w.Header().Set("ETag", "\""+out.Version+"\"")
		jsonResponse(w, 200, out)
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Output receipt unavailable")
		return
	}
	var version int64
	err = tx.QueryRow(r.Context(), `SELECT version FROM function_output_policies WHERE function_id=$1 FOR UPDATE`, id).Scan(&version)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Output policy unavailable")
		return
	}
	if expected != "\""+strconv.FormatInt(version, 10)+"\"" || version == int64(^uint64(0)>>1) {
		fail(w, r, 412, "output_version_conflict", "Reload current policy")
		return
	}
	out := functionOutputPolicy{Enabled: *input.Enabled, Version: strconv.FormatInt(version+1, 10), Retention: 3}
	var since time.Time
	err = tx.QueryRow(r.Context(), `INSERT INTO function_output_policies(function_id,project_id,branch_id,enabled,version,capture_since,actor_id) VALUES($1,$2,$3,$4,$5,clock_timestamp(),$6) ON CONFLICT(function_id) DO UPDATE SET enabled=$4,version=$5,capture_since=clock_timestamp(),actor_id=$6,updated_at=clock_timestamp() RETURNING capture_since`, id, project, branch, *input.Enabled, version+1, actor).Scan(&since)
	out.Since = &since
	raw, _ = json.Marshal(out)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO function_output_policy_requests(actor_id,function_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4,$5)`, actor, id, keyHash, hash, raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'configure_function_output','function',$2,'succeeded',$3)`, actor, id, requestID(r))
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		fail(w, r, 503, "output_policy_unconfirmed", "Inspect current policy or replay exact key/input")
		return
	}
	w.Header().Set("ETag", "\""+out.Version+"\"")
	jsonResponse(w, 200, out)
}
func (s *server) outputAEAD() (cipher.AEAD, error) {
	if s.functions == nil || len(s.functions.key) != 32 {
		return nil, errors.New("output encryption key unavailable")
	}
	mac := hmac.New(sha256.New, s.functions.key)
	mac.Write([]byte("neon.function.output.encryption.v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func outputAAD(fn, instance, boot string, seq int64) []byte {
	return []byte("neon.function.output.v1\x00" + fn + "\x00" + instance + "\x00" + boot + "\x00" + strconv.FormatInt(seq, 10))
}
func (s *server) sealFunctionOutput(fn, instance, boot string, seq int64, message string) ([]byte, error) {
	a, e := s.outputAEAD()
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return a.Seal(nonce, nonce, []byte(message), outputAAD(fn, instance, boot, seq)), nil
}
func (s *server) openFunctionOutput(fn, instance, boot string, seq int64, data []byte) (string, error) {
	a, e := s.outputAEAD()
	if e != nil || len(data) < 28 {
		return "", errors.New("output ciphertext unavailable")
	}
	v, e := a.Open(nil, data[:a.NonceSize()], data[a.NonceSize():], outputAAD(fn, instance, boot, seq))
	return string(v), e
}

type outputHistoryCursor struct {
	Function string    `json:"function"`
	At       time.Time `json:"at"`
	Instance string    `json:"instance"`
	Sequence int64     `json:"sequence"`
}

func (s *server) functionOutputLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.functionReadBranch(w, r) {
		return
	}
	id, err := s.outputFunctionID(r.Context(), r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug"))
	if err != nil {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	limit, ok := functionReadLimit(w, r)
	if !ok {
		return
	}
	level := r.URL.Query().Get("severity")
	if level != "" && level != "DEBUG" && level != "INFO" && level != "WARN" && level != "ERROR" {
		fail(w, r, 422, "invalid_severity", "Known severity required")
		return
	}
	cursor := outputHistoryCursor{id, time.Now().Add(time.Hour), "~", 1 << 62}
	if raw := r.URL.Query().Get("after"); raw != "" {
		body, e := base64.RawURLEncoding.DecodeString(raw)
		if e != nil || len(raw) > 1024 || json.Unmarshal(body, &cursor) != nil || cursor.Function != id || cursor.At.IsZero() || cursor.Sequence < 1 {
			fail(w, r, 422, "invalid_cursor", "Cursor must belong to this Function")
			return
		}
	}
	rows, err := s.db.Query(r.Context(), `SELECT instance_id,boot_id,sequence,deployment_id,generation,emitted_at,captured_at,stream,severity,scope_name,truncated,ciphertext FROM function_output_records WHERE function_id=$1 AND project_id=$2 AND branch_id=$3 AND expires_at>now() AND ($4='' OR severity=$4) AND (captured_at,instance_id,sequence)<($5,$6,$7) ORDER BY captured_at DESC,instance_id DESC,sequence DESC LIMIT $8`, id, r.PathValue("project"), r.PathValue("branch"), level, cursor.At, cursor.Instance, cursor.Sequence, limit+1)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Output history unavailable")
		return
	}
	defer rows.Close()
	items := []record{}
	next := ""
	for rows.Next() {
		var instance, boot, deployment, stream, severity, scope string
		var seq, generation int64
		var emitted, captured time.Time
		var truncated bool
		var encrypted []byte
		if rows.Scan(&instance, &boot, &seq, &deployment, &generation, &emitted, &captured, &stream, &severity, &scope, &truncated, &encrypted) != nil {
			fail(w, r, 503, "metadata_unavailable", "Output record unavailable")
			return
		}
		if len(items) == limit {
			last := items[len(items)-1]
			body, _ := json.Marshal(outputHistoryCursor{id, last["captured_at"].(time.Time), stringVal(last["instance_id"]), last["sequence"].(int64)})
			next = base64.RawURLEncoding.EncodeToString(body)
			break
		}
		message, e := s.openFunctionOutput(id, instance, boot, seq, encrypted)
		if e != nil {
			fail(w, r, 503, "output_decryption_unavailable", "Output key or record verification unavailable")
			return
		}
		items = append(items, record{"instance_id": instance, "boot_id": boot, "sequence": seq, "deployment_id": deployment, "generation_token": strconv.FormatInt(generation, 10), "timestamp": emitted, "captured_at": captured, "stream": stream, "severity": severity, "scope_name": scope, "message": message, "truncated": truncated})
	}
	if rows.Err() != nil {
		fail(w, r, 503, "metadata_unavailable", "Output page unavailable")
		return
	}
	rows.Close()
	var dropped int64
	if s.db.QueryRow(r.Context(), `SELECT COALESCE(sum(c.dropped_lines),0) FROM function_output_cursors c JOIN function_instances i ON i.id=c.instance_id WHERE i.function_id=$1`, id).Scan(&dropped) != nil {
		fail(w, r, 503, "metadata_unavailable", "Output gap accounting unavailable")
		return
	}
	jsonResponse(w, 200, record{"items": items, "next_after": next, "function_id": id, "retention_days": 3, "guest_dropped_lines": dropped, "record_limit": 10000, "capture": "acknowledged_customer_output_not_trusted_platform_attestation", "redaction": "exact_known_environment_and_database_secrets_best_effort"})
}
