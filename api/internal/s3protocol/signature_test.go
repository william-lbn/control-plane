package s3protocol

import (
	"bytes"
	"github.com/minio/minio-go/v7/pkg/signer"
	"net/http"
	"testing"
	"time"
)

func TestSignatureWithActualS3SDKSigner(t *testing.T) {
	id := "bcr_0123456789abcdef01234567"
	secret := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	region := "us-east-2"
	for _, method := range []string{"GET", "HEAD", "PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			body := []byte{}
			if method == "PUT" {
				body = []byte("real SDK bytes / 中文")
			}
			r, e := http.NewRequest(method, "http://192.0.2.1:30893/s3/br_0123456789abcdef/bucket/中文%20file.txt?tag=z&prefix=space%20value", bytes.NewReader(body))
			if e != nil {
				t.Fatal(e)
			}
			r.Header.Set("X-Amz-Content-Sha256", Hash(body))
			r = signer.SignV4(*r, id, secret, "", region)
			parsed, e := Parse(r, region, time.Now())
			if e != nil {
				t.Fatal("actual SDK signature parse rejected")
			}
			if parsed.Verify(r, body, secret, region) != nil {
				t.Fatal("actual SDK canonical/signature mismatch")
			}
			if method == "PUT" && parsed.Verify(r, []byte("tampered"), secret, region) == nil {
				t.Fatal("tampered payload accepted")
			}
			r.URL.RawQuery += "&other=changed"
			if parsed.Verify(r, body, secret, region) == nil {
				t.Fatal("changed query accepted")
			}
		})
	}
	r, _ := http.NewRequest("GET", "http://192.0.2.1:30893/bucket/中文%20file", nil)
	r = signer.PreSignV4(*r, id, secret, "", region, 60)
	parsed, e := Parse(r, region, time.Now())
	if e != nil || !parsed.Presigned || parsed.Verify(r, nil, secret, region) != nil {
		t.Fatal("actual SDK presigned GET rejected")
	}
	if _, e = Parse(r, region, time.Now().Add(61*time.Second)); e == nil {
		t.Fatal("expired presign accepted")
	}
	r.Host = "192.0.2.2:30893"
	if parsed.Verify(r, nil, secret, region) == nil {
		t.Fatal("changed authority accepted")
	}
}
func TestSignatureRejectsAmbiguousAndUnsupportedCredentials(t *testing.T) {
	for _, header := range []string{"Bearer console-token", "AWS4-HMAC-SHA256 Credential=other/20261014/us-east-2/s3/aws4_request, SignedHeaders=host, Signature=x", "AWS4-ECDSA-P256-SHA256 bad"} {
		r, _ := http.NewRequest("GET", "http://s3.invalid/bucket/key", nil)
		r.Header.Set("Authorization", header)
		if _, e := Parse(r, "us-east-2", time.Now()); e == nil {
			t.Fatal("unsupported authentication accepted")
		}
	}
	if Encode("中文 space/+", true) != "%E4%B8%AD%E6%96%87%20space/%2B" {
		t.Fatal("canonical URI encoding differs from S3 byte rules")
	}
}
