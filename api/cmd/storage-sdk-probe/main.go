// Linux acceptance CLI; credentials are read from a protected file, never
// argv/env/stdout. Results contain checkpoint names, never signed URLs or bytes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

type config struct {
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Access   string `json:"access_key"`
	Secret   string `json:"secret_key"`
	Bucket   string `json:"bucket"`
	Mode     string `json:"mode"`
}
type onceTransport struct {
	http.RoundTripper
	mu   sync.Mutex
	sent map[string]bool
}

func (t *onceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == "PUT" || r.Method == "POST" || r.Method == "DELETE" {
		key := r.Method + " " + r.URL.RequestURI()
		t.mu.Lock()
		used := t.sent[key]
		t.sent[key] = true
		t.mu.Unlock()
		if used {
			return nil, errors.New("mutation retry blocked before dispatch")
		}
	}
	return t.RoundTripper.RoundTrip(r)
}
func main() {
	if runtime.GOOS != "linux" || len(os.Args) != 2 {
		os.Exit(2)
	}
	info, e := os.Stat(os.Args[1])
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		os.Exit(2)
	}
	raw, e := os.ReadFile(os.Args[1])
	if e != nil {
		os.Exit(2)
	}
	var c config
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		os.Exit(2)
	}
	endpoint, e := url.Parse(c.Endpoint)
	if e != nil || endpoint.Scheme != "http" || (endpoint.Hostname() != "192.168.146.100" && endpoint.Hostname() != "192.168.146.101" && endpoint.Hostname() != "192.168.146.102") || endpoint.Port() != "30893" || endpoint.Path != "" || endpoint.User != nil || endpoint.RawQuery != "" || !strings.HasPrefix(c.Bucket, "ci-s3-") {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	inner := &http.Transport{Proxy: nil, MaxIdleConns: 2, ResponseHeaderTimeout: 120 * time.Second}
	defer inner.CloseIdleConnections()
	transport := &onceTransport{RoundTripper: inner, sent: map[string]bool{}}
	client, e := minio.New(endpoint.Host, &minio.Options{Creds: credentials.NewStaticV4(c.Access, c.Secret, ""), Secure: false, Region: c.Region, Transport: transport})
	if e != nil {
		os.Exit(2)
	}
	checks := []string{}
	stage := "initialize"
	result := "fail"
	run := func() error {
		key := "folder/中文 file.bin"
		payload := []byte("Neon real S3 SDK bytes 中文\n")
		switch c.Mode {
		case "write":
			stage = "make_bucket"
			if e = client.MakeBucket(ctx, c.Bucket, minio.MakeBucketOptions{Region: c.Region}); e != nil {
				return e
			}
			checks = append(checks, "real_sdk_create_private_bucket")
			stage = "put_object"
			_, e = client.PutObject(ctx, c.Bucket, key, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: true, SendContentMd5: true})
			if e != nil {
				return e
			}
			checks = append(checks, "real_sdk_put_unicode_key")
			stage = "head_object"
			stat, e := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
			if e != nil || stat.Size != int64(len(payload)) {
				return errors.New("stat rejected")
			}
			checks = append(checks, "real_sdk_head_object")
			stage = "list_objects"
			count := 0
			for item := range client.ListObjects(ctx, c.Bucket, minio.ListObjectsOptions{Recursive: true, Prefix: "folder/"}) {
				if item.Err != nil {
					return item.Err
				}
				if item.Key == key {
					count++
				}
			}
			if count != 1 {
				return errors.New("list rejected")
			}
			checks = append(checks, "real_sdk_list_v2_utf8")
			stage = "presigned_get"
			presign, e := client.PresignedGetObject(ctx, c.Bucket, key, time.Minute, nil)
			if e != nil {
				return e
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", presign.String(), nil)
			response, e := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
			if e != nil {
				return e
			}
			b, e := io.ReadAll(io.LimitReader(response.Body, 1024))
			response.Body.Close()
			if e != nil || response.StatusCode != 200 || !bytes.Equal(b, payload) {
				return errors.New("presign rejected")
			}
			checks = append(checks, "real_sdk_presigned_download")
		case "read":
			stage = "get_object"
			object, e := client.GetObject(ctx, c.Bucket, key, minio.GetObjectOptions{})
			if e != nil {
				return e
			}
			b, e := io.ReadAll(io.LimitReader(object, 1024))
			object.Close()
			if e != nil || !bytes.Equal(b, payload) {
				return errors.New("read rejected")
			}
			checks = append(checks, "real_sdk_get_exact_bytes")
			stage = "range_get"
			options := minio.GetObjectOptions{}
			options.SetRange(1, 4)
			object, e = client.GetObject(ctx, c.Bucket, key, options)
			if e != nil {
				return e
			}
			b, e = io.ReadAll(io.LimitReader(object, 1024))
			object.Close()
			if e != nil || !bytes.Equal(b, payload[1:5]) {
				return errors.New("range rejected")
			}
			checks = append(checks, "real_sdk_range_get")
		case "deny_write":
			stage = "scope_denied_put"
			_, e = client.PutObject(ctx, c.Bucket, "forbidden", bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{DisableMultipart: true, SendContentMd5: true})
			if e == nil || minio.ToErrorResponse(e).StatusCode != 403 {
				return errors.New("write scope was not rejected")
			}
			checks = append(checks, "real_sdk_read_credential_write_denied")
		case "deny_read":
			stage = "revoked_or_rotated_key"
			_, e = client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
			if e == nil || minio.ToErrorResponse(e).StatusCode != 403 {
				return errors.New("old key was not rejected")
			}
			checks = append(checks, "real_sdk_old_or_revoked_key_denied")
		case "cleanup":
			// Retain the primary data/manifest. Only auxiliary CRUD probes retire.
			stage = "delete_probe_put"
			_, e = client.PutObject(ctx, c.Bucket, "delete-probe", bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{DisableMultipart: true, SendContentMd5: true})
			if e != nil {
				return e
			}
			stage = "delete_object"
			if e = client.RemoveObject(ctx, c.Bucket, "delete-probe", minio.RemoveObjectOptions{}); e != nil {
				return e
			}
			checks = append(checks, "real_sdk_delete_auxiliary_manifest_primary_object_retained")
			stage = "empty_bucket_create"
			if e = client.MakeBucket(ctx, c.Bucket+"-empty", minio.MakeBucketOptions{Region: c.Region}); e != nil {
				return e
			}
			stage = "delete_bucket"
			if e = client.RemoveBucket(ctx, c.Bucket+"-empty"); e != nil {
				return e
			}
			checks = append(checks, "real_sdk_delete_empty_bucket_physical_blobs_retained")
		default:
			return errors.New("unknown mode")
		}
		return nil
	}
	if e = run(); e == nil {
		result = "pass"
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"result": result, "stage": stage, "checks": checks, "credentials_in_output": false})
	if result != "pass" {
		os.Exit(1)
	}
}
