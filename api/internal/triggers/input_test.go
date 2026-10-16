package triggers

import (
	"strings"
	"testing"
)

func TestTriggerNamesAndCanonicalApplicationPaths(t *testing.T) {
	for _, name := range []string{"schedule", "每分钟检查", strings.Repeat("中", 256)} {
		if !ValidName(name) {
			t.Fatal("valid name rejected")
		}
	}
	for _, name := range []string{"", " name", "name ", "bad\nname", strings.Repeat("x", 257)} {
		if ValidName(name) {
			t.Fatal("invalid name admitted")
		}
	}
	for _, value := range []string{"/", "/tick", "/tick/", "/report%20name", "/%E6%B5%8B%E8%AF%95"} {
		if !ValidPath(value) {
			t.Fatalf("valid application path rejected: %s", value)
		}
	}
	for _, value := range []string{"", "tick", "https://target.invalid/", "//target.invalid/path", "/tick?x=1", "/tick#fragment", "/a/../b", "/a/%2e%2e/b", "/a\\b", "/a%5cb", "/%00", "/%0d%0a", "//", "/a//b"} {
		if ValidPath(value) {
			t.Fatalf("unsafe path admitted: %s", value)
		}
	}
}
