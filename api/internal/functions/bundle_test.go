package functions

import (
	"archive/zip"
	"bytes"
	"os"
	"strings"
	"testing"
)

func archiveFor(t *testing.T, names ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, name := range names {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(`export default { fetch: () => new Response("ok") };`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestBundleEntryAndDigest(t *testing.T) {
	for _, entry := range []string{"index.mjs", "index.js"} {
		archive := archiveFor(t, "assets/test.txt", entry)
		bundle, err := ValidateBundle(archive)
		if err != nil || bundle.Entry != entry || len(bundle.Digest) != 64 || bundle.ExpandedBytes == 0 || len(bundle.Files) != 2 {
			t.Fatalf("valid bundle rejected: %v", err)
		}
		reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			t.Fatal(err)
		}
		offset, err := reader.File[0].DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		archive[offset] ^= 1
		if _, err := ValidateBundle(archive); err == nil {
			t.Fatal("damaged archive accepted")
		}
	}
}

func TestUnsafeBundlePaths(t *testing.T) {
	for _, name := range []string{"../escape", "..", "/absolute", "C:/windows", "a\\b", "a/../b", ".neon/config", ".neon", "bad\x00path", "bad\tpath", "."} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			if _, err := ValidateBundle(archiveFor(t, "index.mjs", name)); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, names := range [][]string{{"index.mjs", "index.mjs"}, {"index.mjs", "index.js"}, {"package.json"}, {"index.mjs", "asset", "asset/child"}} {
		if _, err := ValidateBundle(archiveFor(t, names...)); err == nil {
			t.Fatalf("invalid entries accepted: %v", names)
		}
	}
}

func TestBundleSymlinkAndCompressionBomb(t *testing.T) {
	for _, symlink := range []bool{true, false} {
		var out bytes.Buffer
		w := zip.NewWriter(&out)
		header := &zip.FileHeader{Name: "index.mjs", Method: zip.Deflate}
		if symlink {
			header.SetMode(os.ModeSymlink | 0777)
		}
		f, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		content := []byte("/run/neon/config")
		if !symlink {
			content = bytes.Repeat([]byte("a"), MaxFileBytes+1)
		}
		if _, err = f.Write(content); err != nil {
			t.Fatal(err)
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = ValidateBundle(out.Bytes()); err == nil {
			t.Fatal("unsafe ZIP accepted")
		}
	}
}

func TestEnvironmentNeverPermitsPlatformOverride(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "node_options", "NODE_PATH", "PGPASSWORD", "NEON_MANAGER_KEY", "PLATFORM_TOKEN", "PATH", "PORT", "BAD-NAME"} {
		err := ValidateEnvironment(map[string]string{name: "secret-must-not-appear"})
		if err == nil || strings.Contains(err.Error(), "secret-must-not-appear") {
			t.Fatalf("reserved environment accepted or exposed: %s", name)
		}
	}
	if err := ValidateEnvironment(map[string]string{"CUSTOM_TOKEN": "test", "APP_MODE": "production"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnvironment(nil); err == nil {
		t.Fatal("null environment accepted")
	}
	if err := ValidateEnvironment(map[string]string{"APP_KEY": strings.Repeat("a", 8193)}); err == nil {
		t.Fatal("oversized value accepted")
	}
}

func TestFunctionSlugContract(t *testing.T) {
	for _, slug := range []string{"hello", "a1", "12345678901234567890"} {
		if !ValidSlug(slug) {
			t.Fatal("valid slug rejected")
		}
	}
	for _, slug := range []string{"", "Hello", "a-b", "a_b", "123456789012345678901", "../a"} {
		if ValidSlug(slug) {
			t.Fatal("invalid slug accepted")
		}
	}
}
