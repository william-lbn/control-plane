package control

import (
	"net/http"
	"strings"
)

// Unsupported authorization/encryption promises must never be acknowledged by
// publishing ordinary private/plain objects. This runs before opening SQL.
func rejectUnsupportedS3Headers(w http.ResponseWriter, r *http.Request) bool {
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		unsupported := strings.HasPrefix(lower, "x-amz-grant-") ||
			strings.HasPrefix(lower, "x-amz-server-side-encryption") ||
			strings.HasPrefix(lower, "x-amz-copy-source-server-side-encryption") ||
			strings.HasPrefix(lower, "x-amz-object-lock-") || lower == "x-amz-website-redirect-location"
		if lower == "x-amz-acl" {
			unsupported = len(values) != 1 || values[0] != "private"
		}
		if lower == "x-amz-storage-class" {
			unsupported = len(values) != 1 || values[0] != "STANDARD"
		}
		if unsupported {
			s3Error(w, r, http.StatusNotImplemented, "NotImplemented")
			return true
		}
	}
	return false
}
