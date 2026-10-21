package s3protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/signer"
)

type boundedStreamReader struct{ source io.Reader }

func (r boundedStreamReader) Read(data []byte) (int, error) {
	if len(data) > 64<<10 {
		return 0, errors.New("full object read attempted")
	}
	return r.source.Read(data)
}

func TestSignedStreamAuthenticatesLargeActualSDKUploadWithoutWholeWireBuffer(t *testing.T) {
	const id = "bcr_0123456789abcdef01234567"
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const region = "us-east-2"
	payload := bytes.Repeat([]byte("x"), (17<<20)+9)
	expected := sha256.Sum256(payload)
	calls := 0
	transport := sdkTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		s, err := Parse(r, region, time.Now())
		if err != nil || r.Header.Get("X-Amz-Content-Sha256") != SignedChunkPayload {
			t.Fatal("real SDK signed stream required", err)
		}
		actual := sha256.New()
		if err = s.VerifyAndDecodeTo(r, boundedStreamReader{r.Body}, actual, secret, region, int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(actual.Sum(nil)) != hex.EncodeToString(expected[:]) {
			t.Fatal("SDK stream decoded a different object")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{"\"synthetic\""}}, Body: io.NopCloser(bytes.NewReader(nil)), Request: r}, nil
	})
	client, err := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: region, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.PutObject(context.Background(), "ci-s3-large", "large.bin", bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{DisableMultipart: true, SendContentMd5: true}); err != nil || calls != 1 {
		t.Fatal("real SDK large stream failed", err)
	}
}

func TestSignedStreamHashPresignTamperQuotaAndSinkFailure(t *testing.T) {
	const id = "bcr_0123456789abcdef01234567"
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const region = "us-east-2"
	data := []byte("original bytes")
	for _, presigned := range []bool{false, true} {
		r, _ := http.NewRequest("PUT", "http://192.0.2.1:30893/bucket/key", bytes.NewReader(data))
		if presigned {
			r = signer.PreSignV4(*r, id, secret, "", region, 60)
		} else {
			r.Header.Set("X-Amz-Content-Sha256", Hash(data))
			r = signer.SignV4(*r, id, secret, "", region)
		}
		s, err := Parse(r, region, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err = s.VerifyAndDecodeTo(r, bytes.NewReader(data), &out, secret, region, int64(len(data))); err != nil || !bytes.Equal(out.Bytes(), data) {
			t.Fatal("valid stream rejected", err)
		}
		if err = s.VerifyAndDecodeTo(r, bytes.NewReader(data), io.Discard, secret, region, int64(len(data)-1)); err == nil {
			t.Fatal("stream quota bypassed")
		}
		if !presigned && s.VerifyAndDecodeTo(r, bytes.NewReader([]byte("changed bytes!")), io.Discard, secret, region, int64(len(data))) == nil {
			t.Fatal("changed signed bytes accepted")
		}
		expected := errors.New("private buffer failed")
		if err = s.VerifyAndDecodeTo(r, bytes.NewReader(data), failingStreamWriter{expected}, secret, region, int64(len(data))); !errors.Is(err, expected) {
			t.Fatal("buffer write failure ignored", err)
		}
		r.URL.RawQuery += "&untrusted=changed"
		if s.VerifyAndDecodeTo(r, bytes.NewReader(data), io.Discard, secret, region, int64(len(data))) == nil {
			t.Fatal("changed original request accepted")
		}
	}
}

type failingStreamWriter struct{ err error }

func (w failingStreamWriter) Write([]byte) (int, error) { return 0, w.err }

func TestSignedStreamRejectsChangedChunkChainLengthAndTerminal(t *testing.T) {
	const id = "bcr_0123456789abcdef01234567"
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const region = "us-east-2"
	data := bytes.Repeat([]byte("x"), 131079)
	transport := sdkTransport(func(r *http.Request) (*http.Response, error) {
		wire, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Parse(r, region, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		badByte := append([]byte{}, wire...)
		badByte[bytes.Index(badByte, []byte("\r\n"))+2] ^= 1
		for _, bad := range [][]byte{badByte, wire[:len(wire)-86], wire[:len(wire)-1], append(append([]byte{}, wire...), 'x'), append([]byte("z"), wire[1:]...)} {
			if e := s.VerifyAndDecodeTo(r, bytes.NewReader(bad), io.Discard, secret, region, int64(len(data))); e == nil {
				t.Fatal("invalid chained stream or terminal accepted")
			}
		}
		changed := r.Clone(r.Context())
		changed.Header.Set("X-Amz-Decoded-Content-Length", "0")
		if s.VerifyAndDecodeTo(changed, bytes.NewReader(wire), io.Discard, secret, region, int64(len(data))) == nil {
			t.Fatal("changed signed length accepted")
		}
		if s.VerifyAndDecodeTo(r, bytes.NewReader(wire), io.Discard, secret, region, int64(len(data)-1)) == nil {
			t.Fatal("decoded size bound bypassed")
		}
		if s.VerifyAndDecodeTo(r, bytes.NewReader(wire), io.Discard, secret, region, int64(len(data))) != nil {
			t.Fatal("original chain rejected")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{"\"synthetic\""}}, Body: io.NopCloser(bytes.NewReader(nil)), Request: r}, nil
	})
	client, err := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: region, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.PutObject(context.Background(), "ci-stream-negative", "key", bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{DisableMultipart: true}); err != nil {
		t.Fatal(err)
	}
}
