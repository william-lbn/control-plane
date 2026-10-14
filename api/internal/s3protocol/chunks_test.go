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
)

type sdkTransport func(*http.Request) (*http.Response, error)

func (f sdkTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise actual Client.PutObject, not only the ordinary SignV4 helper: the
// SDK uses chained streaming signatures even for small known-size HTTP PUTs.
func TestSignedChunksWithActualSDKPutObject(t *testing.T) {
	const id = "bcr_0123456789abcdef01234567"
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const region = "us-east-2"
	for _, size := range []int{1, 65536, 131079} {
		t.Run(string(rune('a'+size%7)), func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), size)
			calls := 0
			transport := sdkTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "PUT" || r.Header.Get("X-Amz-Content-Sha256") != SignedChunkPayload {
					t.Fatal("SDK upload did not exercise signed chunks")
				}
				wire, e := io.ReadAll(r.Body)
				if e != nil {
					t.Fatal("SDK stream unavailable")
				}
				s, e := Parse(r, region, time.Now())
				if e != nil {
					t.Fatal("SDK seed rejected")
				}
				decoded, e := s.VerifyAndDecode(r, wire, secret, region, size)
				if e != nil || !bytes.Equal(decoded, payload) {
					t.Fatal("SDK chunk bytes/signature mismatch")
				}
				for name, bad := range map[string][]byte{
					"missing terminal": wire[:len(wire)-86],
					"extra bytes":      append(append([]byte{}, wire...), 'x'),
					"truncated":        wire[:len(wire)-1],
					"malformed length": append([]byte("z"), wire[1:]...),
				} {
					if _, e = s.VerifyAndDecode(r, bad, secret, region, size); e == nil {
						t.Fatal(name + " accepted")
					}
				}
				bad := append([]byte{}, wire...)
				start := bytes.Index(bad, []byte("\r\n")) + 2
				bad[start] ^= 1
				if _, e = s.VerifyAndDecode(r, bad, secret, region, size); e == nil {
					t.Fatal("tampered chunk accepted")
				}
				if _, e = s.VerifyAndDecode(r, wire, secret, region, size-1); e == nil {
					t.Fatal("decoded quota bypass accepted")
				}
				changed := r.Clone(r.Context())
				changed.Header.Set("X-Amz-Decoded-Content-Length", "0")
				if _, e = s.VerifyAndDecode(changed, wire, secret, region, size); e == nil {
					t.Fatal("changed signed length accepted")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{"\"synthetic\""}}, Body: io.NopCloser(bytes.NewReader(nil)), Request: r}, nil
			})
			client, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: region, Transport: transport})
			if e != nil {
				t.Fatal("SDK setup failed")
			}
			_, e = client.PutObject(context.Background(), "ci-s3-bucket", "中文 key", bytes.NewReader(payload), int64(size), minio.PutObjectOptions{DisableMultipart: true, SendContentMd5: true})
			if e != nil || calls != 1 {
				t.Fatal("single SDK upload failed")
			}
		})
	}
}
