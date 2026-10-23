package s3protocol

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/minio/minio-go/v7/pkg/signer"
	"github.com/minio/minio-go/v7/pkg/tags"
)

func TestS3SettingsActualSDKConfigurationAuthentication(t *testing.T) {
	calls := 0
	transport := sdkTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		var body []byte
		var e error
		if r.Body != nil {
			body, e = io.ReadAll(io.LimitReader(r.Body, 65537))
		}
		if e != nil {
			t.Fatal(e)
		}
		s, e := Parse(r, policyRegion, time.Now())
		if e != nil || s.Verify(r, body, policySecret, policyRegion) != nil {
			t.Fatal("actual SDK settings request rejected", r.Method, r.URL.RawQuery, e)
		}
		if len(body) > 0 && s.Verify(r, []byte("changed XML"), policySecret, policyRegion) == nil {
			t.Fatal("SDK configuration checksum did not bind XML")
		}
		status := 200
		if r.Method == http.MethodDelete {
			status = 204
		}
		response := ""
		if r.Method == http.MethodGet {
			response = `<LifecycleConfiguration><Rule><ID>bounded</ID><Status>Disabled</Status><Filter><Prefix>forms/</Prefix></Filter><Expiration><Days>30</Days></Expiration></Rule></LifecycleConfiguration>`
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(response)), Request: r}, nil
	})
	c, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(policyID, policySecret, ""), Region: policyRegion, Transport: transport})
	if e != nil {
		t.Fatal(e)
	}
	tag, e := tags.NewTags(map[string]string{"purpose": "settings"}, true)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	for _, e := range []error{c.SetBucketTagging(ctx, "ci-s3-settings", tag), c.PutObjectTagging(ctx, "ci-s3-settings", "object", tag, minio.PutObjectTaggingOptions{}), c.RemoveObjectTagging(ctx, "ci-s3-settings", "object", minio.RemoveObjectTaggingOptions{}), c.SetBucketLifecycle(ctx, "ci-s3-settings", &lifecycle.Configuration{Rules: []lifecycle.Rule{{ID: "bounded", Status: "Disabled", RuleFilter: lifecycle.Filter{Prefix: "forms/"}, Expiration: lifecycle.Expiration{Days: 30}}}})} {
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, e = c.GetBucketLifecycle(ctx, "ci-s3-settings"); e != nil || calls != 5 {
		t.Fatal("SDK lifecycle extension request unavailable", e)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		r, _ := http.NewRequest(method, "http://192.0.2.1:30893/bucket/key", nil)
		r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		r = signer.SignV4(*r, policyID, policySecret, "", policyRegion)
		s, e := Parse(r, policyRegion, time.Now())
		if e != nil || s.Verify(r, nil, policySecret, policyRegion) == nil {
			t.Fatal("unscoped ordinary unsigned mutation accepted")
		}
	}
}
