package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var dataAPITablePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,62}$`)

// Console testing is an authenticated server action. Only the application JWT
// reaches the public data relay; Console cookies/CSRF and the browser's Origin
// never enter the branch runtime. Public CORS policy remains enforced there.
func (s *server) dataAPIConsoleRequest(w http.ResponseWriter, r *http.Request) {
	if userFrom(r).APIKey {
		fail(w, r, 403, "forbidden", "A Console session is required")
		return
	}
	var input struct {
		Method string          `json:"method"`
		Table  string          `json:"table"`
		Token  string          `json:"application_token"`
		Body   json.RawMessage `json:"body"`
	}
	if err := readJSON(r, &input); err != nil || !dataAPITablePattern.MatchString(input.Table) || len(input.Token) > 16384 || input.Token == "" || strings.ContainsAny(input.Token, "\r\n") {
		fail(w, r, 422, "invalid_request", "A table, bounded application JWT and supported method are required")
		return
	}
	switch input.Method {
	case "GET", "POST", "PATCH", "DELETE":
	default:
		fail(w, r, 422, "invalid_request", "Unsupported Console Data API method")
		return
	}
	branch := r.PathValue("branch")
	if _, err := s.one(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, branch, r.PathValue("project")); err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Branch not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read branch")
		}
		return
	}
	body := input.Body
	if input.Method == "GET" {
		body = nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 145*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, input.Method, "/data/v1/"+branch+"/"+input.Table, bytes.NewReader(body))
	if err != nil {
		fail(w, r, 422, "invalid_request", "Invalid request")
		return
	}
	request.SetPathValue("dataBranch", branch)
	request.Header.Set("Authorization", "Bearer "+input.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Prefer", "return=representation")
	output := &dataAPIConsoleWriter{header: make(http.Header), code: 200}
	s.dataAPIRelay(output, request)
	jsonResponse(w, 200, map[string]any{"status": output.code, "body": output.body.String(), "truncated": output.truncated})
}

// Keep response memory bounded even if an allowed table contains large values.
// The server does not persist or log the JWT/request body/result body.
type dataAPIConsoleWriter struct {
	header    http.Header
	code      int
	started   bool
	body      bytes.Buffer
	truncated bool
}

func (w *dataAPIConsoleWriter) Header() http.Header { return w.header }
func (w *dataAPIConsoleWriter) WriteHeader(code int) {
	if !w.started {
		w.code = code
		w.started = true
	}
}
func (w *dataAPIConsoleWriter) Write(p []byte) (int, error) {
	w.WriteHeader(200)
	n := len(p)
	remaining := 65536 - w.body.Len()
	if len(p) > remaining {
		w.truncated = true
		p = p[:remaining]
	}
	w.body.Write(p)
	return n, nil
}
