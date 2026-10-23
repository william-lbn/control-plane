package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/s3protocol"
)

func TestS3PostBranchCapabilityBinding(t *testing.T) {
	anchor := "br_0123456789abcdef"
	child := "br_1123456789abcdef"
	policy := func(condition any, branch string) s3protocol.PostPolicy {
		t.Helper()
		now := time.Now().UTC()
		fields := map[string]string{"key": "forms/file", "bucket": "ci-post", "x-amz-algorithm": "AWS4-HMAC-SHA256", "x-amz-credential": "bcr_0123456789abcdef01234567/" + now.Format("20060102") + "/us-east-2/s3/aws4_request", "x-amz-date": now.Format("20060102T150405Z"), "x-amz-signature": strings.Repeat("a", 64)}
		conditions := []any{}
		for k, v := range fields {
			if k != "x-amz-signature" {
				conditions = append(conditions, map[string]string{k: v})
			}
		}
		if condition != nil {
			fields["x-neon-branch"] = branch
			conditions = append(conditions, condition)
		}
		raw, _ := json.Marshal(map[string]any{"expiration": now.Add(time.Minute).Format(time.RFC3339), "conditions": conditions})
		fields["policy"] = base64.StdEncoding.EncodeToString(raw)
		p, e := s3protocol.ParsePostPolicy(fields, "ci-post", "file", "us-east-2", now, 1024)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	original := policy(nil, "")
	if !s3PostBranchAllowed(original, anchor, anchor) || s3PostBranchAllowed(original, child, anchor) {
		t.Fatal("unsigned URL widened original POST policy to descendant")
	}
	exact := policy(map[string]string{"x-neon-branch": child}, child)
	if !s3PostBranchAllowed(exact, child, anchor) || s3PostBranchAllowed(exact, anchor, anchor) {
		t.Fatal("exact POST target not enforced")
	}
	prefix := policy([]string{"starts-with", "$x-neon-branch", "br_"}, child)
	if s3PostBranchAllowed(prefix, child, anchor) {
		t.Fatal("prefix policy accepted as exact branch binding")
	}
}

func TestS3PostRejectsMalformedFormsBeforeDatabaseAndErasesBuffers(t *testing.T) {
	t.Setenv("NEON_S3_PUBLIC_ORIGIN", "http://s3.test")
	directory := t.TempDir()
	t.Setenv("NEON_STORAGE_BUFFER_DIR", directory)
	s := &server{}
	cases := []struct {
		name, path                     string
		fields                         [][2]string
		file, lastField, authorization bool
		status                         int
	}{
		{name: "unsigned-file", fields: [][2]string{{"key", "forms/key"}}, file: true, status: 403},
		{name: "duplicate-field", fields: [][2]string{{"key", "one"}, {"KEY", "two"}}, file: true, status: 400},
		{name: "file-not-last", fields: [][2]string{{"key", "forms/key"}}, file: true, lastField: true, status: 400},
		{name: "missing-file", fields: [][2]string{{"key", "forms/key"}}, status: 400},
		{name: "object-path", path: "/s3/br_0123456789abcdef/ci-post/key", file: true, status: 400},
		{name: "query-auth", path: "/s3/br_0123456789abcdef/ci-post?X-Amz-Date=untrusted", file: true, status: 403},
		{name: "mixed-auth", file: true, authorization: true, status: 403},
		{name: "field-limit", fields: [][2]string{{"key", strings.Repeat("x", 16385)}}, file: true, status: 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for _, f := range c.fields {
				if e := writer.WriteField(f[0], f[1]); e != nil {
					t.Fatal(e)
				}
			}
			if c.file {
				part, e := writer.CreateFormFile("file", "file.txt")
				if e != nil {
					t.Fatal(e)
				}
				part.Write([]byte("owned temporary POST bytes"))
			}
			if c.lastField {
				writer.WriteField("after", "forbidden")
			}
			writer.Close()
			path := c.path
			if path == "" {
				path = "/s3/br_0123456789abcdef/ci-post"
			}
			r := httptest.NewRequest(http.MethodPost, "http://s3.test"+path, &body)
			r.Header.Set("Content-Type", writer.FormDataContentType())
			if c.authorization {
				r.Header.Set("Authorization", "mixed")
			}
			w := httptest.NewRecorder()
			s.s3Handler().ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatal("POST rejected at unexpected stage", w.Code, w.Body.String())
			}
			files, e := os.ReadDir(directory)
			if e != nil || len(files) != 0 || len(storagePayloadSlots) != 0 {
				t.Fatal("failed browser upload retained plaintext or capacity")
			}
		})
	}
}
