package control

import (
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

func validStorageDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// Source predicates are evaluated under the same directory transaction lock as
// the copy. ETag conditions take precedence over their paired date conditions,
// as documented by S3 CopyObject; they are not four independent AND predicates.
type storageCopyConditions struct {
	Match, NoneMatch               string
	ModifiedSince, UnmodifiedSince *time.Time
}

func parseStorageCopyConditions(h http.Header) (storageCopyConditions, error) {
	var c storageCopyConditions
	for _, field := range []struct {
		name   string
		target *string
	}{
		{"X-Amz-Copy-Source-If-Match", &c.Match},
		{"X-Amz-Copy-Source-If-None-Match", &c.NoneMatch},
	} {
		values := h.Values(field.name)
		if len(values) == 0 {
			continue
		}
		v := strings.TrimSpace(values[0])
		// This service uses a single SHA-256 ETag, or wildcard. Reject weak tags,
		// ambiguous repeated fields and lists instead of silently ignoring them.
		bare := strings.Trim(v, "\"")
		if len(values) != 1 || (v != "*" && (!validStorageDigest(bare) || (v != bare && v != "\""+bare+"\""))) {
			return c, &storageMutationFault{400, "InvalidArgument"}
		}
		*field.target = bare
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{
		{"X-Amz-Copy-Source-If-Modified-Since", &c.ModifiedSince},
		{"X-Amz-Copy-Source-If-Unmodified-Since", &c.UnmodifiedSince},
	} {
		values := h.Values(field.name)
		if len(values) == 0 {
			continue
		}
		v, err := http.ParseTime(values[0])
		if len(values) != 1 || err != nil {
			return c, &storageMutationFault{400, "InvalidArgument"}
		}
		*field.target = &v
	}
	return c, nil
}

func (c storageCopyConditions) accepts(digest string, modified time.Time) bool {
	match := strings.Trim(c.Match, "\"")
	none := strings.Trim(c.NoneMatch, "\"")
	if match != "" && match != "*" && match != digest {
		return false
	}
	if none != "" && (none == "*" || none == digest) {
		return false
	}
	// Last-Modified and HTTP condition dates have whole-second precision.
	modified = modified.UTC().Truncate(time.Second)
	if match == "" && c.UnmodifiedSince != nil && modified.After(*c.UnmodifiedSince) {
		return false
	}
	if none == "" && c.ModifiedSince != nil && !modified.After(*c.ModifiedSince) {
		return false
	}
	return true
}
