package functions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const MaxOutputLine = 4096
const MaxOutputRecords = 512
const MaxOutputPage = 16

type OutputRecord struct {
	Sequence  int64     `json:"sequence"`
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	Severity  string    `json:"severity"`
	ScopeName string    `json:"scope_name"`
	Message   string    `json:"message"`
	Truncated bool      `json:"truncated"`
}
type OutputPage struct {
	Scope   Scope          `json:"scope"`
	BootID  string         `json:"boot_id"`
	Records []OutputRecord `json:"records"`
	Next    int64          `json:"next_sequence"`
	Dropped int64          `json:"dropped"`
}

// Identity and sequence belong to the privileged supervisor. App level hints
// never authorize a platform scope, instance, boot or deployment identity.
type OutputJournal struct {
	mu      sync.Mutex
	records []OutputRecord
	next    int64
	secrets []string
}

func NewOutputJournal(database string, environment map[string]string) *OutputJournal {
	values := []string{database}
	if u, e := url.Parse(database); e == nil && u.User != nil {
		if p, ok := u.User.Password(); ok {
			values = append(values, p, url.QueryEscape(p), url.PathEscape(p))
		}
	}
	for _, v := range environment {
		if v != "" {
			values = append(values, v, url.QueryEscape(v))
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return &OutputJournal{secrets: values}
}
func (j *OutputJournal) append(stream, level, scope, message string, truncated bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	message = strings.ToValidUTF8(message, "�")
	for _, secret := range j.secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if len(message) > MaxOutputLine {
		message = "[output line exceeded 4096 bytes]"
		truncated = true
	}
	j.next++
	j.records = append(j.records, OutputRecord{j.next, time.Now().UTC(), stream, level, scope, message, truncated})
	if len(j.records) > MaxOutputRecords {
		copy(j.records, j.records[1:])
		j.records = j.records[:MaxOutputRecords]
	}
}
func (j *OutputJournal) Platform(message string) {
	if j != nil {
		j.append("platform", "INFO", "neon.function.request", message, false)
	}
}

type OutputWriter struct {
	journal  *OutputJournal
	stream   string
	mu       sync.Mutex
	pending  []byte
	overflow bool
}

func (j *OutputJournal) Writer(stream string) *OutputWriter {
	return &OutputWriter{journal: j, stream: stream}
}
func (w *OutputWriter) line() {
	text := string(w.pending)
	level := "INFO"
	if w.stream == "stderr" {
		level = "ERROR"
	}
	if w.overflow {
		text = "[output line exceeded 4096 bytes]"
	} else if strings.HasPrefix(text, "@neon-app-log-v1:") {
		var v struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if strictJSON([]byte(strings.TrimPrefix(text, "@neon-app-log-v1:")), &v) == nil && (v.Level == "DEBUG" || v.Level == "INFO" || v.Level == "WARN" || v.Level == "ERROR") {
			level = v.Level
			text = v.Message
		}
	}
	w.journal.append(w.stream, level, "neon.function.app", text, w.overflow)
	w.pending = nil
	w.overflow = false
}
func (w *OutputWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(b)
	for len(b) > 0 {
		index := bytes.IndexByte(b, '\n')
		part := b
		if index >= 0 {
			part = b[:index]
		}
		if !w.overflow {
			if len(w.pending)+len(part) > MaxOutputLine {
				w.pending = nil
				w.overflow = true
			} else {
				w.pending = append(w.pending, part...)
			}
		}
		if index < 0 {
			break
		}
		w.line()
		b = b[index+1:]
	}
	return n, nil
}
func (w *OutputWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 || w.overflow {
		w.line()
	}
}
func (j *OutputJournal) page(scope Scope, boot string, after int64) OutputPage {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := OutputPage{Scope: scope, BootID: boot, Records: []OutputRecord{}, Next: after}
	if len(j.records) > 0 && after < j.records[0].Sequence-1 {
		out.Dropped = j.records[0].Sequence - 1 - after
		out.Next = j.records[0].Sequence - 1
	}
	encoded := 0
	for _, v := range j.records {
		if v.Sequence > after {
			raw, _ := json.Marshal(v)
			if encoded+len(raw) > 60<<10 {
				break
			}
			encoded += len(raw)
			out.Records = append(out.Records, v)
			out.Next = v.Sequence
			if len(out.Records) == MaxOutputPage {
				break
			}
		}
	}
	return out
}
func (m *Manager) outputPage(w http.ResponseWriter, r *http.Request, body []byte) {
	var in struct {
		BootID     string `json:"boot_id"`
		Generation int64  `json:"generation"`
		After      int64  `json:"after"`
	}
	if strictJSON(body, &in) != nil || in.BootID != m.bootID || in.Generation != m.scope.Generation || in.After < 0 || m.output == nil {
		managerError(w, 422, "invalid_output_scope")
		return
	}
	m.output.mu.Lock()
	next := m.output.next
	m.output.mu.Unlock()
	if in.After > next {
		managerError(w, 422, "invalid_output_cursor")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(m.output.page(m.scope, m.bootID, in.After))
}
func (c *InstanceClient) Output(ctx context.Context, boot string, after int64) (OutputPage, error) {
	return c.outputRequest(ctx, "/internal/v1/output", boot, after)
}
func (c *InstanceClient) ShutdownOutput(ctx context.Context, boot string, after int64) (OutputPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.outputRequest(ctx, "/internal/v1/shutdown-output", boot, after)
}
func (c *InstanceClient) outputRequest(ctx context.Context, path, boot string, after int64) (OutputPage, error) {
	var out OutputPage
	if !bootPattern.MatchString(boot) || after < 0 {
		return out, errors.New("explicit output boot/cursor required")
	}
	body, _ := json.Marshal(struct {
		BootID     string `json:"boot_id"`
		Generation int64  `json:"generation"`
		After      int64  `json:"after"`
	}{boot, c.scope.Generation, after})
	response, err := c.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return out, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
	if err != nil || len(raw) >= 128<<10 || response.StatusCode != 200 || strictJSON(raw, &out) != nil || out.Scope != c.scope || out.BootID != boot || out.Next < after || out.Dropped < 0 || len(out.Records) > MaxOutputPage {
		return OutputPage{}, errors.New("output identity or bounds rejected")
	}
	last := after + out.Dropped
	for _, v := range out.Records {
		if v.Sequence != last+1 || v.Timestamp.IsZero() || len(v.Message) > MaxOutputLine || !utf8.ValidString(v.Message) || (v.Stream != "stdout" && v.Stream != "stderr" && v.Stream != "platform") || (v.Severity != "DEBUG" && v.Severity != "INFO" && v.Severity != "WARN" && v.Severity != "ERROR") || (v.ScopeName != "neon.function.app" && v.ScopeName != "neon.function.request") || (v.Stream == "platform") != (v.ScopeName == "neon.function.request") {
			return OutputPage{}, errors.New("output record rejected")
		}
		last = v.Sequence
	}
	if out.Next != last {
		return OutputPage{}, errors.New("output sequence rejected")
	}
	return out, nil
}
