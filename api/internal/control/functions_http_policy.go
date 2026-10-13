package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Different ports are different web origins, but cookies share a host. The
// local public listener therefore removes Cookie and Set-Cookie in both
// directions. A distinct production hostname and trusted TLS remain a gate.
func functionPublicOrigin() string {
	return strings.TrimRight(os.Getenv("NEON_FUNCTIONS_PUBLIC_ORIGIN"), "/")
}
func validateFunctionPublicOrigin() error {
	value := os.Getenv("NEON_FUNCTIONS_PUBLIC_ORIGIN")
	if value == "" {
		return nil
	}
	u, e := url.Parse(value)
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && os.Getenv("NEON_FUNCTIONS_PUBLIC_LAB_HTTP") == "true")) || value == strings.TrimRight(os.Getenv("NEON_AUTH_PUBLIC_ORIGIN"), "/") {
		return errFunctionsPlatform
	}
	return nil
}

func (s *server) functionHTTPPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.functionsConfigured(w, r) {
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	version, e := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	if e != nil || version < 1 {
		fail(w, r, 428, "version_required", "Function numeric If-Match required")
		return
	}
	var input struct {
		Public      bool `json:"public_enabled"`
		Idle        bool `json:"idle_enabled"`
		Timeout     int  `json:"idle_timeout_seconds"`
		Acknowledge bool `json:"acknowledge_public_access_and_background_eviction"`
	}
	if readJSON(r, &input) != nil || input.Timeout < 30 || input.Timeout > 3600 || input.Idle && !input.Public || (input.Public || input.Idle) && !input.Acknowledge {
		fail(w, r, 422, "invalid_function_http_policy", "Timeout 30..3600 and explicit public access/background eviction acknowledgement required")
		return
	}
	if input.Public && functionPublicOrigin() == "" {
		fail(w, r, 503, "function_public_unconfigured", "Operator must configure the separate Functions origin")
		return
	}
	canonical, _ := json.Marshal(record{"project": r.PathValue("project"), "branch": r.PathValue("branch"), "slug": r.PathValue("slug"), "version": version, "policy": input})
	fingerprint := s.functionFingerprint(canonical)
	tx, e := s.db.Begin(r.Context())
	if e != nil {
		fail(w, r, 503, "metadata_unavailable", "Policy unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var id, state string
	var current int64
	e = tx.QueryRow(r.Context(), `SELECT id,state,version FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted' FOR UPDATE`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug")).Scan(&id, &state, &current)
	if e != nil {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	var prior string
	var response []byte
	e = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM function_policy_requests WHERE actor_id=$1 AND key_hash=$2`, userFrom(r).ID, keyHash(key)).Scan(&prior, &response)
	if e == nil {
		if prior != fingerprint {
			fail(w, r, 409, "idempotency_conflict", "Policy key input changed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(response)
		return
	}
	if !isNoRows(e) {
		fail(w, r, 503, "metadata_unavailable", "Policy replay unavailable")
		return
	}
	if current != version {
		fail(w, r, 412, "version_conflict", "Refresh the Function before editing its policy")
		return
	}
	if state != "active" && state != "suspended" {
		fail(w, r, 409, "function_operation_in_progress", "Finish the original Function operation first")
		return
	}
	_, e = tx.Exec(r.Context(), `UPDATE function_definitions SET version=version+1,idle_timeout_seconds=$2,updated_at=now() WHERE id=$1`, id, input.Timeout)
	if e == nil {
		_, e = tx.Exec(r.Context(), `INSERT INTO function_http_policies(function_id,public_enabled,idle_enabled,actor_id) VALUES($1,$2,$3,$4) ON CONFLICT(function_id) DO UPDATE SET public_enabled=$2,idle_enabled=$3,actor_id=$4,updated_at=now()`, id, input.Public, input.Idle, userFrom(r).ID)
	}
	response, _ = json.Marshal(record{"function_id": id, "version_token": strconv.FormatInt(current+1, 10), "public_enabled": input.Public, "idle_enabled": input.Idle, "idle_timeout_seconds": input.Timeout})
	if e == nil {
		_, e = tx.Exec(r.Context(), `INSERT INTO function_policy_requests(actor_id,key_hash,request_hash,function_id,response) VALUES($1,$2,$3,$4,$5)`, userFrom(r).ID, keyHash(key), fingerprint, id, response)
	}
	if e == nil {
		_, e = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'set_function_http_policy','function',$2,'succeeded',$3)`, userFrom(r).ID, id, requestID(r))
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		fail(w, r, 503, "metadata_unavailable", "Policy transaction unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(response)
}

// Keep the physical session exclusively owned until invocation/stream ends.
// Every API instance uses this same shared key; idle admission takes exclusive.
// Close on unlock failure so the pool never transfers an advisory lock owner.
// This is a cooperative admission barrier, not an external guest epoch fence.
func (s *server) functionAdmissionLock(ctx context.Context, id string, exclusive bool) (func(), bool) {
	// A held lock consumes a pool connection while metadata checks need another.
	// Reserve query headroom rather than letting all requests hold the complete
	// pool and deadlock each other until their invocation deadlines expire.
	s.functionSlotsOnce.Do(func() {
		limit := max(1, min(16, int(s.db.Config().MaxConns-1)/2))
		s.functionSlots = make(chan struct{}, limit)
	})
	select {
	case s.functionSlots <- struct{}{}:
	default:
		return nil, false
	}
	acquired := false
	defer func() {
		if !acquired {
			<-s.functionSlots
		}
	}()
	connection, e := s.db.Acquire(ctx)
	if e != nil {
		return nil, false
	}
	sql := "SELECT pg_try_advisory_lock_shared(hashtextextended($1,794210079))"
	unlock := "SELECT pg_advisory_unlock_shared(hashtextextended($1,794210079))"
	if exclusive {
		sql = "SELECT pg_try_advisory_lock(hashtextextended($1,794210079))"
		unlock = "SELECT pg_advisory_unlock(hashtextextended($1,794210079))"
	}
	var locked bool
	if e = connection.QueryRow(ctx, sql, id).Scan(&locked); e != nil {
		connection.Conn().Close(context.Background())
		connection.Release()
		return nil, false
	}
	if !locked {
		connection.Release()
		return nil, false
	}
	acquired = true
	return func() {
		defer func() { <-s.functionSlots }()
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var released bool
		if connection.QueryRow(c, unlock, id).Scan(&released) != nil || !released {
			connection.Conn().Close(c)
		}
		connection.Release()
	}, true
}
