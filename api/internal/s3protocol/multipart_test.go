package s3protocol

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestSignatureActualSDKMultipartSevenOperations(t *testing.T) {
	id, secret, region := "bcr_0123456789abcdef01234567", strings.Repeat("s", 64), "us-east-2"
	count := 0
	client, e := minio.New("192.0.2.1:30893", &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: region, Transport: copySDKTransport(func(r *http.Request) (*http.Response, error) {
		count++
		var wire []byte
		if r.Body != nil {
			wire, _ = io.ReadAll(r.Body)
		}
		p, err := Parse(r, region, time.Now())
		body, verify := p.VerifyAndDecode(r, wire, secret, region, 8<<20)
		if err != nil || verify != nil {
			t.Errorf("actual multipart signature rejected: method=%s payload=%s wire_bytes=%d md5_signed=%t", r.Method, r.Header.Get("X-Amz-Content-Sha256"), len(wire), strings.Contains(p.Headers, "content-md5"))
		}
		if len(wire) == 0 && p.Verify(r, []byte("injected"), secret, region) == nil {
			t.Error("bodyless control allowed injected bytes")
		}
		changed := r.Clone(r.Context())
		u := *r.URL
		u.RawQuery += "&tampered=true"
		changed.URL = &u
		if p.Verify(changed, body, secret, region) == nil {
			t.Error("changed multipart target accepted")
		}
		q := r.URL.Query()
		response := `<InitiateMultipartUploadResult><Bucket>multipart-test</Bucket><Key>file</Key><UploadId>mpu_0123456789abcdef01234567</UploadId></InitiateMultipartUploadResult>`
		status := 200
		h := http.Header{}
		switch {
		case r.Method == "GET" && q.Has("uploads"):
			response = `<ListMultipartUploadsResult><Bucket>multipart-test</Bucket><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>`
		case r.Method == "PUT" && r.Header.Get("X-Amz-Copy-Source") != "":
			response = `<CopyPartResult><ETag>"` + strings.Repeat("a", 32) + `"</ETag><LastModified>2026-10-16T00:00:00Z</LastModified></CopyPartResult>`
		case r.Method == "PUT":
			response = ""
			h.Set("ETag", "\""+strings.Repeat("a", 32)+"\"")
		case r.Method == "GET":
			response = `<ListPartsResult><Bucket>multipart-test</Bucket><Key>file</Key><UploadId>mpu_0123456789abcdef01234567</UploadId><IsTruncated>false</IsTruncated></ListPartsResult>`
		case r.Method == "POST" && q.Has("uploadId"):
			response = `<CompleteMultipartUploadResult><Bucket>multipart-test</Bucket><Key>file</Key><ETag>"` + strings.Repeat("a", 64) + `"</ETag></CompleteMultipartUploadResult>`
		case r.Method == "DELETE":
			response = ""
			status = 204
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})})
	if e != nil {
		t.Fatal(e)
	}
	core := minio.Core{Client: client}
	ctx := context.Background()
	upload, e := core.NewMultipartUpload(ctx, "multipart-test", "file", minio.PutObjectOptions{UserMetadata: map[string]string{"purpose": "fixture"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = core.ListMultipartUploads(ctx, "multipart-test", "", "", "", "", 1); e != nil {
		t.Fatal(e)
	}
	if _, e = core.PutObjectPart(ctx, "multipart-test", "file", upload, 1, bytes.NewReader([]byte("abc")), 3, minio.PutObjectPartOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e = core.CopyObjectPart(ctx, "multipart-test", "source", "multipart-test", "file", upload, 2, 0, 1, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = core.ListObjectParts(ctx, "multipart-test", "file", upload, 0, 1); e != nil {
		t.Fatal(e)
	}
	if _, e = core.CompleteMultipartUpload(ctx, "multipart-test", "file", upload, []minio.CompletePart{{PartNumber: 1, ETag: strings.Repeat("a", 32)}}, minio.PutObjectOptions{}); e != nil {
		t.Fatal(e)
	}
	if e = core.AbortMultipartUpload(ctx, "multipart-test", "file", upload); e != nil {
		t.Fatal(e)
	}
	if count != 7 {
		t.Fatal("seven actual SDK requests required", count)
	}
}
