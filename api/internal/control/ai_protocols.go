package control

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

func registerAINativePublic(mux *http.ServeMux, branch string, handler http.HandlerFunc) {
	root := "POST /ai/v1/{" + branch + "}"
	for _, path := range []string{"/v1/responses", "/openai/v1/responses", "/v1/embeddings", "/openai/v1/embeddings", "/v1/messages", "/anthropic/v1/messages", "/gemini/v1beta/models/{aiModelAction}"} {
		mux.HandleFunc(root+path, handler)
	}
}
func aiNativeRequestRoute(r *http.Request) (string, string, bool) {
	if p := r.PathValue("protocol"); p != "" {
		return p, "", false
	}
	if action := r.PathValue("aiModelAction"); action != "" {
		model, method, ok := strings.Cut(action, ":")
		if !ok {
			return "invalid", "", false
		}
		if method != "generateContent" && method != "streamGenerateContent" {
			return "invalid", "", false
		}
		if r.URL.RawQuery != "" && r.URL.RawQuery != "alt=sse" {
			return "invalid", "", false
		}
		return aigateway.Gemini, model, method == "streamGenerateContent"
	}
	for _, p := range []string{aigateway.Responses, aigateway.Embeddings, aigateway.Messages} {
		if strings.HasSuffix(r.URL.Path, "/"+p) {
			return p, "", false
		}
	}
	return "", "", false
}
func aiNativePath(protocol, model string, stream bool) string {
	if protocol == aigateway.Gemini {
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		return "/gemini/v1beta/models/" + url.PathEscape(model) + ":" + action
	}
	return "/" + protocol
}
func pathWithVersion(path string) string {
	if strings.HasPrefix(path, "/gemini/") {
		return path
	}
	return "/v1" + path
}

// SDK supplier headers carry the branch credential at our public boundary;
// supplier authentication is independently created from the private route.
func aiAuthorization(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		return h
	}
	p, _, _ := aiNativeRequestRoute(r)
	if p == aigateway.Messages {
		if key := r.Header.Get("x-api-key"); key != "" {
			return "Bearer " + key
		}
	}
	if p == aigateway.Gemini {
		if key := r.Header.Get("x-goog-api-key"); key != "" {
			return "Bearer " + key
		}
	}
	return ""
}
