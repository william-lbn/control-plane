package s3protocol

import (
	"bytes"
	"crypto/hmac"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// POST policies bind every submitted field, the bucket and the credential scope.
// Specification: https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sigv4-HTTPPOSTConstructPolicy.html
type PostPolicy struct {
	fields, exact                           map[string]string
	encoded, signature, date, scope, region string
	expiry                                  time.Time
	minimum, maximum                        int64
}

func (p PostPolicy) KeyID() string            { return strings.Split(p.fields["x-amz-credential"], "/")[0] }
func (p PostPolicy) Field(name string) string { return p.fields[strings.ToLower(name)] }
func (p PostPolicy) ExactField(name, value string) bool {
	actual, ok := p.exact[strings.ToLower(name)]
	return ok && actual == value
}
func (p PostPolicy) Fields() map[string]string {
	out := map[string]string{}
	for k, v := range p.fields {
		out[k] = v
	}
	return out
}

func uniquePolicyJSON(raw []byte) bool {
	if len(raw) == 0 || len(raw) > 16384 || !utf8.Valid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var value func(int) bool
	value = func(depth int) bool {
		nodes++
		if depth > 12 || nodes > 1000 {
			return false
		}
		token, e := d.Token()
		if e != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				name, e := d.Token()
				key, ok := name.(string)
				if e != nil || !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}
func ParsePostPolicy(input map[string]string, bucket, filename, region string, now time.Time, limit int64) (PostPolicy, error) {
	p := PostPolicy{fields: map[string]string{}, exact: map[string]string{}, region: region, maximum: limit}
	if len(input) > 64 || limit < 0 || len(filename) > 1024 || !utf8.ValidString(filename) || strings.ContainsAny(filename, "\x00\r\n") {
		return p, ErrSignature
	}
	if at := strings.LastIndexAny(filename, "/\\"); at >= 0 {
		filename = filename[at+1:]
	}
	total := 0
	for key, value := range input {
		key = strings.ToLower(key)
		if _, exists := p.fields[key]; exists || !headerName.MatchString(strings.ReplaceAll(key, "_", "-")) || len(value) > 16384 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return p, ErrSignature
		}
		total += len(key) + len(value)
		if total > 32768 {
			return p, ErrSignature
		}
		p.fields[key] = strings.ReplaceAll(value, "${filename}", filename)
	}
	if existing, ok := p.fields["bucket"]; ok && existing != bucket {
		return p, ErrSignature
	}
	p.fields["bucket"] = bucket
	if p.Field("x-amz-algorithm") != "AWS4-HMAC-SHA256" || p.Field("x-amz-security-token") != "" {
		return p, ErrSignature
	}
	pieces := strings.Split(p.Field("x-amz-credential"), "/")
	p.date = p.Field("x-amz-date")
	p.signature = p.Field("x-amz-signature")
	p.encoded = p.Field("policy")
	if len(pieces) != 5 || !keyID.MatchString(pieces[0]) || pieces[2] != region || pieces[3] != "s3" || pieces[4] != "aws4_request" || len(p.date) != 16 || pieces[1] != p.date[:8] || !digestPattern.MatchString(p.signature) {
		return p, ErrSignature
	}
	p.scope = strings.Join(pieces[1:], "/")
	date, e := time.Parse("20060102T150405Z", p.date)
	if e != nil {
		return p, ErrSignature
	}
	raw, e := base64.StdEncoding.Strict().DecodeString(p.encoded)
	if e != nil || base64.StdEncoding.EncodeToString(raw) != p.encoded || !uniquePolicyJSON(raw) {
		return p, ErrSignature
	}
	var policy struct {
		Expiration string `json:"expiration"`
		Conditions []any  `json:"conditions"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(&policy) != nil || len(policy.Conditions) < 1 || len(policy.Conditions) > 100 {
		return p, ErrSignature
	}
	p.expiry, e = time.Parse(time.RFC3339Nano, policy.Expiration)
	if e != nil || !p.expiry.After(date) || p.expiry.After(date.Add(24*time.Hour)) || !now.Before(p.expiry) || now.Before(date.Add(-5*time.Minute)) {
		return p, ErrSignature
	}
	covered := map[string]bool{}
	match := func(name, expected, operation string) bool {
		name = strings.ToLower(name)
		actual, exists := p.fields[name]
		if !exists {
			return false
		}
		covered[name] = true
		if operation == "eq" {
			if previous, ok := p.exact[name]; ok && previous != expected {
				return false
			}
			p.exact[name] = expected
			return actual == expected
		}
		if operation != "starts-with" {
			return false
		}
		values := []string{actual}
		if name == "content-type" {
			values = strings.Split(actual, ",")
		}
		for _, v := range values {
			if !strings.HasPrefix(v, expected) {
				return false
			}
		}
		return true
	}
	for _, condition := range policy.Conditions {
		switch v := condition.(type) {
		case map[string]any:
			if len(v) < 1 || len(v) > 16 {
				return p, ErrSignature
			}
			for key, raw := range v {
				expected, ok := raw.(string)
				if !ok || !match(key, expected, "eq") {
					return p, ErrSignature
				}
			}
		case []any:
			if len(v) != 3 {
				return p, ErrSignature
			}
			operation, ok := v[0].(string)
			if !ok {
				return p, ErrSignature
			}
			if operation == "content-length-range" {
				a, okA := v[1].(json.Number)
				b, okB := v[2].(json.Number)
				if !okA || !okB {
					return p, ErrSignature
				}
				low, eA := a.Int64()
				high, eB := b.Int64()
				if eA != nil || eB != nil || low < 0 || high < low {
					return p, ErrSignature
				}
				if low > p.minimum {
					p.minimum = low
				}
				if high < p.maximum {
					p.maximum = high
				}
				if p.minimum > p.maximum {
					return p, ErrSignature
				}
				continue
			}
			name, okA := v[1].(string)
			expected, okB := v[2].(string)
			if !okA || !okB || !strings.HasPrefix(name, "$") || !match(strings.TrimPrefix(name, "$"), expected, operation) {
				return p, ErrSignature
			}
		default:
			return p, ErrSignature
		}
	}
	for key := range p.fields {
		if key == "policy" || key == "x-amz-signature" || strings.HasPrefix(key, "x-ignore-") {
			continue
		}
		if !covered[key] {
			return p, ErrSignature
		}
	}
	if p.Field("key") == "" {
		return p, ErrSignature
	}
	for _, name := range []string{"bucket", "x-amz-algorithm", "x-amz-credential", "x-amz-date"} {
		if !p.ExactField(name, p.Field(name)) {
			return p, ErrSignature
		}
	}
	return p, nil
}
func (p PostPolicy) Verify(secret string, size int64, now time.Time) error {
	if len(secret) < 32 || len(secret) > 256 || size < p.minimum || size > p.maximum || !now.Before(p.expiry) {
		return ErrSignature
	}
	signature := Signature{Date: p.date, Scope: p.scope}
	expected := hex.EncodeToString(mac(signature.signingKey(secret, p.region), p.encoded))
	if !hmac.Equal([]byte(expected), []byte(p.signature)) {
		return ErrSignature
	}
	return nil
}
