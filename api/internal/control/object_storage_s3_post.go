package control

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/s3protocol"
)

type s3PostStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w s3PostStatusWriter) WriteHeader(status int) {
	if status == http.StatusOK {
		status = w.status
	}
	w.ResponseWriter.WriteHeader(status)
}

func s3PostBranchAllowed(policy s3protocol.PostPolicy, branch, anchor string) bool {
	return (branch == anchor || policy.ExactField("x-neon-branch", branch)) && (policy.Field("x-neon-branch") == "" || policy.ExactField("x-neon-branch", branch))
}

func (s *server) s3BrowserPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		return false
	}
	if r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
		s3Error(w, r, 403, "SignatureDoesNotMatch")
		return true
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	branch, bucket := "", ""
	if len(parts) == 1 {
		bucket = parts[0]
	} else if len(parts) == 3 && parts[0] == "s3" && dataBranchID.MatchString(parts[1]) {
		branch, bucket = parts[1], parts[2]
	}
	if !storageBucketName.MatchString(bucket) {
		s3Error(w, r, 400, "InvalidURI")
		return true
	}
	if r.ContentLength > storageObjectLimit+(128<<10) {
		s3Error(w, r, 413, "EntityTooLarge")
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, storageObjectLimit+(128<<10))
	reader, e := r.MultipartReader()
	if e != nil {
		s3Error(w, r, 400, "MalformedPOSTRequest")
		return true
	}
	fields := map[string]string{}
	var payload *storagePayload
	filename := ""
	count, total := 0, 0
	defer func() {
		if payload != nil {
			payload.Close()
		}
	}()
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil || payload != nil {
			s3Error(w, r, 400, "MalformedPOSTRequest")
			return true
		}
		count++
		if count > 65 {
			s3Error(w, r, 413, "EntityTooLarge")
			return true
		}
		name := strings.ToLower(part.FormName())
		if name == "file" {
			filename = part.FileName()
			payload, e = readStoragePayload(r.Context(), part, storageObjectLimit)
			part.Close()
			if e != nil {
				s.storagePayloadFailure(w, r, e, true)
				return true
			}
			continue
		}
		if _, exists := fields[name]; exists || name == "" {
			part.Close()
			s3Error(w, r, 400, "MalformedPOSTRequest")
			return true
		}
		raw, e := io.ReadAll(io.LimitReader(part, 16385))
		part.Close()
		if e != nil || len(raw) > 16384 {
			s3Error(w, r, 400, "MalformedPOSTRequest")
			return true
		}
		total += len(raw)
		if total > 32768 {
			s3Error(w, r, 413, "EntityTooLarge")
			return true
		}
		fields[name] = string(raw)
	}
	if payload == nil {
		s3Error(w, r, 400, "MalformedPOSTRequest")
		return true
	}
	policy, e := s3protocol.ParsePostPolicy(fields, bucket, filename, s3Region(), time.Now(), storageObjectLimit)
	if e != nil {
		s3Error(w, r, 403, "SignatureDoesNotMatch")
		return true
	}
	token, anchor, e := s.loadStorageSigning(r.Context(), policy.KeyID())
	if e != nil {
		s3Error(w, r, 403, "InvalidAccessKeyId")
		return true
	}
	if branch == "" {
		branch = anchor
	}
	if policy.Verify(storageSigningSecret(token), payload.Size, time.Now()) != nil {
		s3Error(w, r, 403, "SignatureDoesNotMatch")
		return true
	}
	// Unlike a normal SigV4 request, a POST policy does not sign the URL path.
	// Repeated bucket names across branches need an exact additional binding.
	if !s3PostBranchAllowed(policy, branch, anchor) {
		s3Error(w, r, 403, "AccessDenied")
		return true
	}
	if _, e = s.verifyBackendCredential(r.Context(), token, branch, "storage:write", ""); e != nil {
		s3Error(w, r, 403, "AccessDenied")
		return true
	}
	key := policy.Field("key")
	if !validStorageKey(key) {
		s3Error(w, r, 400, "InvalidArgument")
		return true
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/octet-stream")
	status := 204
	for name, value := range policy.Fields() {
		switch name {
		case "bucket", "key", "policy", "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-signature", "x-neon-branch":
		case "success_action_status":
			if value != "200" && value != "201" && value != "204" {
				s3Error(w, r, 400, "InvalidArgument")
				return true
			}
			status, _ = strconv.Atoi(value)
		case "content-type", "content-disposition", "cache-control", "content-md5", "x-amz-checksum-sha256", "x-amz-checksum-sha1", "x-amz-checksum-crc32", "x-amz-checksum-crc32c", "x-amz-checksum-crc64nvme", "x-amz-sdk-checksum-algorithm":
			headers.Set(name, value)
		case "acl":
			if value != "private" {
				s3Error(w, r, 501, "NotImplemented")
				return true
			}
		case "x-amz-tagging":
			headers.Set(name, value)
		case "tagging":
			var v struct {
				Tags []storageTag `xml:"TagSet>Tag"`
			}
			if policy.Field("x-amz-tagging") != "" || !decodeStorageConfiguration([]byte(value), "Tagging", &v) {
				s3Error(w, r, 400, "InvalidTag")
				return true
			}
			tags, e := normalizeStorageTags(v.Tags, 10)
			if e != nil {
				s3Error(w, r, 400, "InvalidTag")
				return true
			}
			encoded := url.Values{}
			for _, tag := range tags {
				encoded.Set(tag.Key, tag.Value)
			}
			headers.Set("X-Amz-Tagging", encoded.Encode())
		default:
			if strings.HasPrefix(name, "x-amz-meta-") {
				headers.Set(name, value)
			} else if !strings.HasPrefix(name, "x-ignore-") {
				s3Error(w, r, 501, "NotImplemented")
				return true
			}
		}
	}
	request := r.Clone(r.Context())
	request.Method = http.MethodPut
	request.Header = headers
	if _, e = storageTagsHeader(request); e != nil {
		s3Error(w, r, 400, "InvalidTag")
		return true
	}
	if _, e = storageResponseHeaders(request); e != nil {
		s3Error(w, r, 400, "InvalidArgument")
		return true
	}
	if _, e = metadataFromHeaders(request, true); e != nil {
		s3Error(w, r, 400, "InvalidArgument")
		return true
	}
	if e = validateStoragePayloadChecksums(headers, payload); e != nil {
		s.storagePayloadFailure(w, r, e, true)
		return true
	}
	if expected := r.Header.Get("X-Amz-Expected-Bucket-Owner"); expected != "" && expected != branch {
		s3Error(w, r, 403, "AccessDenied")
		return true
	}
	conn, p, close, e := s.openObjectStorage(r, "", branch)
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return true
	}
	defer close()
	if origin := r.Header.Get("Origin"); origin != "" {
		rules, e := loadStorageCORS(r, conn, bucket)
		if e == nil {
			if rule := matchStorageCORS(rules, origin, r.Method, nil); rule != nil {
				applyStorageCORS(w, rule, origin, nil, false)
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if status == 201 {
		w.Header().Set("Content-Type", "application/xml")
	}
	output := s3PostStatusWriter{ResponseWriter: w, status: status}
	if !s.s3WritePayload(output, request, conn, p, bucket, key, payload) {
		return true
	}
	if status == 201 {
		body, _ := xml.Marshal(struct {
			XMLName  xml.Name `xml:"PostResponse"`
			Location string   `xml:"Location"`
			Bucket   string   `xml:"Bucket"`
			Key      string   `xml:"Key"`
			ETag     string   `xml:"ETag"`
		}{Location: s3Origin() + "/s3/" + branch + "/" + url.PathEscape(bucket) + "/" + s3protocol.Encode(key, true), Bucket: bucket, Key: key, ETag: w.Header().Get("ETag")})
		w.Write(append([]byte(xml.Header), body...))
	}
	close()
	s.db.Exec(r.Context(), `UPDATE backend_credentials SET last_used_at=now() WHERE id=$1`, policy.KeyID())
	return true
}
