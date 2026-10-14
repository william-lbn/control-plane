package control

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// Console invocation is an authenticated JSON envelope. Customer HTML/JS and
// Set-Cookie are NEVER served as executable content on the Console origin.
// A public raw HTTP/SSE/WS ingress requires a separate origin/admission gate.
func (s *server) functionConsoleInvoke(w http.ResponseWriter, r *http.Request) {
	if !s.functionsConfigured(w, r) {
		return
	}
	var input struct {
		Method  string              `json:"method"`
		Path    string              `json:"path"`
		Headers map[string][]string `json:"headers"`
		Body    string              `json:"body"`
	}
	if readJSON(r, &input) != nil || len(input.Body) > functions.MaxInvokeBytes || !strings.HasPrefix(input.Path, "/") {
		fail(w, r, 422, "invalid_invocation", "Bounded method/path/body required")
		return
	}
	parsed, err := url.ParseRequestURI(input.Path)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Fragment != "" {
		fail(w, r, 422, "invalid_invocation", "An application-relative path is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	runtime, err := s.loadPublishedFunction(ctx, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug"))
	if err != nil {
		failFunctionObservation(w, r, err)
		return
	}
	p, instance, bootID := runtime.target, runtime.instance, runtime.bootID
	release, admitted := s.functionAdmissionLock(ctx, p.FunctionID, false)
	if !admitted {
		fail(w, r, 503, "function_admission_busy", "Function retirement admission busy; request not dispatched")
		return
	}
	defer release()
	if input.Headers == nil {
		input.Headers = map[string][]string{}
	}
	invocation := functions.Invocation{BootID: bootID, Generation: p.Generation, Method: input.Method,
		URL: "https://function.invalid/functions/v1/" + p.BranchID + "/" + p.Slug + input.Path, Headers: input.Headers, Body: []byte(input.Body)}
	if !functions.ValidateInvocation(instance.Scope, invocation) {
		fail(w, r, 422, "invalid_invocation", "Invocation method, path or headers rejected before dispatch")
		return
	}
	client, status, err := s.observePublishedFunction(ctx, runtime)
	if err != nil {
		failFunctionObservation(w, r, err)
		return
	}
	defer client.Close()
	if !status.NodeReady || status.Draining {
		fail(w, r, 503, "function_runtime_unavailable", "Original boot is not ready")
		return
	}
	event, e := s.beginFunctionEvent(ctx, r, p, instance.Scope.InstanceID, "console", invocation.Method)
	if e != nil {
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
	response, err := client.Invoke(ctx, invocation)
	if err != nil {
		fail(w, r, 502, "function_invocation_outcome_unknown", "Invocation outcome may be unknown; inspect before retrying")
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, functions.MaxInvokeBytes+1))
	if err != nil || len(body) > functions.MaxInvokeBytes {
		finish(response.StatusCode, int64(len(body)), "response_incomplete")
		fail(w, r, 502, "function_invocation_outcome_unknown", "Invocation response failed or exceeded limits; do not replay automatically")
		return
	}
	_, _ = s.db.Exec(ctx, `UPDATE function_instances SET last_invoked_at=now(),updated_at=now() WHERE id=$1 AND boot_id=$2`, instance.Scope.InstanceID, bootID)
	finish(response.StatusCode, int64(len(body)), "completed")
	jsonResponse(w, 200, record{"function_id": p.FunctionID, "deployment_id": p.DeploymentID, "status_code": response.StatusCode, "headers": response.Header, "body": base64.StdEncoding.EncodeToString(body), "body_encoding": "base64", "request_id": requestID(r)})
}
