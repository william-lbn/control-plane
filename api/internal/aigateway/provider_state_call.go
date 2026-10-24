package aigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

var providerStatePath = regexp.MustCompile(`^/(responses/resp_[a-zA-Z0-9_-]{1,192}(/input_items)?|conversations(/conv_[a-zA-Z0-9_-]{1,192}(/items(/[a-zA-Z0-9_-]{1,192})?)?)?)$`)

// One bounded supplier request. The route supplies authentication and pinned
// network policy; incoming cookies, SDK headers and process proxies are unused.
// A failed mutation after dispatch is ambiguous and must never be retried here.
func ProviderStateCall(ctx context.Context, route Route, method, path, query string, body []byte, timeout time.Duration) (value map[string]any, status int, dispatched bool, err error) {
	if !route.AllowProviderState || !route.Supports(Responses) || !providerStatePath.MatchString(path) || strings.ContainsAny(query, "\r\n#") || len(query) > 2048 || len(body) > MaxBodyBytes || method != "GET" && method != "POST" && method != "DELETE" {
		return nil, 0, false, errors.New("provider state request rejected")
	}
	target := strings.TrimRight(route.BaseURL, "/") + path
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if route.key != "" {
		req.Header.Set("Authorization", "Bearer "+route.key)
	}
	var sent atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }}))
	client := Client(route, timeout)
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, sent.Load(), errors.New("supplier state outcome unavailable")
	}
	defer response.Body.Close()
	status = response.StatusCode
	if status < 200 || status > 299 {
		return nil, status, sent.Load(), errors.New("supplier state request rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes {
		return nil, status, sent.Load(), errors.New("supplier state response exceeds limits")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&value) != nil || value == nil || d.Decode(new(any)) != io.EOF {
		return nil, status, sent.Load(), errors.New("invalid supplier state response")
	}
	return value, status, sent.Load(), nil
}

func RewriteProviderResponse(value map[string]any, model string, binding ProviderStateBinding) error {
	if !ResponseHandle.MatchString(binding.ResponseID) || len(binding.Proof) < 32 || binding.CommitResponse == nil {
		return errors.New("provider state binding rejected")
	}
	q := NativeRequest{Protocol: Responses, providerState: &binding}
	if err := q.rewriteProviderState(value); err != nil {
		return err
	}
	value["model"] = model
	return nil
}
