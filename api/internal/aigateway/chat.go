package aigateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const MaxBodyBytes = 32768
const MaxResponseBytes = 1048576

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type ChatRequest struct {
	Protocol    string    `json:"-"`
	RequestID   string    `json:"-"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature *float64  `json:"temperature,omitempty"`
}
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}
type Outcome struct {
	Status           string
	Usage            *Usage
	UpstreamStatus   int
	FirstTokenMillis *int64
	Dispatched       bool
}

// This first protocol slice accepts text chat only. Reject unsupported fields
// rather than silently dropping tools/images or pretending to convert protocols.
func Decode(raw []byte, c *Config) (ChatRequest, Route, error) {
	var q ChatRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > MaxBodyBytes || d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF {
		return q, Route{}, errors.New("invalid chat request")
	}
	r, ok := c.Find(q.Model)
	if !ok || !r.Supports("chat_completions") {
		return q, r, errors.New("model unavailable")
	}
	if len(q.Messages) < 1 || len(q.Messages) > 32 || q.MaxTokens < 1 || q.MaxTokens > r.MaxOutputTokens || q.Temperature != nil && (*q.Temperature < 0 || *q.Temperature > 2) {
		return q, r, errors.New("invalid chat limits")
	}
	size := 0
	for _, m := range q.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" {
			return q, r, errors.New("unsupported message role")
		}
		size += len(m.Content)
	}
	if size < 1 || size > 16000 {
		return q, r, errors.New("chat input exceeds limits")
	}
	return q, r, nil
}

// DNS answers are checked on every connection. Redirects, proxy environment
// variables and caller-supplied Authorization/Cookie headers are never used.
func Client(r Route, timeout time.Duration) *http.Client {
	transport := &http.Transport{TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: timeout, MaxIdleConnsPerHost: 2, DisableCompression: true}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil || len(ips) == 0 || len(ips) > 8 {
			if e == nil {
				e = errors.New("provider address rejected")
			}
			return nil, e
		}
		// Reject the complete DNS answer if any address left the tested pins.
		// Selecting just one permitted answer would accept a mixed rebind.
		for _, ip := range ips {
			matched := false
			for _, allowed := range r.AllowedIPs {
				if ip.IP.Equal(net.ParseIP(allowed)) {
					matched = true
					break
				}
			}
			if !matched {
				return nil, errors.New("provider address rejected")
			}
		}
		target := net.JoinHostPort(ips[0].IP.String(), port)
		if r.egressProxy != "" {
			return dialProviderProxy(ctx, network, r.egressProxy, target)
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, target)
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "gateway_error", "code": code, "message": code}})
}
func Error(w http.ResponseWriter, status int, code string) { writeError(w, status, code) }
func usageFrom(v map[string]any) *Usage {
	raw, ok := v["usage"]
	if !ok || raw == nil {
		return nil
	}
	b, _ := json.Marshal(raw)
	var u Usage
	if json.Unmarshal(b, &u) != nil || u.PromptTokens < 0 || u.PromptTokens > 10000000 || u.CompletionTokens < 0 || u.CompletionTokens > 10000000 || u.TotalTokens < 0 || u.TotalTokens != u.PromptTokens+u.CompletionTokens || u.TotalTokens > 10000000 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return nil
	}
	for _, field := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if len(fields[field]) == 0 || string(fields[field]) == "null" {
			return nil
		}
	}
	var details struct {
		Cached int64 `json:"cached_tokens"`
	}
	if raw := fields["prompt_tokens_details"]; len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &details) != nil || details.Cached < 0 || details.Cached > u.PromptTokens {
			return nil
		}
		u.CacheReadTokens = details.Cached
	}
	return &u
}
func normalize(data []byte, model string) ([]byte, *Usage, error) {
	var v map[string]any
	if json.Unmarshal(data, &v) != nil || v == nil {
		return nil, nil, errors.New("invalid provider response")
	}
	if _, ok := v["error"]; ok {
		return nil, nil, errors.New("provider error")
	}
	if v["object"] != "chat.completion" && v["object"] != "chat.completion.chunk" {
		return nil, nil, errors.New("unsupported provider response")
	}
	if _, ok := v["choices"].([]any); !ok {
		return nil, nil, errors.New("invalid provider choices")
	}
	v["model"] = model
	out, e := json.Marshal(v)
	return out, usageFrom(v), e
}

// Exactly one upstream attempt: inference is not replayed after timeouts or
// disconnects. Missing stream usage stays unknown, never fabricated as zero.
func Forward(ctx context.Context, w http.ResponseWriter, q ChatRequest, r Route, timeout time.Duration) Outcome {
	outcome := Outcome{Status: "failed"}
	started := time.Now()
	q.Model = r.UpstreamModel
	payload, _ := json.Marshal(q)
	var body map[string]any
	json.Unmarshal(payload, &body)
	if q.Stream {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	payload, _ = json.Marshal(body)
	u, _ := url.Parse(strings.TrimRight(r.BaseURL, "/") + "/chat/completions")
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	if q.RequestID != "" {
		request.Header.Set("X-Client-Request-Id", q.RequestID)
	}
	if r.key != "" {
		request.Header.Set("Authorization", "Bearer "+r.key)
	}
	client := Client(r, timeout)
	defer client.CloseIdleConnections()
	var dispatched atomic.Bool
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{WroteHeaders: func() { dispatched.Store(true) }}))
	response, err := client.Do(request)
	outcome.Dispatched = dispatched.Load()
	if err != nil {
		writeError(w, 502, "provider_unavailable")
		return outcome
	}
	defer response.Body.Close()
	outcome.Dispatched = true
	outcome.UpstreamStatus = response.StatusCode
	if response.StatusCode != 200 {
		status := 502
		if response.StatusCode == 429 {
			status = 429
		}
		writeError(w, status, "provider_rejected")
		return outcome
	}
	if !q.Stream {
		raw, e := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
		if e != nil || len(raw) > MaxResponseBytes {
			writeError(w, 502, "invalid_provider_response")
			return outcome
		}
		out, usage, e := normalize(raw, r.ID)
		if e != nil {
			writeError(w, 502, "invalid_provider_response")
			return outcome
		}
		w.Header().Set("Content-Type", "application/json")
		outcome.Usage = usage
		if _, e = w.Write(out); e != nil {
			outcome.Status = "cancelled"
			return outcome
		}
		outcome.Usage = usage
		outcome.Status = "completed"
		if usage == nil {
			outcome.Status = "usage_unknown"
		}
		return outcome
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		writeError(w, 502, "invalid_provider_stream")
		return outcome
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, MaxResponseBytes+1))
	scanner.Buffer(make([]byte, 4096), 65536)
	total := 0
	done := false
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	flush := http.NewResponseController(w)
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > MaxResponseBytes {
			break
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			io.WriteString(w, "data: [DONE]\n\n")
			flush.Flush()
			break
		}
		out, usage, e := normalize([]byte(data), r.ID)
		if e != nil {
			break
		}
		if usage != nil {
			outcome.Usage = usage
		}
		if outcome.FirstTokenMillis == nil {
			ms := time.Since(started).Milliseconds()
			outcome.FirstTokenMillis = &ms
		}
		if _, e = io.WriteString(w, "data: "+string(out)+"\n\n"); e != nil {
			outcome.Status = "cancelled"
			return outcome
		}
		if flush.Flush() != nil {
			outcome.Status = "cancelled"
			return outcome
		}
	}
	if !done || scanner.Err() != nil {
		io.WriteString(w, "event: error\ndata: {\"error\":{\"code\":\"provider_stream_incomplete\"}}\n\n")
		flush.Flush()
		outcome.Status = "incomplete"
		return outcome
	}
	outcome.Status = "completed"
	if outcome.Usage == nil {
		outcome.Status = "usage_unknown"
	}
	return outcome
}
