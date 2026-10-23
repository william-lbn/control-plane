package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/signer"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func probeS3Capabilities(ctx context.Context, client *minio.Client, c config, transport *onceTransport, stage, check func(string)) error {
	if !strings.HasPrefix(c.Output, "/run/e2e-private/") || !strings.HasPrefix(c.Bucket, "ci-s3-capabilities-") {
		return errors.New("protected capability fixture required")
	}
	state := struct {
		Bucket  string   `json:"bucket"`
		Checks  []string `json:"checks"`
		Running string   `json:"running"`
	}{Bucket: c.Bucket, Checks: []string{}}
	if raw, e := os.ReadFile(c.Output); e == nil {
		if json.Unmarshal(raw, &state) != nil || state.Bucket != c.Bucket {
			return errors.New("original capability checkpoint differs")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	for _, v := range state.Checks {
		check(v)
	}
	save := func() error {
		raw, e := json.Marshal(state)
		if e != nil {
			return e
		}
		return os.WriteFile(c.Output, raw, 0o600)
	}
	step := func(name string, run func() error) error {
		for _, v := range state.Checks {
			if v == name {
				return nil
			}
		}
		stage(name)
		if state.Running != "" {
			return errors.New("inspect original capability mutation before resuming")
		}
		state.Running = name
		if e := save(); e != nil {
			return e
		}
		if e := run(); e != nil {
			return e
		}
		state.Running = ""
		state.Checks = append(state.Checks, name)
		if e := save(); e != nil {
			return e
		}
		check(name)
		return nil
	}

	raw := func(method, bucket, key, query string, body []byte, headers http.Header) (int, []byte, error) {
		target, e := url.Parse(c.Endpoint)
		if e != nil {
			return 0, nil, e
		}
		target.Path = "/" + bucket
		if key != "" {
			target.Path += "/" + key
		}
		target.RawQuery = query
		request, e := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
		if e != nil {
			return 0, nil, e
		}
		request.Header = headers.Clone()
		if request.Header == nil {
			request.Header = make(http.Header)
		}
		request.Header.Set("X-Amz-Content-Sha256", fmt.Sprintf("%x", sha256.Sum256(body)))
		request = signer.SignV4(*request, c.Access, c.Secret, "", c.Region)
		response, e := (&http.Client{Transport: transport}).Do(request)
		if e != nil {
			return 0, nil, e
		}
		defer response.Body.Close()
		wire, e := io.ReadAll(io.LimitReader(response.Body, 65537))
		return response.StatusCode, wire, e
	}
	content := []byte("Owned private S3 capability bytes, never a public grant.\n")
	if e := step("SDK_explicit_private_STANDARD_PUT_and_object_ACL", func() error {
		status, _, e := raw("PUT", c.Bucket, "capabilities/source.txt", "", content, http.Header{"X-Amz-Acl": {"private"}, "X-Amz-Storage-Class": {"STANDARD"}})
		if e != nil {
			return e
		}
		if status != 200 {
			return errors.New("private STANDARD object rejected")
		}
		status, wire, e := raw("GET", c.Bucket, "capabilities/source.txt", "acl", nil, nil)
		if e != nil {
			return e
		}
		if status != 200 || !bytes.Contains(wire, []byte(c.Branch)) {
			return errors.New("object ACL representation differs")
		}
		return nil
	}); e != nil {
		return e
	}
	if e := step("SDK_unsupported_grants_and_encryption_do_not_publish_objects", func() error {
		cases := []struct{ name, value string }{
			{"X-Amz-Grant-Read", `uri="http://acs.amazonaws.com/groups/global/AllUsers"`},
			{"X-Amz-Grant-Full-Control", `id="foreign"`}, {"X-Amz-Acl", "public-read"},
			{"X-Amz-Server-Side-Encryption", "AES256"}, {"X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256"},
			{"X-Amz-Copy-Source-Server-Side-Encryption-Customer-Algorithm", "AES256"}, {"X-Amz-Object-Lock-Mode", "GOVERNANCE"},
			{"X-Amz-Website-Redirect-Location", "https://invalid.example"}, {"X-Amz-Storage-Class", "GLACIER"},
		}
		for i, v := range cases {
			for _, operation := range []string{"PUT", "COPY", "MULTIPART"} {
				method, query, body := "PUT", "", content
				headers := make(http.Header)
				headers.Set(v.name, v.value)
				if operation == "COPY" {
					headers.Set("X-Amz-Copy-Source", "/"+c.Bucket+"/capabilities/source.txt")
					body = nil
				}
				if operation == "MULTIPART" {
					method = "POST"
					query = "uploads"
					body = nil
				}
				status, _, e := raw(method, c.Bucket, fmt.Sprintf("rejected/%d-%s", i, operation), query, body, headers)
				if e != nil {
					return e
				}
				if status != 501 {
					return fmt.Errorf("unsupported header %d operation %s returned %d", i, operation, status)
				}
			}
		}
		status, _, e := raw("PUT", c.Bucket+"-denied", "", "", nil, http.Header{"X-Amz-Acl": {"public-read"}})
		if e != nil {
			return e
		}
		if status != 501 {
			return errors.New("unsupported bucket grant acknowledged")
		}
		status, _, e = raw("HEAD", c.Bucket+"-denied", "", "", nil, nil)
		if e != nil {
			return e
		}
		if status != 404 {
			return errors.New("rejected bucket exists")
		}
		status, _, e = raw("GET", c.Bucket, "capabilities/source.txt", "", nil, http.Header{"X-Amz-Server-Side-Encryption-Customer-Algorithm": {"AES256"}})
		if e != nil {
			return e
		}
		if status != 501 {
			return errors.New("unsupported decryption request returned ordinary bytes")
		}
		return nil
	}); e != nil {
		return e
	}
	if e := step("SDK_explicit_private_STANDARD_multipart_publishes_once", func() error {
		key := "capabilities/private-multipart.txt"
		status, wire, e := raw("POST", c.Bucket, key, "uploads", nil, http.Header{"X-Amz-Acl": {"private"}, "X-Amz-Storage-Class": {"STANDARD"}})
		if e != nil {
			return e
		}
		if status != 200 {
			return errors.New("private STANDARD multipart rejected")
		}
		var init struct {
			UploadID string `xml:"UploadId"`
		}
		if xml.Unmarshal(wire, &init) != nil || init.UploadID == "" {
			return errors.New("multipart identity missing")
		}
		core := minio.Core{Client: client}
		part, e := core.PutObjectPart(ctx, c.Bucket, key, init.UploadID, 1, bytes.NewReader(content), int64(len(content)), minio.PutObjectPartOptions{})
		if e != nil {
			return e
		}
		_, e = core.CompleteMultipartUpload(ctx, c.Bucket, key, init.UploadID, []minio.CompletePart{{PartNumber: 1, ETag: part.ETag}}, minio.PutObjectOptions{})
		return e
	}); e != nil {
		return e
	}
	if e := step("SDK_unsupported_matrix_operations_return_explicit_501", func() error {
		for _, q := range []string{"notification", "logging", "encryption", "replication", "website", "accelerate", "requestPayment", "analytics", "metrics", "inventory", "ownershipControls", "intelligent-tiering", "versions"} {
			status, _, e := raw("GET", c.Bucket, "", q, nil, nil)
			if e != nil {
				return e
			}
			if status != 501 {
				return fmt.Errorf("unsupported bucket subresource %s returned %d", q, status)
			}
		}
		for _, q := range []string{"select", "restore", "torrent", "legal-hold", "retention"} {
			status, _, e := raw("GET", c.Bucket, "capabilities/source.txt", q, nil, nil)
			if e != nil {
				return e
			}
			if status != 501 {
				return fmt.Errorf("unsupported object subresource %s returned %d", q, status)
			}
		}
		return nil
	}); e != nil {
		return e
	}
	return step("SDK_rejected_mutations_preserve_exact_owned_object_inventory", func() error {
		count := 0
		for v := range client.ListObjects(ctx, c.Bucket, minio.ListObjectsOptions{Recursive: true}) {
			if v.Err != nil {
				return v.Err
			}
			if v.Key != "capabilities/source.txt" && v.Key != "capabilities/private-multipart.txt" {
				return errors.New("rejected object was published")
			}
			count++
		}
		if count != 2 {
			return errors.New("owned object inventory differs")
		}
		obj, e := client.GetObject(ctx, c.Bucket, "capabilities/source.txt", minio.GetObjectOptions{})
		if e != nil {
			return e
		}
		defer obj.Close()
		raw, e := io.ReadAll(io.LimitReader(obj, int64(len(content)+1)))
		if e != nil {
			return e
		}
		if !bytes.Equal(raw, content) {
			return errors.New("rejected mutation changed original bytes")
		}
		return nil
	})
}
