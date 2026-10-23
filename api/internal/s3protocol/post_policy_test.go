package s3protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/signer"
)

const policyID = "bcr_0123456789abcdef01234567"
const policySecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const policyRegion = "us-east-2"

func actualSDKPostFields(t *testing.T) map[string]string {
	t.Helper()
	client, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(policyID, policySecret, ""), Region: policyRegion})
	if e != nil {
		t.Fatal(e)
	}
	p := minio.NewPostPolicy()
	for _, e := range []error{p.SetBucket("ci-s3-post"), p.SetKeyStartsWith("forms/"), p.SetExpires(time.Now().UTC().Add(10 * time.Minute)), p.SetContentLengthRange(1, 4096), p.SetContentTypeStartsWith("image/"), p.SetUserMetadata("purpose", "post-policy"), p.SetSuccessStatusAction("201")} {
		if e != nil {
			t.Fatal(e)
		}
	}
	_, fields, e := client.PresignedPostPolicy(context.Background(), p)
	if e != nil {
		t.Fatal(e)
	}
	fields["key"] = "forms/${filename}"
	fields["Content-Type"] = "image/png"
	return fields
}
func clonePostFields(input map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range input {
		out[k] = v
	}
	return out
}
func rewritePostPolicy(t *testing.T, fields map[string]string, mutate func(map[string]any)) {
	t.Helper()
	raw, e := base64.StdEncoding.DecodeString(fields["policy"])
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		t.Fatal("SDK policy JSON invalid")
	}
	mutate(doc)
	raw, e = json.Marshal(doc)
	if e != nil {
		t.Fatal(e)
	}
	fields["policy"] = base64.StdEncoding.EncodeToString(raw)
	date, e := time.Parse("20060102T150405Z", fields["x-amz-date"])
	if e != nil {
		t.Fatal(e)
	}
	fields["x-amz-signature"] = signer.PostPresignSignatureV4(fields["policy"], date, policySecret, policyRegion)
}
func TestPostPolicyActualSDKConditionsAndFilename(t *testing.T) {
	fields := actualSDKPostFields(t)
	p, e := ParsePostPolicy(fields, "ci-s3-post", `C:\fakepath\中文.png`, policyRegion, time.Now(), 4096)
	if e != nil || p.Verify(policySecret, 37, time.Now()) != nil || p.Field("key") != "forms/中文.png" || p.KeyID() != policyID {
		t.Fatal("actual SDK POST policy or filename expansion rejected", e)
	}
	copy := p.Fields()
	copy["key"] = "changed"
	if p.Field("key") == "changed" {
		t.Fatal("mutable policy fields exposed")
	}
	for _, size := range []int64{0, 4097} {
		if p.Verify(policySecret, size, time.Now()) == nil {
			t.Fatal("POST content length condition bypassed")
		}
	}
	if p.Verify(strings.Repeat("z", 64), 37, time.Now()) == nil || p.Verify(policySecret, 37, time.Now().Add(time.Hour)) == nil {
		t.Fatal("wrong secret or expired capability accepted")
	}
	multi := clonePostFields(fields)
	rewritePostPolicy(t, multi, func(doc map[string]any) {
		doc["conditions"] = append(doc["conditions"].([]any), map[string]string{"bucket": "ci-s3-post", "x-amz-meta-purpose": "post-policy"})
	})
	if _, e = ParsePostPolicy(multi, "ci-s3-post", "file.png", policyRegion, time.Now(), 4096); e != nil {
		t.Fatal("combined exact conditions rejected")
	}
}
func TestPostPolicyRejectsUnboundAmbiguousExpiredAndChangedFields(t *testing.T) {
	original := actualSDKPostFields(t)
	cases := map[string]func(map[string]string){"signature": func(f map[string]string) { f["x-amz-signature"] = strings.Repeat("0", 64) }, "key": func(f map[string]string) { f["key"] = "outside/file.png" }, "content-type-list": func(f map[string]string) { f["Content-Type"] = "image/png,text/plain" }, "unbound-field": func(f map[string]string) { f["Cache-Control"] = "public" }, "case-duplicate": func(f map[string]string) { f["X-Amz-Date"] = f["x-amz-date"] }, "metadata": func(f map[string]string) { f["x-amz-meta-purpose"] = "changed" }, "wrong-bucket": func(f map[string]string) { f["bucket"] = "ci-s3-other" }, "sts": func(f map[string]string) { f["x-amz-security-token"] = "temporary" }, "expired": func(f map[string]string) {
		rewritePostPolicy(t, f, func(doc map[string]any) { doc["expiration"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) })
	}, "noninteger-length": func(f map[string]string) {
		rewritePostPolicy(t, f, func(doc map[string]any) {
			doc["conditions"] = append(doc["conditions"].([]any), []any{"content-length-range", 1.5, 100})
		})
	}, "duplicate-json": func(f map[string]string) {
		raw, _ := base64.StdEncoding.DecodeString(f["policy"])
		raw = append([]byte(`{"expiration":"2099-01-01T00:00:00Z",`), raw[1:]...)
		f["policy"] = base64.StdEncoding.EncodeToString(raw)
	}}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := clonePostFields(original)
			mutate(f)
			p, e := ParsePostPolicy(f, "ci-s3-post", "file.png", policyRegion, time.Now(), 4096)
			if e == nil && p.Verify(policySecret, 37, time.Now()) == nil {
				t.Fatal("invalid POST capability accepted")
			}
		})
	}
	if _, e := ParsePostPolicy(original, "ci-s3-post", "file.png", policyRegion, time.Now().Add(-time.Hour), 4096); e == nil {
		t.Fatal("future capability accepted")
	}
}
