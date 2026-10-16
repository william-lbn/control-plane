package triggers

import (
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

func ValidName(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) || len(value) > 1024 || utf8.RuneCountInString(value) > 256 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

// Application-relative canonical URI only. The dispatcher never accepts an
// arbitrary destination URL, query, fragment or normalization escape.
func ValidPath(value string) bool {
	if value == "" || len(value) > 2048 || !utf8.ValidString(value) || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "?#\\") {
		return false
	}
	u, err := url.ParseRequestURI(value)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Fragment != "" || u.RawQuery != "" {
		return false
	}
	for _, r := range u.Path {
		if unicode.IsControl(r) || r == '\\' {
			return false
		}
	}
	cleanable := strings.TrimSuffix(u.Path, "/")
	if cleanable == "" {
		return u.Path == "/"
	}
	return path.Clean(cleanable) == cleanable
}
