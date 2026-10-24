package aigateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const (
	Responses  = "responses"
	Embeddings = "embeddings"
	Messages   = "messages"
	Gemini     = "gemini"
)

func ValidateProtocols(protocols []string) error {
	seen := map[string]bool{}
	for _, p := range protocols {
		if seen[p] || p != "chat_completions" && p != Responses && p != Embeddings && p != Messages && p != Gemini {
			return errors.New("invalid AI protocol capabilities")
		}
		seen[p] = true
	}
	return nil
}
func (r Route) SupportedProtocols() []string {
	if len(r.Protocols) == 0 {
		return []string{"chat_completions"}
	}
	return append([]string(nil), r.Protocols...)
}
func (r Route) Supports(protocol string) bool {
	for _, p := range r.SupportedProtocols() {
		if p == protocol {
			return true
		}
	}
	return false
}

// NativeRequest preserves the vendor's wire objects. No Chat-to-vendor
// conversion, remote file lookup, conversation store, or hosted tool execution.
type NativeRequest struct {
	RequestID       string
	Model           string
	Protocol        string
	Stream          bool
	MaxTokens       int
	body            map[string]any
	providerState   *ProviderStateBinding
	stateRequested  bool
	stateIntentHash string
}

var errNativeRequest = errors.New("invalid or unsupported native protocol request")
var geminiModel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func nativeObject(raw []byte) (map[string]any, error) {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if len(raw) == 0 || len(raw) > MaxBodyBytes || d.Decode(&v) != nil || v == nil || d.Decode(new(any)) != io.EOF {
		return nil, errNativeRequest
	}
	return v, nil
}
func number(v any, min, max int64) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, e := n.Int64()
	return i, e == nil && i >= min && i <= max
}
func nativeFields(v map[string]any, fields string) bool {
	allowed := "|" + strings.ReplaceAll(fields, " ", "|") + "|"
	for k := range v {
		if !strings.Contains(allowed, "|"+k+"|") {
			return false
		}
	}
	return true
}

// Reject provider-owned handles recursively: a shared supplier credential must
// never let a branch select another branch's stored input or conversation.
func statelessInput(v any, depth int) bool {
	if depth > 24 {
		return false
	}
	switch x := v.(type) {
	case []any:
		if len(x) > 256 {
			return false
		}
		for _, item := range x {
			if !statelessInput(item, depth+1) {
				return false
			}
		}
	case map[string]any:
		for k, value := range x {
			switch k {
			case "file_url", "fileUrl", "file_id", "file_uri", "fileUri", "fileData", "cachedContent", "conversation", "previous_response_id", "container", "vector_store_ids":
				return false
			case "image_url":
				s, ok := value.(string)
				if image, yes := value.(map[string]any); yes {
					s, ok = image["url"].(string)
				}
				if !ok || !strings.HasPrefix(s, "data:image/") {
					return false
				}
			}
			if k == "type" && (value == "item_reference" || value == "input_file" && x["file_data"] == nil) {
				return false
			}
			if !statelessInput(value, depth+1) {
				return false
			}
		}
	}
	return true
}
func boundedSequence(v any) bool { a, ok := v.([]any); return ok && len(a) > 0 && len(a) <= 32 }
func nonemptyInput(v any) bool {
	if s, ok := v.(string); ok {
		return len(s) > 0 && len(s) <= 16000
	}
	return boundedSequence(v)
}

func DecodeNative(raw []byte, c *Config, protocol, pathModel string, pathStream bool) (NativeRequest, Route, error) {
	q := NativeRequest{Protocol: protocol}
	v, err := nativeObject(raw)
	if err != nil {
		return q, Route{}, err
	}
	q.Model, _ = v["model"].(string)
	if protocol == Gemini && pathModel != "" {
		if q.Model != "" && q.Model != pathModel {
			return q, Route{}, errNativeRequest
		}
		q.Model = pathModel
	}
	r, ok := c.Find(q.Model)
	if !ok || !r.Supports(protocol) || !statelessInput(v, 0) || validateNativeMedia(v, protocol, r) != nil || nativeTextBytes(v) > 16000 {
		return q, r, errNativeRequest
	}
	if stream, exists := v["stream"]; exists {
		var valid bool
		q.Stream, valid = stream.(bool)
		if !valid {
			return q, r, errNativeRequest
		}
	}
	limitField := ""
	for field, maximum := range map[string]float64{"temperature": 2, "top_p": 1} {
		if value, exists := v[field]; exists {
			n, ok := value.(json.Number)
			if !ok {
				return q, r, errNativeRequest
			}
			f, e := n.Float64()
			if e != nil || f < 0 || f > maximum {
				return q, r, errNativeRequest
			}
		}
	}
	switch protocol {
	case "chat_completions":
		if !nativeFields(v, "model messages max_tokens max_completion_tokens stream temperature top_p tools tool_choice parallel_tool_calls response_format seed stop stream_options") || !boundedSequence(v["messages"]) {
			return q, r, errNativeRequest
		}
		inputBytes := 0
		for _, item := range v["messages"].([]any) {
			m, ok := item.(map[string]any)
			if !ok || !nativeFields(m, "role content name tool_calls tool_call_id refusal") {
				return q, r, errNativeRequest
			}
			role, _ := m["role"].(string)
			if role != "user" && role != "assistant" && role != "system" && role != "developer" && role != "tool" {
				return q, r, errNativeRequest
			}
			if !nonemptyInput(m["content"]) && !(role == "assistant" && m["tool_calls"] != nil) {
				return q, r, errNativeRequest
			}
			if text, ok := m["content"].(string); ok {
				inputBytes += len(text)
			} else {
				inputBytes += nativeTextBytes(m["content"])
			}
		}
		if inputBytes > 16000 {
			return q, r, errNativeRequest
		}
		if tools, exists := v["tools"]; exists {
			a, ok := tools.([]any)
			if !ok || len(a) > 32 {
				return q, r, errNativeRequest
			}
			for _, item := range a {
				tool, ok := item.(map[string]any)
				if !ok || tool["type"] != "function" {
					return q, r, errNativeRequest
				}
			}
		}
		limitField = "max_tokens"
		if _, exists := v["max_completion_tokens"]; exists {
			if v["max_tokens"] != nil {
				return q, r, errNativeRequest
			}
			limitField = "max_completion_tokens"
		}
	case Responses:
		if !nativeFields(v, "model input instructions max_output_tokens stream temperature top_p tools tool_choice parallel_tool_calls text reasoning store background include truncation") || !nonemptyInput(v["input"]) {
			return q, r, errNativeRequest
		}
		for _, k := range []string{"store", "background"} {
			if value, exists := v[k]; exists && value != false {
				return q, r, errNativeRequest
			}
		}
		v["store"] = false
		v["background"] = false
		if tools, exists := v["tools"]; exists {
			a, ok := tools.([]any)
			if !ok || len(a) > 32 {
				return q, r, errNativeRequest
			}
			for _, t := range a {
				tool, ok := t.(map[string]any)
				if !ok || tool["type"] != "function" {
					return q, r, errNativeRequest
				}
			}
		}
		limitField = "max_output_tokens"
	case Embeddings:
		if q.Stream || !nativeFields(v, "model input encoding_format dimensions") || !nonemptyInput(v["input"]) {
			return q, r, errNativeRequest
		}
		if format, exists := v["encoding_format"]; exists && format != "float" && format != "base64" {
			return q, r, errNativeRequest
		}
		if n, exists := v["dimensions"]; exists {
			if _, ok := number(n, 1, 65536); !ok {
				return q, r, errNativeRequest
			}
		}
	case Messages:
		if !nativeFields(v, "model messages system max_tokens stream temperature top_p top_k stop_sequences tools tool_choice thinking output_config") || !boundedSequence(v["messages"]) {
			return q, r, errNativeRequest
		}
		if tools, exists := v["tools"]; exists {
			a, ok := tools.([]any)
			if !ok || len(a) > 32 {
				return q, r, errNativeRequest
			}
			for _, t := range a {
				tool, ok := t.(map[string]any)
				if !ok {
					return q, r, errNativeRequest
				}
				if kind, exists := tool["type"]; exists && kind != "custom" {
					return q, r, errNativeRequest
				}
			}
		}
		limitField = "max_tokens"
	case Gemini:
		if !nativeFields(v, "model contents systemInstruction tools toolConfig generationConfig safetySettings") || !boundedSequence(v["contents"]) || !geminiModel.MatchString(r.UpstreamModel) {
			return q, r, errNativeRequest
		}
		q.Stream = pathStream
		if tools, exists := v["tools"]; exists {
			a, ok := tools.([]any)
			if !ok || len(a) > 32 {
				return q, r, errNativeRequest
			}
			for _, t := range a {
				tool, ok := t.(map[string]any)
				if !ok || !nativeFields(tool, "functionDeclarations") {
					return q, r, errNativeRequest
				}
			}
		}
		g, ok := v["generationConfig"].(map[string]any)
		if !ok {
			return q, r, errNativeRequest
		}
		n, ok := number(g["maxOutputTokens"], 1, int64(r.MaxOutputTokens))
		if !ok {
			return q, r, errNativeRequest
		}
		q.MaxTokens = int(n)
	default:
		return q, r, errNativeRequest
	}
	if limitField != "" {
		n, ok := number(v[limitField], 1, int64(r.MaxOutputTokens))
		if !ok {
			return q, r, errNativeRequest
		}
		q.MaxTokens = int(n)
	}
	q.body = v
	return q, r, nil
}

func (q NativeRequest) upstream(r Route) (string, []byte) {
	body := make(map[string]any, len(q.body))
	for k, v := range q.body {
		body[k] = v
	}
	body["model"] = r.UpstreamModel
	path := "/" + q.Protocol
	if q.Protocol == "chat_completions" {
		path = "/chat/completions"
		if q.Stream {
			body["stream_options"] = map[string]bool{"include_usage": true}
		}
	}
	if q.Protocol == Gemini {
		delete(body, "model")
		action := "generateContent"
		if q.Stream {
			action = "streamGenerateContent"
		}
		path = "/models/" + url.PathEscape(r.UpstreamModel) + ":" + action
		if q.Stream {
			path += "?alt=sse"
		}
		base := strings.TrimSuffix(strings.TrimRight(r.BaseURL, "/"), "/v1") + "/v1beta"
		payload, _ := json.Marshal(body)
		return base + path, payload
	}
	payload, _ := json.Marshal(body)
	return strings.TrimRight(r.BaseURL, "/") + path, payload
}
