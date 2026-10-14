// Package s3protocol verifies bounded S3 SigV4 requests. It never follows URLs,
// resolves DNS, reads credentials or performs an object mutation. Credential
// lifecycle/lineage/scope and payload size are enforced by the admitting Driver.
package s3protocol

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrSignature = errors.New("S3 signature rejected")
var keyID = regexp.MustCompile(`^bcr_[a-f0-9]{24}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var headerName = regexp.MustCompile(`^[a-z0-9-]+$`)

type Signature struct {
	KeyID, Date, Scope, Headers, Value string
	Presigned                          bool
	Expires                            int
}

// Parse rejects ambiguous authentication, duplicate control query parameters,
// temporary credentials, unsupported authentication algorithms and expired dates.
func Parse(r *http.Request, region string, now time.Time) (Signature, error) {
	var out Signature
	if len(r.URL.RawQuery) > 16<<10 || len(r.Header.Get("Authorization")) > 2048 || len(r.Header.Values("Authorization")) > 1 || r.Header.Get("X-Amz-Security-Token") != "" {
		return out, ErrSignature
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return out, ErrSignature
	}
	for _, v := range q {
		// S3 operations use scalar query controls. Reject duplicate parameters
		// rather than accepting differing SDK/proxy interpretations of ordering.
		if len(v) != 1 {
			return out, ErrSignature
		}
	}
	credential := ""
	if q.Get("X-Amz-Algorithm") != "" {
		if r.Header.Get("Authorization") != "" || q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || q.Get("X-Amz-Security-Token") != "" {
			return out, ErrSignature
		}
		out.Presigned = true
		credential = q.Get("X-Amz-Credential")
		out.Date = q.Get("X-Amz-Date")
		out.Headers = q.Get("X-Amz-SignedHeaders")
		out.Value = q.Get("X-Amz-Signature")
		out.Expires, e = strconv.Atoi(q.Get("X-Amz-Expires"))
		if e != nil || out.Expires < 1 || out.Expires > 86400 {
			return out, ErrSignature
		}
	} else {
		authorization := r.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 ") {
			return out, ErrSignature
		}
		entries := strings.Split(strings.TrimPrefix(authorization, "AWS4-HMAC-SHA256 "), ",")
		if len(entries) != 3 {
			return out, ErrSignature
		}
		values := map[string]string{}
		for _, entry := range entries {
			k, v, ok := strings.Cut(strings.TrimSpace(entry), "=")
			if !ok || values[k] != "" {
				return out, ErrSignature
			}
			values[k] = v
		}
		if len(values) != 3 {
			return out, ErrSignature
		}
		credential = values["Credential"]
		out.Headers = values["SignedHeaders"]
		out.Value = values["Signature"]
		out.Date = r.Header.Get("X-Amz-Date")
	}
	pieces := strings.Split(credential, "/")
	if len(pieces) != 5 || !keyID.MatchString(pieces[0]) || pieces[2] != region || pieces[3] != "s3" || pieces[4] != "aws4_request" || len(out.Date) != 16 || pieces[1] != out.Date[:8] || !digestPattern.MatchString(out.Value) {
		return out, ErrSignature
	}
	out.KeyID = pieces[0]
	out.Scope = strings.Join(pieces[1:], "/")
	date, e := time.Parse("20060102T150405Z", out.Date)
	if e != nil {
		return out, ErrSignature
	}
	if out.Presigned {
		if now.Before(date.Add(-5*time.Minute)) || !now.Before(date.Add(time.Duration(out.Expires)*time.Second)) {
			return out, ErrSignature
		}
	} else if date.Before(now.Add(-5*time.Minute)) || date.After(now.Add(5*time.Minute)) {
		return out, ErrSignature
	}
	names := strings.Split(out.Headers, ";")
	seen := map[string]bool{}
	previous := ""
	if len(names) > 32 {
		return out, ErrSignature
	}
	for _, name := range names {
		if !headerName.MatchString(name) || name <= previous || seen[name] {
			return out, ErrSignature
		}
		seen[name] = true
		previous = name
	}
	if !seen["host"] || (!out.Presigned && !seen["x-amz-date"]) {
		return out, ErrSignature
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") && lower != "x-amz-content-sha256" && !seen[lower] {
			return out, ErrSignature
		}
	}
	return out, nil
}

func Encode(value string, slash bool) string {
	const hexadecimal = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-._~", rune(b)) || slash && b == '/' {
			out.WriteByte(b)
		} else {
			out.WriteByte('%')
			out.WriteByte(hexadecimal[b>>4])
			out.WriteByte(hexadecimal[b&15])
		}
	}
	return out.String()
}
func canonicalQuery(r *http.Request, presigned bool) (string, error) {
	values, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return "", ErrSignature
	}
	var pairs []string
	for name, items := range values {
		if presigned && name == "X-Amz-Signature" {
			continue
		}
		for _, value := range items {
			pairs = append(pairs, Encode(name, false)+"="+Encode(value, false))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&"), nil
}
func Hash(body []byte) string { value := sha256.Sum256(body); return hex.EncodeToString(value[:]) }
func mac(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return h.Sum(nil)
}

// Verify binds the original method/path/query/authority and signed headers.
// UNSIGNED-PAYLOAD is accepted for bodyless reads and a bodyless COPY whose
// source header is signed, plus query-presigned PUT (standard S3 capability).
// Normal header-signed uploads/deletes still require payload hashes.
// Signed chunk payloads must use VerifyAndDecode, before any object mutation.
func (s Signature) Verify(r *http.Request, body []byte, secret, region string) error {
	if len(s.Date) != 16 || len(secret) < 32 || len(secret) > 256 || !keyID.MatchString(s.KeyID) || !digestPattern.MatchString(s.Value) {
		return ErrSignature
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if s.Presigned {
		payload = "UNSIGNED-PAYLOAD"
	}
	if payload == "UNSIGNED-PAYLOAD" {
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		copy := r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "" && strings.Contains(";"+s.Headers+";", ";x-amz-copy-source;")
		// Query signatures authorize a bounded PUT capability. Ordinary header
		// signatures still require a payload hash; DELETE/POST remain excluded.
		upload := s.Presigned && r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") == ""
		// MinIO SetBucketCors signs Content-MD5 and uses UNSIGNED-PAYLOAD.
		// Limit this exception to bounded XML configuration, never object PUT.
		checksum := md5.Sum(body)
		cors := r.Method == http.MethodPut && r.URL.Query().Has("cors") && len(body) > 0 && len(body) <= 65536 && r.Header.Get("X-Amz-Copy-Source") == "" && strings.Contains(";"+s.Headers+";", ";content-md5;") && r.Header.Get("Content-MD5") == base64.StdEncoding.EncodeToString(checksum[:])
		if !upload && !cors && ((!read && !copy) || len(body) != 0) {
			return ErrSignature
		}
	} else if !digestPattern.MatchString(payload) || !hmac.Equal([]byte(payload), []byte(Hash(body))) {
		return ErrSignature
	}
	return s.verifyCanonical(r, payload, secret, region)
}
func (s Signature) signingKey(secret, region string) []byte {
	key := mac([]byte("AWS4"+secret), s.Date[:8])
	key = mac(key, region)
	key = mac(key, "s3")
	return mac(key, "aws4_request")
}
func (s Signature) verifyCanonical(r *http.Request, payload, secret, region string) error {
	query, e := canonicalQuery(r, s.Presigned)
	if e != nil {
		return e
	}
	var headers strings.Builder
	for _, name := range strings.Split(s.Headers, ";") {
		values := r.Header.Values(name)
		if name == "host" {
			values = []string{r.Host}
		}
		if len(values) == 0 || len(values) > 16 {
			return ErrSignature
		}
		for i, v := range values {
			if strings.ContainsAny(v, "\r\n\x00") {
				return ErrSignature
			}
			values[i] = strings.Join(strings.Fields(v), " ")
		}
		headers.WriteString(name + ":" + strings.Join(values, ",") + "\n")
	}
	uri := Encode(r.URL.Path, true)
	if uri == "" {
		uri = "/"
	}
	canonical := r.Method + "\n" + uri + "\n" + query + "\n" + headers.String() + "\n" + s.Headers + "\n" + payload
	signing := s.signingKey(secret, region)
	expected := hex.EncodeToString(mac(signing, "AWS4-HMAC-SHA256\n"+s.Date+"\n"+s.Scope+"\n"+Hash([]byte(canonical))))
	if !hmac.Equal([]byte(expected), []byte(s.Value)) {
		return ErrSignature
	}
	return nil
}
