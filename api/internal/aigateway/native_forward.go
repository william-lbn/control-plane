package aigateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
)

func usageNumber(v map[string]any, name string) (int64, bool) { return number(v[name], 0, 10000000) }
func optionalUsageNumber(v map[string]any, name string) (int64, bool) {
	if _, ok := v[name]; !ok {
		return 0, true
	}
	return usageNumber(v, name)
}
func nativeUsage(v map[string]any, protocol string) *Usage {
	field := "usage"
	if protocol == Gemini {
		field = "usageMetadata"
	}
	u, ok := v[field].(map[string]any)
	if !ok {
		return nil
	}
	var input, output, total, read, write int64
	var a, b, c, d, e bool
	d, e = true, true
	switch protocol {
	case "chat_completions":
		raw, _ := json.Marshal(v)
		var original map[string]any
		if json.Unmarshal(raw, &original) != nil {
			return nil
		}
		return usageFrom(original)
	case Responses:
		input, a = usageNumber(u, "input_tokens")
		output, b = usageNumber(u, "output_tokens")
		total, c = usageNumber(u, "total_tokens")
		if details, ok := u["input_tokens_details"].(map[string]any); ok {
			read, d = optionalUsageNumber(details, "cached_tokens")
		}
	case Embeddings:
		input, a = usageNumber(u, "prompt_tokens")
		output, b = 0, true
		total, c = usageNumber(u, "total_tokens")
	case Messages:
		input, a = usageNumber(u, "input_tokens")
		output, b = usageNumber(u, "output_tokens")
		read, d = optionalUsageNumber(u, "cache_read_input_tokens")
		write, e = optionalUsageNumber(u, "cache_creation_input_tokens")
		input += read + write
		total, c = input+output, true
	case Gemini:
		input, a = usageNumber(u, "promptTokenCount")
		total, c = usageNumber(u, "totalTokenCount")
		candidate, valid := optionalUsageNumber(u, "candidatesTokenCount")
		thought, validThought := optionalUsageNumber(u, "thoughtsTokenCount")
		tool, validTool := optionalUsageNumber(u, "toolUsePromptTokenCount")
		input += tool
		output, b = candidate+thought, valid && validThought && validTool
		read, d = optionalUsageNumber(u, "cachedContentTokenCount")
	}
	if !a || !b || !c || !d || !e || total != input+output || total > 10000000 || read+write > input {
		return nil
	}
	return &Usage{PromptTokens: input, CompletionTokens: output, TotalTokens: total, CacheReadTokens: read, CacheWriteTokens: write}
}

func nativeResponse(raw []byte, q NativeRequest, route Route) ([]byte, *Usage, error) {
	v, err := nativeObjectResponse(raw)
	if err != nil {
		return nil, nil, err
	}
	if v["error"] != nil {
		return nil, nil, errors.New("provider error")
	}
	switch q.Protocol {
	case "chat_completions":
		return normalize(raw, route.ID)
	case Responses:
		if v["object"] != "response" || v["status"] != "completed" && v["status"] != "incomplete" {
			return nil, nil, errNativeRequest
		}
		if _, ok := v["output"].([]any); !ok {
			return nil, nil, errNativeRequest
		}
		v["model"] = route.ID
		if err := q.rewriteProviderState(v); err != nil {
			if errors.Is(err, errProviderStateReceipt) {
				return nil, nativeUsage(v, q.Protocol), err
			}
			return nil, nil, err
		}
	case Embeddings:
		if v["object"] != "list" {
			return nil, nil, errNativeRequest
		}
		if _, ok := v["data"].([]any); !ok {
			return nil, nil, errNativeRequest
		}
		v["model"] = route.ID
	case Messages:
		if v["type"] != "message" {
			return nil, nil, errNativeRequest
		}
		if _, ok := v["content"].([]any); !ok {
			return nil, nil, errNativeRequest
		}
		v["model"] = route.ID
	case Gemini:
		if _, ok := v["candidates"].([]any); !ok {
			if _, ok := v["promptFeedback"].(map[string]any); !ok {
				return nil, nil, errNativeRequest
			}
		}
		if _, exists := v["modelVersion"]; exists {
			v["modelVersion"] = route.ID
		}
	}
	out, err := json.Marshal(v)
	return out, nativeUsage(v, q.Protocol), err
}
func nativeObjectResponse(raw []byte) (map[string]any, error) {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if len(raw) > MaxResponseBytes || d.Decode(&v) != nil || v == nil || d.Decode(new(any)) != io.EOF {
		return nil, errNativeRequest
	}
	return v, nil
}

// ForwardNative makes one bounded attempt. Vendor event names and JSON tool /
// multimodal objects are retained; stored-response APIs are deliberately absent.
func ForwardNative(ctx context.Context, w http.ResponseWriter, q NativeRequest, route Route, timeout time.Duration) Outcome {
	outcome := Outcome{Status: "failed"}
	if q.stateRequested && q.providerState == nil {
		writeError(w, 503, "provider_state_unavailable")
		return outcome
	}
	started := time.Now()
	target, payload := q.upstream(route)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		writeError(w, 502, "provider_unavailable")
		return outcome
	}
	request.Header.Set("Content-Type", "application/json")
	if q.RequestID != "" {
		request.Header.Set("X-Client-Request-Id", q.RequestID)
	}
	switch q.Protocol {
	case Messages:
		request.Header.Set("anthropic-version", "2023-06-01")
		if route.key != "" {
			request.Header.Set("x-api-key", route.key)
		}
	case Gemini:
		if route.key != "" {
			request.Header.Set("x-goog-api-key", route.key)
		}
	default:
		if route.key != "" {
			request.Header.Set("Authorization", "Bearer "+route.key)
		}
	}
	var dispatched atomic.Bool
	request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { dispatched.Store(true) }}))
	client := Client(route, timeout)
	defer client.CloseIdleConnections()
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
		if e != nil {
			writeError(w, 502, "invalid_provider_response")
			return outcome
		}
		out, usage, e := nativeResponse(raw, q, route)
		outcome.Usage = usage
		if e != nil {
			writeError(w, 502, "invalid_provider_response")
			return outcome
		}
		outcome.Usage = usage
		outcome.Status = "completed"
		if q.Protocol == Responses {
			if value, e := nativeObjectResponse(out); e == nil && value["status"] == "incomplete" {
				outcome.Status = "incomplete"
			}
		}
		if usage == nil && outcome.Status == "completed" {
			outcome.Status = "usage_unknown"
		}
		w.Header().Set("Content-Type", "application/json")
		if _, e = w.Write(out); e != nil {
			outcome.Status = "cancelled"
		}
		return outcome
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		writeError(w, 502, "invalid_provider_stream")
		return outcome
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	flush := http.NewResponseController(w)
	scanner := bufio.NewScanner(io.LimitReader(response.Body, MaxResponseBytes+1))
	scanner.Buffer(make([]byte, 4096), 65536)
	bytesRead := 0
	event := ""
	data := []string{}
	terminal := false
	failed := false
	var messageUsage map[string]any
	messageFinalUsage := false
	var geminiUsage *Usage
	geminiFinalUsage := false
	frame := func() bool {
		if len(data) == 0 {
			event = ""
			return true
		}
		if q.Protocol == "chat_completions" && strings.Join(data, "\n") == "[DONE]" {
			terminal = true
			data = nil
			event = ""
			if _, e := io.WriteString(w, "data: [DONE]\n\n"); e != nil {
				outcome.Status = "cancelled"
				return false
			}
			if flush.Flush() != nil {
				outcome.Status = "cancelled"
				return false
			}
			return true
		}
		v, e := nativeObjectResponse([]byte(strings.Join(data, "\n")))
		if e != nil {
			return false
		}
		kind, _ := v["type"].(string)
		if event != "" && kind != "" && event != kind {
			return false
		}
		if kind == "error" || v["error"] != nil {
			return false
		}
		switch q.Protocol {
		case "chat_completions":
			if v["object"] != "chat.completion.chunk" {
				return false
			}
			if _, ok := v["choices"].([]any); !ok {
				return false
			}
			v["model"] = route.ID
			if usage := nativeUsage(v, "chat_completions"); usage != nil {
				outcome.Usage = usage
			}
		case Responses:
			if !strings.HasPrefix(kind, "response.") {
				return false
			}
			if resp, ok := v["response"].(map[string]any); ok {
				if resp["error"] != nil {
					return false
				}
				resp["model"] = route.ID
				if err := q.rewriteProviderState(resp); err != nil {
					if errors.Is(err, errProviderStateReceipt) && (kind == "response.completed" || kind == "response.incomplete") {
						outcome.Usage = nativeUsage(resp, Responses)
					}
					return false
				}
				if kind == "response.completed" || kind == "response.incomplete" {
					outcome.Usage = nativeUsage(resp, Responses)
				}
			}
			terminal = kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed"
			if err := q.rewriteProviderEvent(v); err != nil {
				return false
			}
			failed = kind == "response.failed" || kind == "response.incomplete"
		case Messages:
			if kind == "message_start" {
				m, ok := v["message"].(map[string]any)
				if !ok {
					return false
				}
				m["model"] = route.ID
				messageUsage, _ = m["usage"].(map[string]any)
			}
			if kind == "message_delta" {
				if u, ok := v["usage"].(map[string]any); ok && messageUsage != nil {
					_, messageFinalUsage = usageNumber(u, "output_tokens")
					for k, value := range u {
						messageUsage[k] = value
					}
				}
			}
			terminal = kind == "message_stop"
			if terminal && messageFinalUsage {
				outcome.Usage = nativeUsage(map[string]any{"usage": messageUsage}, Messages)
			}
		case Gemini:
			if _, ok := v["candidates"].([]any); !ok {
				if _, ok := v["promptFeedback"].(map[string]any); !ok {
					return false
				}
			}
			geminiUsage = nativeUsage(v, Gemini)
			geminiFinalUsage = false
			if candidates, ok := v["candidates"].([]any); ok {
				for _, candidate := range candidates {
					if item, ok := candidate.(map[string]any); ok {
						if reason, ok := item["finishReason"].(string); ok && reason != "" {
							geminiFinalUsage = true
						}
					}
				}
			}
			if feedback, ok := v["promptFeedback"].(map[string]any); ok {
				if reason, ok := feedback["blockReason"].(string); ok && reason != "" {
					geminiFinalUsage = true
				}
			}
			if _, exists := v["modelVersion"]; exists {
				v["modelVersion"] = route.ID
			}
		}
		out, _ := json.Marshal(v)
		if outcome.FirstTokenMillis == nil {
			ms := time.Since(started).Milliseconds()
			outcome.FirstTokenMillis = &ms
		}
		prefix := ""
		if event != "" {
			prefix = "event: " + event + "\n"
		}
		if _, e = io.WriteString(w, prefix+"data: "+string(out)+"\n\n"); e != nil {
			outcome.Status = "cancelled"
			return false
		}
		if flush.Flush() != nil {
			outcome.Status = "cancelled"
			return false
		}
		event = ""
		data = nil
		return true
	}
	for scanner.Scan() {
		line := scanner.Text()
		bytesRead += len(line) + 1
		if bytesRead > MaxResponseBytes {
			break
		}
		if line == "" {
			if !frame() {
				failed = true
				break
			}
			if terminal {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	// Gemini uses clean EOF instead of a terminal event. A truncated transport is
	// not clean EOF; final cumulative usage is accepted only after that boundary.
	if q.Protocol == Gemini && scanner.Err() == nil && bytesRead <= MaxResponseBytes && !failed && len(data) == 0 && geminiUsage != nil && geminiFinalUsage {
		terminal = true
		outcome.Usage = geminiUsage
	}
	if outcome.Status == "cancelled" {
		return outcome
	}
	if !terminal || failed || scanner.Err() != nil {
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
