package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

var storageMetadataKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func normalizeStorageMetadata(input map[string]string) (map[string]string, error) {
	result := map[string]string{}
	total := 0
	if len(input) > 64 {
		return nil, errors.New("too many metadata entries")
	}
	for k, v := range input {
		k = strings.ToLower(k)
		if !storageMetadataKey.MatchString(k) {
			return nil, errors.New("invalid metadata key")
		}
		if _, exists := result[k]; exists {
			return nil, errors.New("duplicate metadata key")
		}
		for _, c := range v {
			if c < 32 || c > 126 {
				return nil, errors.New("metadata values require printable ASCII")
			}
		}
		total += len(k) + len(v)
		if total > 2048 {
			return nil, errors.New("metadata exceeds 2 KiB")
		}
		result[k] = v
	}
	return result, nil
}
func metadataFromHeaders(r *http.Request, s3 bool) (map[string]string, error) {
	values := map[string]string{}
	if s3 {
		for name, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
				if len(v) != 1 {
					return nil, errors.New("duplicate metadata header")
				}
				values[strings.TrimPrefix(strings.ToLower(name), "x-amz-meta-")] = v[0]
			}
		}
	} else if raw := r.Header.Get("X-Neon-Object-Metadata"); raw != "" {
		if len(raw) > 4096 || json.Unmarshal([]byte(raw), &values) != nil {
			return nil, errors.New("invalid metadata JSON")
		}
	}
	return normalizeStorageMetadata(values)
}
func writeStorageMetadata(w http.ResponseWriter, metadata map[string]string) {
	for k, v := range metadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}
}
