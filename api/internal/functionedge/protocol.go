// Package functionedge implements the public data plane. It has no dependency
// on control, PostgreSQL, Kubernetes or object-storage credentials.
package functionedge

import (
	"net/http"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

const BrokerPath = "/internal/function-edge/v1/invoke"
const MaxStreamDuration = 15 * time.Minute

type Admission struct {
	Branch    string      `json:"branch"`
	Slug      string      `json:"slug"`
	Method    string      `json:"method"`
	URI       string      `json:"uri"`
	Headers   http.Header `json:"headers"`
	Body      []byte      `json:"body"`
	WebSocket bool        `json:"websocket"`
}

type Grant struct {
	EventID           string                     `json:"event_id"`
	RequestID         string                     `json:"request_id"`
	DefinitionVersion int64                      `json:"definition_version"`
	Deadline          time.Time                  `json:"deadline"`
	Request           functions.DelegatedRequest `json:"request"`
}

// Completion is created by the edge after transferring bytes. This object is
// accepted only on the original mutually authenticated admission connection.
type Completion struct {
	EventID            string `json:"event_id"`
	Dispatched         bool   `json:"dispatched"`
	Status             int    `json:"status"`
	RequestBytes       int64  `json:"request_bytes"`
	ResponseBytes      int64  `json:"response_bytes"`
	StreamRequestBytes int64  `json:"stream_request_bytes"`
	DurationMS         int64  `json:"duration_ms"`
	Outcome            string `json:"outcome"`
}

// Headers are shared by edge and broker: callers cannot inject manager auth,
// control metadata, connection headers or cookies into customer execution.
func IngressHeaders(in http.Header) http.Header {
	out := make(http.Header)
	connection := map[string]bool{}
	for _, values := range in.Values("Connection") {
		for _, name := range strings.Split(values, ",") {
			connection[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range in {
		lower := strings.ToLower(name)
		if connection[lower] {
			continue
		}
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

func CopyPublicHeaders(out, in http.Header) {
	for name, values := range IngressHeaders(in) {
		switch strings.ToLower(name) {
		case "set-cookie", "clear-site-data", "service-worker-allowed", "strict-transport-security":
			continue
		}
		out[name] = values
	}
	out.Set("Cache-Control", "no-store")
	out.Set("X-Content-Type-Options", "nosniff")
}
