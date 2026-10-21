package functions

import (
	"bytes"
	"encoding/json"
	"net/http"
)

type shutdownResult struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *shutdownResult) Header() http.Header { return w.header }
func (w *shutdownResult) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *shutdownResult) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	return w.body.Write(b)
}

// The real supervisor closes its listener after child shutdown. Return the
// final bounded tail in that SAME authenticated shutdown response, before the
// active handler drains; a follow-up request could arrive after process exit.
func (m *Manager) shutdownOutput(w http.ResponseWriter, r *http.Request, body []byte) {
	var input struct {
		BootID     string `json:"boot_id"`
		Generation int64  `json:"generation"`
		After      int64  `json:"after"`
	}
	if strictJSON(body, &input) != nil || input.BootID != m.bootID || input.Generation != m.scope.Generation || input.After < 0 || m.output == nil {
		managerError(w, 422, "invalid_output_shutdown_scope")
		return
	}
	m.output.mu.Lock()
	next := m.output.next
	m.output.mu.Unlock()
	if input.After > next {
		managerError(w, 422, "invalid_output_cursor")
		return
	}
	request, _ := json.Marshal(struct {
		BootID     string `json:"boot_id"`
		Generation int64  `json:"generation"`
	}{input.BootID, input.Generation})
	result := &shutdownResult{header: http.Header{}}
	m.shutdown(result, r, request)
	if result.code != 204 {
		for k, values := range result.header {
			w.Header()[k] = values
		}
		w.WriteHeader(result.code)
		_, _ = w.Write(result.body.Bytes())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(m.output.tailPage(m.scope, m.bootID, input.After))
}
func (j *OutputJournal) tailPage(scope Scope, boot string, after int64) OutputPage {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := OutputPage{Scope: scope, BootID: boot, Records: []OutputRecord{}, Next: after}
	start := len(j.records)
	encoded := 0
	for i := len(j.records) - 1; i >= 0 && j.records[i].Sequence > after; i-- {
		raw, _ := json.Marshal(j.records[i])
		if len(j.records)-i > MaxOutputPage || encoded+len(raw) > 60<<10 {
			break
		}
		encoded += len(raw)
		start = i
	}
	if start < len(j.records) {
		out.Records = append(out.Records, j.records[start:]...)
		out.Dropped = out.Records[0].Sequence - after - 1
		out.Next = out.Records[len(out.Records)-1].Sequence
	}
	return out
}
