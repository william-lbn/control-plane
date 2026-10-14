package control

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type storageCORSRule struct {
	ID      string   `json:"id,omitempty" xml:"ID,omitempty"`
	Origins []string `json:"allowed_origins" xml:"AllowedOrigin"`
	Methods []string `json:"allowed_methods" xml:"AllowedMethod"`
	Headers []string `json:"allowed_headers,omitempty" xml:"AllowedHeader"`
	Expose  []string `json:"expose_headers,omitempty" xml:"ExposeHeader"`
	MaxAge  int      `json:"max_age_seconds,omitempty" xml:"MaxAgeSeconds,omitempty"`
}

var corsHeaderName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'+.^_` + "`" + `|~*-]+$`)

func validStorageCORS(rules []storageCORSRule) bool {
	if len(rules) > 100 {
		return false
	}
	raw, _ := json.Marshal(rules)
	if len(raw) > 65536 {
		return false
	}
	for _, r := range rules {
		if len(r.ID) > 255 || !utf8.ValidString(r.ID) || len(r.Origins) < 1 || len(r.Origins) > 16 || len(r.Methods) < 1 || len(r.Methods) > 5 || len(r.Headers) > 32 || len(r.Expose) > 32 || r.MaxAge < 0 || r.MaxAge > 86400 {
			return false
		}
		for _, origin := range r.Origins {
			if origin == "*" {
				continue
			}
			if strings.Count(origin, "*") > 1 {
				return false
			}
			u, e := url.Parse(strings.ReplaceAll(origin, "*", "wildcard"))
			if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(origin, "\r\n") {
				return false
			}
		}
		for _, m := range r.Methods {
			if m != "GET" && m != "HEAD" && m != "PUT" && m != "POST" && m != "DELETE" {
				return false
			}
		}
		for _, h := range r.Headers {
			if !corsHeaderName.MatchString(h) || strings.Count(h, "*") > 1 {
				return false
			}
		}
		for _, h := range r.Expose {
			if !corsHeaderName.MatchString(h) || strings.Contains(h, "*") {
				return false
			}
		}
	}
	return true
}
func storageCORSETag(rules []storageCORSRule) string {
	if rules == nil {
		rules = []storageCORSRule{}
	}
	raw, _ := json.Marshal(rules)
	return fmt.Sprintf(`"%x"`, sha256.Sum256(raw))
}
func corsPattern(pattern, value string) bool {
	parts := strings.SplitN(pattern, "*", 2)
	if len(parts) == 1 {
		return pattern == value
	}
	return strings.HasPrefix(value, parts[0]) && strings.HasSuffix(value, parts[1]) && len(value) >= len(parts[0])+len(parts[1])
}
func matchStorageCORS(rules []storageCORSRule, origin, method string, headers []string) *storageCORSRule {
	if origin == "" || origin == "null" || strings.ContainsAny(origin, "\r\n") {
		return nil
	}
	for _, r := range rules {
		originOK, methodOK := false, false
		for _, o := range r.Origins {
			if corsPattern(o, origin) {
				originOK = true
			}
		}
		for _, m := range r.Methods {
			if m == method {
				methodOK = true
			}
		}
		if !originOK || !methodOK {
			continue
		}
		all := true
		for _, h := range headers {
			found := false
			for _, a := range r.Headers {
				if corsPattern(strings.ToLower(a), strings.ToLower(strings.TrimSpace(h))) {
					found = true
				}
			}
			if !found {
				all = false
			}
		}
		if all {
			return &r
		}
	}
	return nil
}
func applyStorageCORS(w http.ResponseWriter, rule *storageCORSRule, origin string, headers []string, preflight bool) {
	w.Header().Add("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Origin", origin)
	if preflight {
		w.Header().Add("Vary", "Access-Control-Request-Method")
		w.Header().Add("Vary", "Access-Control-Request-Headers")
		w.Header().Set("Access-Control-Allow-Methods", strings.Join(rule.Methods, ", "))
		if len(headers) > 0 {
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(headers, ", "))
		}
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAge))
	} else if len(rule.Expose) > 0 {
		w.Header().Set("Access-Control-Expose-Headers", strings.Join(rule.Expose, ", "))
	}
	// S3 capabilities do not use Console cookies; never opt into credentialed CORS.
}
func loadStorageCORS(r *http.Request, conn *pgx.Conn, bucket string) ([]storageCORSRule, error) {
	var rules []storageCORSRule
	e := conn.QueryRow(r.Context(), `SELECT cors FROM neon_storage.buckets WHERE name=$1`, bucket).Scan(&rules)
	if rules == nil {
		rules = []storageCORSRule{}
	}
	return rules, e
}
func storeStorageCORS(r *http.Request, conn *pgx.Conn, bucket string, rules []storageCORSRule, expected string) error {
	tx, e := conn.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	if e = storageDirectoryLock(r.Context(), tx); e != nil {
		return e
	}
	var old []storageCORSRule
	if e = tx.QueryRow(r.Context(), `SELECT cors FROM neon_storage.buckets WHERE name=$1 FOR UPDATE`, bucket).Scan(&old); e != nil {
		return e
	}
	if expected != "" && expected != storageCORSETag(old) {
		return &storageMutationFault{412, "PreconditionFailed"}
	}
	if rules == nil {
		rules = []storageCORSRule{}
	}
	_, e = tx.Exec(r.Context(), `UPDATE neon_storage.buckets SET cors=$2 WHERE name=$1`, bucket, rules)
	if e == nil {
		e = tx.Commit(r.Context())
	}
	return e
}
func (s *server) storageBucketCORS(w http.ResponseWriter, r *http.Request) {
	var rules []storageCORSRule
	if r.Method == http.MethodPut {
		var input struct {
			Rules []storageCORSRule `json:"rules"`
		}
		if readJSON(r, &input) != nil || !validStorageCORS(input.Rules) {
			fail(w, r, 422, "invalid_cors", "Valid bounded origin/method/header rules required")
			return
		}
		rules = input.Rules
	}
	if r.Method != http.MethodGet && r.Header.Get("If-Match") == "" {
		fail(w, r, 428, "version_required", "Current CORS ETag required")
		return
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
	if r.Method == http.MethodGet {
		rules, e = loadStorageCORS(r, conn, r.PathValue("bucket"))
	} else {
		e = storeStorageCORS(r, conn, r.PathValue("bucket"), rules, r.Header.Get("If-Match"))
	}
	if e != nil {
		if f, ok := e.(*storageMutationFault); ok {
			fail(w, r, f.Status, "cors_version_conflict", "CORS changed; refresh before retrying")
		} else {
			s.storageFailure(w, r, e)
		}
		return
	}
	if rules == nil {
		rules = []storageCORSRule{}
	}
	w.Header().Set("ETag", storageCORSETag(rules))
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 200, record{"rules": rules, "branch_id": p.BranchID})
	if r.Method != http.MethodGet {
		close()
		s.audit(r.Context(), userFrom(r).ID, "set_bucket_cors", "object_storage", p.BranchID, "succeeded", requestID(r))
	}
}
func validCORSXML(data []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	root := false
	for {
		token, e := decoder.Token()
		if e == io.EOF {
			return root && depth == 0
		}
		if e != nil {
			return false
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Space != "" && t.Name.Space != s3XMLNamespace {
				return false
			}
			if depth == 1 {
				if root || t.Name.Local != "CORSConfiguration" {
					return false
				}
				root = true
			} else if depth == 2 {
				if t.Name.Local != "CORSRule" {
					return false
				}
			} else if depth == 3 {
				switch t.Name.Local {
				case "ID", "AllowedOrigin", "AllowedMethod", "AllowedHeader", "ExposeHeader", "MaxAgeSeconds":
				default:
					return false
				}
			} else {
				return false
			}
			for _, a := range t.Attr {
				if a.Name.Local != "xmlns" && a.Name.Space != "xmlns" {
					return false
				}
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth < 3 && strings.TrimSpace(string(t)) != "" {
				return false
			}
		case xml.Directive:
			return false
		}
	}
}
func (s *server) s3CORSConfiguration(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, bucket string, data []byte) {
	var config struct {
		XMLName xml.Name          `xml:"CORSConfiguration"`
		XMLNS   string            `xml:"xmlns,attr,omitempty"`
		Rules   []storageCORSRule `xml:"CORSRule"`
	}
	switch r.Method {
	case http.MethodGet:
		rules, e := loadStorageCORS(r, conn, bucket)
		if e != nil {
			if isNoRows(e) {
				s3Error(w, r, 404, "NoSuchBucket")
			} else {
				s3Error(w, r, 503, "ServiceUnavailable")
			}
			return
		}
		if len(rules) == 0 {
			s3Error(w, r, 404, "NoSuchCORSConfiguration")
			return
		}
		config.XMLNS = s3XMLNamespace
		config.Rules = rules
		s3XML(w, 200, config)
		return
	case http.MethodPut:
		sum := md5.Sum(data)
		if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
			s3Error(w, r, 400, "BadDigest")
			return
		}
		if len(data) > 65536 || len(data) == 0 {
			s3Error(w, r, 400, "MalformedXML")
			return
		}
		if !validCORSXML(data) {
			s3Error(w, r, 400, "MalformedXML")
			return
		}
		decoder := xml.NewDecoder(bytes.NewReader(data))
		if decoder.Decode(&config) != nil || config.XMLName.Local != "CORSConfiguration" || (config.XMLName.Space != "" && config.XMLName.Space != s3XMLNamespace) || !validStorageCORS(config.Rules) || len(config.Rules) == 0 {
			s3Error(w, r, 400, "MalformedXML")
			return
		}
	case http.MethodDelete:
	default:
		s3Error(w, r, 405, "MethodNotAllowed")
		return
	}
	if e := storeStorageCORS(r, conn, bucket, config.Rules, ""); e != nil {
		if isNoRows(e) {
			s3Error(w, r, 404, "NoSuchBucket")
		} else {
			s3Error(w, r, 503, "ServiceUnavailable")
		}
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(200)
	}
}
func (s *server) s3Preflight(w http.ResponseWriter, r *http.Request) {
	pieces := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/s3/"), "/", 3)
	if !strings.HasPrefix(r.URL.Path, "/s3/") || len(pieces) < 2 || !dataBranchID.MatchString(pieces[0]) || !storageBucketName.MatchString(pieces[1]) {
		s3Error(w, r, 403, "AccessForbidden")
		return
	}
	requested := []string{}
	if value := r.Header.Get("Access-Control-Request-Headers"); value != "" {
		if len(value) > 2048 {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		requested = strings.Split(value, ",")
		if len(requested) > 32 {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		for i, h := range requested {
			requested[i] = strings.ToLower(strings.TrimSpace(h))
			if !corsHeaderName.MatchString(requested[i]) || strings.Contains(requested[i], "*") {
				s3Error(w, r, 400, "InvalidRequest")
				return
			}
		}
	}
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, _, close, e := s.openObjectStorage(r, "", pieces[0])
	if e != nil {
		s3Error(w, r, 403, "AccessForbidden")
		return
	}
	defer close()
	rules, e := loadStorageCORS(r, conn, pieces[1])
	rule := matchStorageCORS(rules, r.Header.Get("Origin"), r.Header.Get("Access-Control-Request-Method"), requested)
	if e != nil || rule == nil {
		s3Error(w, r, 403, "AccessForbidden")
		return
	}
	applyStorageCORS(w, rule, r.Header.Get("Origin"), requested, true)
	w.WriteHeader(200)
}
