package s3protocol

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/signer"
	"io"
	"net/http"
	"strings"
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

// Exercise the actual CopyObject client request, not just a manually populated
// signer request. COPY can differ from ordinary upload payload headers.
type copySDKTransport func(*http.Request) (*http.Response, error)

func (f copySDKTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSignatureActualSDKCopyRequest(t *testing.T) {
	id := "bcr_0123456789abcdef01234567"
	secret := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	client, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: "us-east-2", Transport: copySDKTransport(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		parsed, err := Parse(r, "us-east-2", time.Now())
		mode := r.Header.Get("X-Amz-Content-Sha256")
		if err != nil || parsed.Verify(r, body, secret, "us-east-2") != nil {
			t.Errorf("Actual COPY rejected: payload_mode=%q body_bytes=%d copy_source=%t signed_copy_source=%t", mode, len(body), r.Header.Get("X-Amz-Copy-Source") != "", strings.Contains(parsed.Headers, "x-amz-copy-source"))
		}
		if parsed.Verify(r, []byte("unsigned upload bytes"), secret, "us-east-2") == nil {
			t.Error("COPY marker allowed an unsigned data upload")
		}
		changed := r.Clone(r.Context())
		changed.Header = r.Header.Clone()
		changed.Header.Set("X-Amz-Copy-Source", "copy-source/different.txt")
		if parsed.Verify(changed, body, secret, "us-east-2") == nil {
			t.Error("Signed copy source could be tampered")
		}
		changed.Header.Del("X-Amz-Copy-Source")
		if parsed.Verify(changed, body, secret, "us-east-2") == nil {
			t.Error("Ordinary unsigned PUT accepted")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`<CopyObjectResult><ETag>"abc"</ETag><LastModified>2026-10-14T16:00:00Z</LastModified></CopyObjectResult>`)), Request: r}, nil
	})})
	if e != nil {
		t.Fatal(e)
	}
	_, e = client.CopyObject(context.Background(), minio.CopyDestOptions{Bucket: "copy-target", Object: "copied.txt"}, minio.CopySrcOptions{Bucket: "copy-source", Object: "directory/root.txt", MatchETag: strings.Repeat("a", 64)})
	if e != nil {
		t.Fatal("Copy protocol test response invalid")
	}
}

func TestSignatureActualSDKDeleteObjectsRequest(t *testing.T) {
	id := "bcr_0123456789abcdef01234567"
	secret := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	requests := 0
	client, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: "us-east-2", Transport: copySDKTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		body, _ := io.ReadAll(r.Body)
		parsed, err := Parse(r, "us-east-2", time.Now())
		if err != nil || parsed.Verify(r, body, secret, "us-east-2") != nil {
			t.Error("Actual signed batch delete rejected")
		}
		sum := md5.Sum(body)
		if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
			t.Errorf("SDK batch checksum is not MD5: md5_present=%t checksum_sha256_present=%t checksum_crc32c_present=%t", r.Header.Get("Content-MD5") != "", r.Header.Get("X-Amz-Checksum-Sha256") != "", r.Header.Get("X-Amz-Checksum-Crc32c") != "")
		}
		if r.Method != "POST" || !r.URL.Query().Has("delete") || len(body) == 0 {
			t.Error("Batch request shape differs")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`)), Request: r}, nil
	})})
	if e != nil {
		t.Fatal(e)
	}
	objects := make(chan minio.ObjectInfo, 2)
	objects <- minio.ObjectInfo{Key: "aux/a.txt"}
	objects <- minio.ObjectInfo{Key: "aux/中文.txt"}
	close(objects)
	for failure := range client.RemoveObjects(context.Background(), "batch-test", objects, minio.RemoveObjectsOptions{}) {
		if failure.Err != nil {
			t.Fatal("Batch protocol test rejected response")
		}
	}
	if requests != 1 {
		t.Fatal("Expected one SDK batch request")
	}
}

func TestSignatureActualSDKPresignedPutAndBoundary(t *testing.T) {
	id := "bcr_0123456789abcdef01234567"
	secret := strings.Repeat("s", 64)
	region := "us-east-2"
	client, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: region})
	if e != nil {
		t.Fatal(e)
	}
	uri, e := client.PresignedPutObject(context.Background(), "upload-bucket", "folder/中文.txt", time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	r, _ := http.NewRequest("PUT", uri.String(), nil)
	p, e := Parse(r, region, time.Now())
	if e != nil || !p.Presigned || p.Verify(r, []byte("SDK authorized bytes"), secret, region) != nil {
		t.Fatal("actual SDK presigned PUT rejected")
	}
	for _, method := range []string{"POST", "DELETE", "GET"} {
		changed := r.Clone(r.Context())
		changed.Method = method
		if p.Verify(changed, []byte("SDK authorized bytes"), secret, region) == nil {
			t.Error("method change accepted")
		}
	}
	changed := r.Clone(r.Context())
	u := *r.URL
	u.Path += "other"
	changed.URL = &u
	if p.Verify(changed, nil, secret, region) == nil {
		t.Error("key change accepted")
	}
	changed = r.Clone(r.Context())
	changed.Header.Set("X-Amz-Copy-Source", "/secret/file")
	if p.Verify(changed, []byte("body"), secret, region) == nil {
		t.Error("unsigned COPY source accepted")
	}
	if _, e = Parse(r, region, time.Now().Add(2*time.Minute)); e == nil {
		t.Error("expired URL accepted")
	}
	normal, _ := http.NewRequest("PUT", "http://192.0.2.1:30893/upload-bucket/plain.txt", nil)
	normal.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	normal = signer.SignV4(*normal, id, secret, "", region)
	n, e := Parse(normal, region, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if n.Verify(normal, []byte("write"), secret, region) == nil {
		t.Error("header unsigned upload broadened")
	}
}

func TestSignatureActualSDKBucketCORSBoundedMD5(t *testing.T) {
	id := "bcr_0123456789abcdef01234567"
	secret := strings.Repeat("s", 64)
	count := 0
	client, e := minio.New("192.0.2.1:30893", &minio.Options{Region: "us-east-2", Creds: credentials.NewStaticV4(id, secret, ""), Transport: copySDKTransport(func(r *http.Request) (*http.Response, error) {
		count++
		body, _ := io.ReadAll(r.Body)
		p, e := Parse(r, "us-east-2", time.Now())
		if e != nil || p.Verify(r, body, secret, "us-east-2") != nil {
			t.Errorf("actual SDK CORS signature denied: payload_kind=%s signed_headers=%s", r.Header.Get("X-Amz-Content-Sha256"), p.Headers)
		}
		if p.Verify(r, append(body, 'x'), secret, "us-east-2") == nil {
			t.Error("changed XML allowed")
		}
		changed := r.Clone(r.Context())
		changed.Header.Set("Content-MD5", "wrong")
		if p.Verify(changed, body, secret, "us-east-2") == nil {
			t.Error("changed checksum allowed")
		}
		changed = r.Clone(r.Context())
		u := *r.URL
		u.RawQuery = ""
		changed.URL = &u
		if p.Verify(changed, body, secret, "us-east-2") == nil {
			t.Error("ordinary unsigned object PUT allowed")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})})
	if e != nil {
		t.Fatal(e)
	}
	policy, e := cors.ParseBucketCorsConfig(strings.NewReader(`<CORSConfiguration><CORSRule><AllowedOrigin>http://127.0.0.1:19001</AllowedOrigin><AllowedMethod>PUT</AllowedMethod><AllowedHeader>*</AllowedHeader></CORSRule></CORSConfiguration>`))
	if e != nil {
		t.Fatal(e)
	}
	if e = client.SetBucketCors(context.Background(), "cors-bucket", policy); e != nil {
		t.Fatal("actual SDK CORS request failed")
	}
	if count != 1 {
		t.Fatal("CORS protocol request unexpectedly repeated")
	}
}
