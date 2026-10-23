package control

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"
	"github.com/william-lbn/control-plane/api/internal/s3protocol"
)

type s3SettingsProtocolTransport func(*http.Request) (*http.Response, error)

func (f s3SettingsProtocolTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestS3SettingsActualSDKDispatchDoesNotTreatTagXMLAsObject(t *testing.T) {
	const id = "bcr_0123456789abcdef01234567"
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const region = "us-east-2"
	calls := 0
	transport := s3SettingsProtocolTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if s3StreamingObjectPUT(r) {
			t.Fatal("SDK tagging XML routed into raw object upload")
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, 65537))
		if e != nil {
			t.Fatal(e)
		}
		signature, e := s3protocol.Parse(r, region, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		decoded, e := signature.VerifyAndDecode(r, body, secret, region, 65536)
		if e != nil || !bytes.Contains(decoded, []byte("<Tagging")) {
			t.Fatal("SDK settings authentication or XML unavailable", e)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil)), Request: r}, nil
	})
	c, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: region, Transport: transport})
	if e != nil {
		t.Fatal(e)
	}
	v, e := tags.NewTags(map[string]string{"purpose": "dispatch"}, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.PutObjectTagging(context.Background(), "ci-s3-settings", "settings/source.txt", v, minio.PutObjectTaggingOptions{VersionID: "null"}); e != nil || calls != 1 {
		t.Fatal("actual SDK settings request failed", e)
	}
	ordinary, _ := http.NewRequest("PUT", "http://s3.test/bucket/key?x-id=PutObject", nil)
	if !s3StreamingObjectPUT(ordinary) {
		t.Fatal("ordinary SDK object upload lost streaming admission")
	}
}
