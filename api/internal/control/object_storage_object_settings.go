package control

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

type storageObjectSettings struct {
	Tags            []storageTag      `json:"tags"`
	ResponseHeaders map[string]string `json:"response_headers"`
}

func normalizeStorageResponseHeaders(input map[string]string) (map[string]string, error) {
	out := map[string]string{}
	total := 0
	for name, value := range input {
		name = strings.ToLower(name)
		if _, exists := out[name]; exists || len(value) > 1024 {
			return nil, errors.New("invalid response headers")
		}
		switch name {
		case "content-disposition":
			kind, params, e := mime.ParseMediaType(value)
			if e != nil || kind != "attachment" && kind != "inline" {
				return nil, errors.New("invalid disposition")
			}
			for key, v := range params {
				if key != "filename" || !cleanStorageString(v, 128) || strings.ContainsAny(v, "/\\") {
					return nil, errors.New("invalid disposition parameter")
				}
			}
			value = mime.FormatMediaType(kind, params)
		case "cache-control":
			if value == "" {
				return nil, errors.New("empty cache configuration")
			}
			for _, r := range value {
				if r < 32 || r > 126 {
					return nil, errors.New("invalid cache configuration")
				}
			}
		default:
			return nil, errors.New("unsupported response header")
		}
		total += len(value)
		if total > 2048 {
			return nil, errors.New("response headers exceed capacity")
		}
		out[name] = value
	}
	return out, nil
}
func storageResponseHeaders(r *http.Request) (map[string]string, error) {
	values := map[string]string{}
	for _, name := range []string{"Content-Disposition", "Cache-Control"} {
		if all := r.Header.Values(name); len(all) > 1 {
			return nil, errors.New("duplicate response header")
		} else if len(all) == 1 {
			values[name] = all[0]
		}
	}
	return normalizeStorageResponseHeaders(values)
}
func storageTagsHeader(r *http.Request) ([]storageTag, error) {
	values := r.Header.Values("X-Amz-Tagging")
	if len(values) > 1 || len(r.Header.Get("X-Amz-Tagging")) > 16384 {
		return nil, errors.New("invalid tagging header")
	}
	q, e := url.ParseQuery(r.Header.Get("X-Amz-Tagging"))
	if e != nil {
		return nil, e
	}
	tags := []storageTag{}
	for key, values := range q {
		if len(values) != 1 {
			return nil, errors.New("duplicate tag")
		}
		tags = append(tags, storageTag{Key: key, Value: values[0]})
	}
	return normalizeStorageTags(tags, 10)
}
func writeStorageResponseHeaders(w http.ResponseWriter, input map[string]string) error {
	headers, e := normalizeStorageResponseHeaders(input)
	if e != nil {
		return e
	}
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	if len(headers) > 0 {
		w.Header().Add("Vary", "Authorization, Cookie")
	}
	return nil
}
func normalizeStorageObjectSettings(v storageObjectSettings) (storageObjectSettings, error) {
	var e error
	v.Tags, e = normalizeStorageTags(v.Tags, 10)
	if e != nil {
		return v, e
	}
	v.ResponseHeaders, e = normalizeStorageResponseHeaders(v.ResponseHeaders)
	return v, e
}
func storageObjectSettingsETag(branch, bucket, key, blob, digest string, v storageObjectSettings) string {
	raw, _ := json.Marshal([]any{"neon-storage-object-settings-v1", branch, bucket, key, blob, digest, v})
	return fmt.Sprintf(`"%x"`, sha256.Sum256(raw))
}
func storageObjectSettingsReceipt(branch, bucket, key, digest string, v storageObjectSettings) record {
	return record{"branch_id": branch, "bucket": bucket, "key": key, "sha256": digest, "settings": v}
}

func (s *server) storageObjectSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	bucket, key := r.PathValue("bucket"), r.URL.Query().Get("key")
	if !storageBucketName.MatchString(bucket) || !validStorageKey(key) {
		fail(w, r, 422, "invalid_object_key", "An exact bucket and object key are required")
		return
	}
	var input struct {
		Settings *storageObjectSettings `json:"settings"`
	}
	var originalKey, requestHash string
	if r.Method != http.MethodGet {
		if len(s.idempotencyKey) < 32 {
			fail(w, r, 503, "storage_configuration_unavailable", "Storage settings key is unavailable")
			return
		}
		var ok bool
		originalKey, ok = creationKey(w, r)
		if !ok {
			return
		}
		if readJSON(r, &input) != nil || input.Settings == nil {
			fail(w, r, 422, "invalid_object_settings", "Typed object settings are required")
			return
		}
		v, e := normalizeStorageObjectSettings(*input.Settings)
		if e != nil {
			fail(w, r, 422, "invalid_object_settings", "Object tags or response headers are invalid")
			return
		}
		input.Settings = &v
		requestHash = s.createRequestHash("storage_object_settings:"+r.PathValue("project")+":"+r.PathValue("branch")+":"+bucket+":"+key, record{"input": input, "etag": r.Header.Get("If-Match")})
	}
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, p, close, e := s.openObjectStorage(r, r.PathValue("project"), r.PathValue("branch"))
	if e != nil {
		s.storageFailure(w, r, e)
		return
	}
	defer close()
	response, etag, replayed, e := nativeStorageObjectSettings(r.Context(), conn, p, bucket, key, userFrom(r).ID, keyHash(originalKey), requestHash, r.Header.Get("If-Match"), input.Settings)
	if e != nil {
		s.storageSettingsFailure(w, r, e)
		return
	}
	w.Header().Set("ETag", etag)
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	jsonResponse(w, 200, response)
	if input.Settings != nil && !replayed {
		close()
		s.audit(r.Context(), userFrom(r).ID, "set_object_settings", "object_storage", p.BranchID, "succeeded", requestID(r))
	}
}
