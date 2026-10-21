package control

import (
	"bytes"
	"context"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

type functionHTTPIdentity struct {
	ID, Project, Branch, Slug, State, Actor string
	Version                                 int64
	Idle, IdleSuspended                     bool
	Timeout                                 int
}
type functionAutomaticStopKey struct{}
type functionExpectedIdentityKey struct{}

func (s *server) functionHTTPIdentity(ctx context.Context, branch, slug string) (functionHTTPIdentity, error) {
	return readFunctionHTTPIdentity(ctx, s.db, branch, slug)
}

type functionIdentityReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// The same reader can be a pool or an already-held transaction. Never borrow
// a second connection while holding branch/configuration locks: a saturated
// pool would otherwise deadlock enabled schedule admission.
func readFunctionHTTPIdentity(ctx context.Context, db functionIdentityReader, branch, slug string) (functionHTTPIdentity, error) {
	var out functionHTTPIdentity
	var orgRole, projectRole string
	e := db.QueryRow(ctx, `SELECT f.id,f.project_id,f.branch_id,f.slug,f.state,f.version,h.actor_id,h.idle_enabled,h.idle_suspended,f.idle_timeout_seconds,m.role,COALESCE(g.role,'')
 FROM function_definitions f JOIN function_http_policies h ON h.function_id=f.id
 JOIN projects p ON p.id=f.project_id JOIN branches b ON b.id=f.branch_id JOIN organizations o ON o.id=f.org_id JOIN users u ON u.id=h.actor_id
 JOIN organization_members m ON m.org_id=f.org_id AND m.user_id=h.actor_id
 LEFT JOIN project_grants g ON g.project_id=f.project_id AND g.user_id=h.actor_id
 WHERE f.branch_id=$1 AND f.slug=$2 AND h.public_enabled AND f.state NOT IN ('deleting','deleted')
 AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active' AND NOT u.disabled`, branch, slug).Scan(&out.ID, &out.Project, &out.Branch, &out.Slug, &out.State, &out.Version, &out.Actor, &out.Idle, &out.IdleSuspended, &out.Timeout, &orgRole, &projectRole)
	if e == nil && effectivePermission(orgRole, projectRole) < 2 {
		e = errBackendCredentialInvalid
	}
	return out, e
}

// Minimal private response collector for reuse of the validated durable intent
// admission. It never forwards internal operation IDs/errors to anonymous users.
type functionIntentResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *functionIntentResponse) Header() http.Header { return w.header }
func (w *functionIntentResponse) WriteHeader(n int) {
	if w.status == 0 {
		w.status = n
	}
}
func (w *functionIntentResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}
func (s *server) automaticFunctionIntent(ctx context.Context, p functionHTTPIdentity, resume bool) bool {
	action, body := "suspend", `{"acknowledge_background_work_stop":true}`
	if resume {
		action = "resume"
		body = `{}`
	}
	ctx = context.WithValue(ctx, functionExpectedIdentityKey{}, p.ID)
	r, _ := http.NewRequestWithContext(context.WithValue(context.WithValue(ctx, userKey, user{ID: p.Actor}), requestIDKey, newID("function_auto_")), http.MethodPost, "http://internal/api/v1/projects/"+p.Project+"/branches/"+p.Branch+"/functions/"+p.Slug+"/"+action, strings.NewReader(body))
	r.SetPathValue("project", p.Project)
	r.SetPathValue("branch", p.Branch)
	r.SetPathValue("slug", p.Slug)
	if !resume {
		r = r.WithContext(context.WithValue(r.Context(), functionAutomaticStopKey{}, true))
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("If-Match", strconv.FormatInt(p.Version, 10))
	r.Header.Set("Idempotency-Key", "function-auto-"+action+"-"+p.ID+"-"+strconv.FormatInt(p.Version, 10))
	w := &functionIntentResponse{header: make(http.Header)}
	if resume {
		s.functionResume(w, r)
	} else {
		s.functionStop(w, r)
	}
	return w.status == http.StatusAccepted
}

func (s *server) awaitPublicFunction(ctx context.Context, branch, slug string) (functionHTTPIdentity, error) {
	return s.awaitFunctionIdentity(ctx, branch, slug, "")
}

// A durable trigger addresses an immutable Function ID as well as its slug.
// A later replacement at the same URL must not be woken or adopted by it.
func (s *server) awaitFunctionIdentity(ctx context.Context, branch, slug, expectedID string) (functionHTTPIdentity, error) {
	deadline := time.NewTimer(180 * time.Second)
	defer deadline.Stop()
	for {
		p, e := s.functionHTTPIdentity(ctx, branch, slug)
		if e != nil {
			return p, e
		}
		if expectedID != "" && p.ID != expectedID {
			return p, errFunctionInstanceConflict
		}
		if p.State == "active" {
			return p, nil
		}
		if p.State == "suspended" && p.Idle && p.IdleSuspended {
			release, ok := s.functionAdmissionLock(ctx, p.ID, true)
			if ok {
				fresh, err := s.functionHTTPIdentity(ctx, branch, slug)
				if err == nil && fresh.ID == p.ID && fresh.State == "suspended" && fresh.Idle && fresh.IdleSuspended {
					s.automaticFunctionIntent(ctx, fresh, true)
				}
				release()
			}
		} else if p.State != "provisioning" {
			return p, errFunctionInstanceConflict
		}
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-deadline.C:
			return p, errFunctionInstanceConflict
		case <-time.After(time.Second):
		}
	}
}

func functionIngressHeaders(in http.Header) map[string][]string {
	out := map[string][]string{}
	for name, values := range in {
		lower := strings.ToLower(name)
		switch lower {
		case "cookie", "host", "connection", "keep-alive", "proxy-authorization", "proxy-authenticate", "te", "trailer", "transfer-encoding", "upgrade", "content-length", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto":
			continue
		}
		if strings.HasPrefix(lower, "x-neon-") || strings.HasPrefix(lower, "x-function-") {
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}
func copyFunctionPublicHeaders(out, in http.Header) {
	for name, values := range functionIngressHeaders(in) {
		switch strings.ToLower(name) {
		case "set-cookie", "clear-site-data", "service-worker-allowed", "strict-transport-security":
			continue
		}
		out[name] = values
	}
	out.Set("Cache-Control", "no-store")
	out.Set("X-Content-Type-Options", "nosniff")
}

func (s *server) functionPublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/functions/v1/{branch}/{slug}", s.functionPublicInvoke)
	mux.HandleFunc("/functions/v1/{branch}/{slug}/{rest...}", s.functionPublicInvoke)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin, _ := url.Parse(functionPublicOrigin())
		if origin == nil || !strings.EqualFold(r.Host, origin.Host) {
			http.Error(w, "Functions authority rejected", 421)
			return
		}
		id := newID("fhr_")
		w.Header().Set("X-Request-ID", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		// Never mount Console auth, API, static files or a cookie-based session here.
		mux.ServeHTTP(w, r)
	})
}
func (s *server) functionPublicInvoke(w http.ResponseWriter, r *http.Request) {
	if !functions.ValidSlug(r.PathValue("slug")) {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	websocket := r.Header.Get("Upgrade") != ""
	if websocket && (!functions.ValidateWebSocketHandshake(r.Method, r.Header) || r.ContentLength > 0 || len(r.TransferEncoding) > 0) {
		fail(w, r, 400, "invalid_websocket_handshake", "A standard GET WebSocket handshake is required")
		return
	}
	body, e := io.ReadAll(io.LimitReader(r.Body, functions.MaxInvokeBytes+1))
	if e != nil || len(body) > functions.MaxInvokeBytes {
		fail(w, r, 413, "invocation_too_large", "Function request exceeds one MiB")
		return
	}
	// Validate before a possible wake so malformed requests allocate no VM.
	headers := functionIngressHeaders(r.Header)
	probe := functions.Invocation{BootID: strings.Repeat("0", 32), Generation: 1, Method: r.Method, URL: functionPublicOrigin() + r.URL.RequestURI(), Headers: headers, Body: body}
	if !functions.ValidateInvocation(functions.Scope{BranchID: r.PathValue("branch"), Slug: r.PathValue("slug"), Generation: 1}, probe) {
		fail(w, r, 422, "invalid_invocation", "Method/path/headers rejected before wake")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	p, e := s.awaitPublicFunction(ctx, r.PathValue("branch"), r.PathValue("slug"))
	if e != nil {
		fail(w, r, 503, "function_unavailable", "Function not available; request has not been dispatched")
		return
	}
	release, ok := s.functionAdmissionLock(ctx, p.ID, false)
	if !ok {
		w.Header().Set("Retry-After", "1")
		fail(w, r, 503, "function_admission_busy", "Request has not been dispatched")
		return
	}
	defer release()
	// Revalidate exposure and parent/actor permissions after cold wake and lock.
	p, e = s.functionHTTPIdentity(ctx, p.Branch, p.Slug)
	if e != nil || p.State != "active" {
		fail(w, r, 503, "function_unavailable", "Function admission changed")
		return
	}
	runtime, e := s.loadPublishedFunction(ctx, p.Project, p.Branch, p.Slug)
	if e != nil {
		failFunctionObservation(w, r, e)
		return
	}
	invocation := functions.Invocation{BootID: runtime.bootID, Generation: runtime.target.Generation, Method: r.Method, URL: functionPublicOrigin() + r.URL.RequestURI(), Headers: headers, Body: body}
	if !functions.ValidateInvocation(runtime.instance.Scope, invocation) {
		fail(w, r, 422, "invalid_invocation", "Method/path/headers rejected before dispatch")
		return
	}
	client, status, e := s.observePublishedFunction(ctx, runtime)
	if e != nil {
		failFunctionObservation(w, r, e)
		return
	}
	defer client.Close()
	if !status.NodeReady || status.Draining {
		fail(w, r, 503, "function_unavailable", "Runtime not accepting requests")
		return
	}
	if websocket {
		capabilities, err := client.Capabilities(ctx)
		if err != nil || capabilities.BootID != runtime.bootID || !capabilities.WebSockets {
			fail(w, r, 503, "function_websocket_runtime_unavailable", "Publish to a WebSocket capable runtime before connecting")
			return
		}
	}
	event, err := s.beginFunctionEvent(ctx, r, runtime.target, runtime.instance.Scope.InstanceID, "public_http", invocation.Method)
	if err != nil {
		fail(w, r, 503, "function_journal_unavailable", "Request not dispatched; admission journal unavailable")
		return
	}
	finalized := false
	finish := func(status int, bytes int64, outcome string) {
		if !finalized {
			finalized = true
			s.finishFunctionEvent(event, status, bytes, outcome)
		}
	}
	defer finish(0, 0, "outcome_unknown")
	if websocket {
		response, err := client.WebSocket(ctx, invocation)
		if err != nil {
			fail(w, r, 502, "function_websocket_rejected", "WebSocket handshake unavailable; no automatic replay")
			return
		}
		defer response.Body.Close()
		if response.StatusCode != 101 {
			copyFunctionPublicHeaders(w.Header(), response.Header)
			w.WriteHeader(response.StatusCode)
			n, _ := io.Copy(w, io.LimitReader(response.Body, 4096))
			finish(response.StatusCode, n, "completed")
			return
		}
		n, err := functions.RelayWebSocket(ctx, w, response)
		outcome := "completed"
		if err != nil {
			outcome = "client_disconnected"
		}
		finish(101, n, outcome)
		updateCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		s.db.Exec(updateCtx, `UPDATE function_instances SET last_invoked_at=now(),updated_at=now() WHERE id=$1 AND boot_id=$2`, runtime.instance.Scope.InstanceID, runtime.bootID)
		return
	}
	response, e := client.Invoke(ctx, invocation)
	if e != nil {
		fail(w, r, 502, "function_invocation_outcome_unknown", "Invocation outcome unknown; no automatic replay")
		return
	}
	defer response.Body.Close()
	copyFunctionPublicHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	buffer := make([]byte, 16<<10)
	var forwarded int64
	outcome := "completed"
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			written, writeErr := w.Write(buffer[:n])
			forwarded += int64(written)
			if writeErr != nil {
				outcome = "client_disconnected"
				break
			}
			if controller.Flush() != nil {
				outcome = "client_disconnected"
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				outcome = "response_incomplete"
			}
			break
		}
	}
	finish(response.StatusCode, forwarded, outcome)
	// The supervisor owns authoritative end-of-stream activity. Metadata is an
	// additional conservative timestamp, never a replacement for that observation.
	updateCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	s.db.Exec(updateCtx, `UPDATE function_instances SET last_invoked_at=now(),updated_at=now() WHERE id=$1 AND boot_id=$2`, runtime.instance.Scope.InstanceID, runtime.bootID)
}

func (s *server) runFunctionIdleController(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	cursor := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cursor = s.suspendIdleFunctions(ctx, cursor)
		}
	}
}

// The supervisor, bound to the original boot and resource identity, owns these
// counters. Customer Node health is not a prerequisite for releasing an idle
// instance: a crashed Node must not keep an otherwise idle VM forever.
func functionIdleEligible(status functions.InstanceStatus, timeout int, now time.Time) bool {
	return timeout >= 30 && timeout <= 3600 && !status.Draining && status.ActiveInvocations == 0 && !status.LastActivity.IsZero() && !status.BackgroundCountersTrusted && now.Sub(status.LastActivity) >= time.Duration(timeout)*time.Second
}

func (s *server) functionIdleCandidates(ctx context.Context, after string) ([]record, error) {
	return s.many(ctx, `SELECT f.id,f.branch_id,f.slug FROM function_definitions f JOIN function_http_policies h ON h.function_id=f.id WHERE f.id>$1 AND f.state='active' AND h.public_enabled AND h.idle_enabled ORDER BY f.id LIMIT 4`, after)
}

// Keyset traversal gives later functions a turn even when the first batch stays
// busy or cannot be observed. Cursor is only a local observation optimization;
// durable stop admission, authorization and leases remain in PostgreSQL.
func (s *server) suspendIdleFunctions(ctx context.Context, cursor string) string {
	rows, e := s.functionIdleCandidates(ctx, cursor)
	if e != nil {
		return cursor
	}
	if len(rows) == 0 {
		return ""
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return cursor
		}
		cursor = stringVal(row["id"])
		callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		p, err := s.functionHTTPIdentity(callCtx, stringVal(row["branch_id"]), stringVal(row["slug"]))
		if err != nil || p.ID != cursor {
			cancel()
			continue
		}
		release, ok := s.functionAdmissionLock(callCtx, p.ID, true)
		if !ok {
			cancel()
			continue
		}
		fresh, err := s.functionHTTPIdentity(callCtx, p.Branch, p.Slug)
		if err == nil && fresh.ID == p.ID && fresh.State == "active" && fresh.Idle {
			runtime, e := s.loadPublishedFunction(callCtx, p.Project, p.Branch, p.Slug)
			if e == nil && runtime.target.FunctionID == p.ID {
				client, status, e := s.observePublishedFunction(callCtx, runtime)
				if e == nil {
					if functionIdleEligible(status, fresh.Timeout, time.Now()) {
						s.automaticFunctionIntent(callCtx, fresh, false)
					}
					client.Close()
				}
			}
		}
		release()
		cancel()
	}
	return cursor
}
