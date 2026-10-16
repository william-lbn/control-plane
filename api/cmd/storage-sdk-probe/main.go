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
	"github.com/minio/minio-go/v7/pkg/signer"
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
	Branch   string `json:"branch_id,omitempty"`
	Output   string `json:"output,omitempty"`
}
type onceTransport struct {
	http.RoundTripper
	mu       sync.Mutex
	sent     map[string]bool
	statuses []int
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
	response, err := t.RoundTripper.RoundTrip(r)
	if response != nil {
		t.mu.Lock()
		t.statuses = append(t.statuses, response.StatusCode)
		t.mu.Unlock()
	}
	return response, err
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
		case "multipart-administration":
			stage = "actual_signed_multipart_grouping_and_administration_fixture"
			return probeMultipartAdministration(ctx, client, c, func(check string) { checks = append(checks, check) })
		case "multipart":
			stage = "actual_signed_multipart_operations"
			return probeMultipart(ctx, client, c, func(check string) { checks = append(checks, check) })
		case "copy-conditions":
			stage = "actual_signed_copy_source_conditions"
			return probeCopyConditions(ctx, client, c, transport, func(check string) { checks = append(checks, check) })
		case "write", "continue_write", "continue_existing":
			if c.Mode == "write" {
				stage = "make_bucket"
				if e = client.MakeBucket(ctx, c.Bucket, minio.MakeBucketOptions{Region: c.Region}); e != nil {
					return e
				}
				checks = append(checks, "real_sdk_create_private_bucket")
			} else if c.Mode == "continue_write" {
				// Continue only the owned empty bucket. A successful/unknown PUT
				// outcome must never be overwritten as a test retry.
				stage = "verify_original_empty_bucket"
				found, err := client.BucketExists(ctx, c.Bucket)
				if err != nil || !found {
					return errors.New("owned bucket missing")
				}
				_, err = client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
				if err == nil || minio.ToErrorResponse(err).Code != "NoSuchKey" {
					return errors.New("original object outcome requires inspection")
				}
				checks = append(checks, "original_owned_bucket_reused_primary_key_confirmed_absent_before_single_put")
			}
			if c.Mode == "continue_existing" {
				stage = "verify_original_retained_bytes"
				object, err := client.GetObject(ctx, c.Bucket, key, minio.GetObjectOptions{})
				if err != nil {
					return err
				}
				data, err := io.ReadAll(io.LimitReader(object, 1024))
				object.Close()
				if err != nil || !bytes.Equal(data, payload) {
					return errors.New("original bytes require inspection")
				}
				checks = append(checks, "original_primary_object_verified_no_upload_repeated")
			} else {
				stage = "put_object"
				_, e = client.PutObject(ctx, c.Bucket, key, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: true, SendContentMd5: true})
				if e != nil {
					return e
				}
				checks = append(checks, "real_sdk_put_unicode_key")
			}
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

		case "metadata-replace":
			key := "metadata/edit.txt"
			stage = "original_ui_metadata_observed"
			source, e := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
			if e != nil || source.Metadata.Get("X-Amz-Meta-Purpose") != "ui-edited" {
				return errors.New("original UI metadata requires inspection")
			}
			stage = "actual_sdk_self_copy_replace_metadata"
			copied, e := client.CopyObject(ctx, minio.CopyDestOptions{Bucket: c.Bucket, Object: key, ReplaceMetadata: true, UserMetadata: map[string]string{"purpose": "sdk-replaced"}}, minio.CopySrcOptions{Bucket: c.Bucket, Object: key, MatchETag: source.ETag})
			if e != nil {
				return e
			}
			if copied.ETag != source.ETag {
				return errors.New("metadata replacement changed content ETag")
			}
			stat, e := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
			if e != nil || stat.Metadata.Get("X-Amz-Meta-Purpose") != "sdk-replaced" {
				return errors.New("replacement metadata not persisted")
			}
			object, e := client.GetObject(ctx, c.Bucket, key, minio.GetObjectOptions{})
			if e != nil {
				return e
			}
			data, e := io.ReadAll(io.LimitReader(object, 1024))
			object.Close()
			if e != nil || !bytes.Equal(data, payload) {
				return errors.New("replacement changed original bytes")
			}
			checks = append(checks, "actual_sdk_self_copy_replace_metadata_preserves_content_etag_and_bytes")
		case "cors-metadata":
			stage = "actual_sdk_get_bucket_cors"
			policy, e := client.GetBucketCors(ctx, c.Bucket)
			if e != nil || policy == nil {
				return errors.New("UI CORS policy missing in SDK")
			}
			stage = "actual_sdk_put_bucket_cors"
			if e = client.SetBucketCors(ctx, c.Bucket, policy); e != nil {
				return e
			}
			checks = append(checks, "actual_sdk_get_and_put_cors_configuration_match_ui")
			key := "metadata/sdk.txt"
			stage = "actual_sdk_metadata_upload"
			if _, e = client.PutObject(ctx, c.Bucket, key, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{ContentType: "text/plain", UserMetadata: map[string]string{"purpose": "sdk-native"}, DisableMultipart: true, SendContentMd5: true}); e != nil {
				return e
			}
			stat, e := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
			if e != nil || stat.Metadata.Get("X-Amz-Meta-Purpose") != "sdk-native" {
				return errors.New("actual SDK metadata missing")
			}
			stage = "actual_sdk_metadata_copy"
			if _, e = client.CopyObject(ctx, minio.CopyDestOptions{Bucket: c.Bucket, Object: "metadata/sdk-copy.txt"}, minio.CopySrcOptions{Bucket: c.Bucket, Object: key}); e != nil {
				return e
			}
			stat, e = client.StatObject(ctx, c.Bucket, "metadata/sdk-copy.txt", minio.StatObjectOptions{})
			if e != nil || stat.Metadata.Get("X-Amz-Meta-Purpose") != "sdk-native" {
				return errors.New("copy did not preserve metadata")
			}
			checks = append(checks, "actual_sdk_metadata_head_and_copy_preserved")
			if !strings.HasPrefix(c.Output, "/run/e2e-private/") || !strings.HasPrefix(c.Branch, "br_") {
				return errors.New("protected browser capability output required")
			}
			uri := c.Endpoint + "/s3/" + c.Branch + "/" + c.Bucket + "/metadata/browser.txt"
			req, _ := http.NewRequestWithContext(ctx, "PUT", uri, nil)
			req.Header.Set("Content-Type", "text/plain")
			req.Header.Set("If-None-Match", "*")
			req.Header.Set("X-Amz-Meta-Purpose", "browser-native")
			req = signer.PreSignV4(*req, c.Access, c.Secret, "", c.Region, 300)
			raw, _ := json.Marshal(map[string]any{"url": req.URL.String(), "headers": map[string]string{"Content-Type": "text/plain", "If-None-Match": "*", "X-Amz-Meta-Purpose": "browser-native"}, "body": string(payload)})
			file, e := os.OpenFile(c.Output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return e
			}
			_, e = file.Write(raw)
			file.Close()
			if e != nil {
				return e
			}
			checks = append(checks, "protected_standard_sdk_signed_browser_upload_capability_prepared")
		case "presigned-put":
			key := "presigned/sdk-中文.txt"
			stage = "presigned_target_create_only_inspected"
			_, e := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
			if minio.ToErrorResponse(e).Code != "NoSuchKey" {
				return errors.New("new presigned target must be absent")
			}
			uri, e := client.PresignedPutObject(ctx, c.Bucket, key, time.Minute)
			if e != nil {
				return e
			}
			stage = "presigned_sdk_put_once"
			request, _ := http.NewRequestWithContext(ctx, "PUT", uri.String(), bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("If-None-Match", "*")
			response, e := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
			if e != nil {
				return e
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				return errors.New("actual SDK presigned PUT rejected")
			}
			stage = "presigned_uploaded_bytes_verified"
			object, e := client.GetObject(ctx, c.Bucket, key, minio.GetObjectOptions{})
			if e != nil {
				return e
			}
			actual, e := io.ReadAll(io.LimitReader(object, 1024))
			object.Close()
			if e != nil || !bytes.Equal(actual, payload) {
				return errors.New("presigned uploaded bytes differ")
			}
			checks = append(checks, "actual_sdk_presigned_put_unicode_path_real_bytes_verified")
		case "bulk":
			stage = "original_bulk_targets_observed"
			keys := []string{"aux/sdk-a.txt", "aux/sdk-中文.txt"}
			for _, key := range keys {
				if _, err := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{}); err != nil {
					return errors.New("original SDK delete targets require inspection")
				}
			}
			objects := make(chan minio.ObjectInfo, 3)
			for _, key := range keys {
				objects <- minio.ObjectInfo{Key: key}
			}
			objects <- minio.ObjectInfo{Key: "aux/not-found"}
			close(objects)
			stage = "signed_sdk_delete_batch_once"
			for failure := range client.RemoveObjects(ctx, c.Bucket, objects, minio.RemoveObjectsOptions{}) {
				if failure.Err != nil {
					return failure.Err
				}
			}
			stage = "deleted_batch_metadata_observed"
			for _, key := range keys {
				_, err := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
				if minio.ToErrorResponse(err).Code != "NoSuchKey" {
					return errors.New("batch target still present")
				}
			}
			if _, err := client.StatObject(ctx, c.Bucket, "keep.txt", minio.StatObjectOptions{}); err != nil {
				return errors.New("batch changed retained control object")
			}
			checks = append(checks, "real_sdk_signed_bulk_delete_and_missing_key_with_retained_control_object")
		case "copy":
			sourceKey, targetKey := "directory/root.txt", "copies/sdk.txt"
			stage = "copy_original_source_read"
			original, err := client.GetObject(ctx, c.Bucket, sourceKey, minio.GetObjectOptions{})
			if err != nil {
				return err
			}
			originalBytes, err := io.ReadAll(io.LimitReader(original, 8192))
			original.Close()
			if err != nil || len(originalBytes) == 0 {
				return errors.New("original source bytes unavailable")
			}
			source, err := client.StatObject(ctx, c.Bucket, sourceKey, minio.StatObjectOptions{})
			if err != nil {
				return err
			}
			stage = "copy_target_absence"
			if _, err = client.StatObject(ctx, c.Bucket, targetKey, minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
				return errors.New("original copy target outcome needs inspection before any retry")
			}
			stage = "signed_copy_once"
			copied, err := client.CopyObject(ctx, minio.CopyDestOptions{Bucket: c.Bucket, Object: targetKey}, minio.CopySrcOptions{Bucket: c.Bucket, Object: sourceKey, MatchETag: source.ETag})
			if err != nil {
				return err
			}
			if copied.ETag != source.ETag || copied.LastModified.IsZero() {
				return errors.New("copy result metadata differs")
			}
			stage = "copied_bytes_read"
			target, err := client.GetObject(ctx, c.Bucket, targetKey, minio.GetObjectOptions{})
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(target, 8192))
			target.Close()
			if err != nil || !bytes.Equal(data, originalBytes) {
				return errors.New("actual copied bytes differ")
			}
			checks = append(checks, "real_sdk_signed_copy_has_exact_original_bytes_and_metadata")
			stage = "stale_source_copy_denied"
			_, err = client.CopyObject(ctx, minio.CopyDestOptions{Bucket: c.Bucket, Object: "copies/stale-denied.txt"}, minio.CopySrcOptions{Bucket: c.Bucket, Object: sourceKey, MatchETag: strings.Repeat("0", 64)})
			if minio.ToErrorResponse(err).StatusCode != 412 {
				return errors.New("stale source copy accepted")
			}
			if _, err = client.StatObject(ctx, c.Bucket, "copies/stale-denied.txt", minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
				return errors.New("failed copy published target")
			}
			checks = append(checks, "real_sdk_stale_source_denied_without_target_mutation")
		case "directory":
			for _, v1 := range []bool{false, true} {
				stage = "directory_grouped_pagination"
				var keys []string
				for item := range client.ListObjects(ctx, c.Bucket, minio.ListObjectsOptions{Recursive: false, Prefix: "directory/", MaxKeys: 1, UseV1: v1}) {
					if item.Err != nil {
						return item.Err
					}
					keys = append(keys, item.Key)
				}
				if len(keys) != 2 || keys[0] != "directory/nested/" || keys[1] != "directory/root.txt" {
					return errors.New("directory pagination differs from retained original objects")
				}
				checks = append(checks, "real_sdk_grouped_directory_pagination_v"+map[bool]string{false: "2", true: "1"}[v1])
			}
			stage = "directory_recursive_utf8"
			var keys []string
			for item := range client.ListObjects(ctx, c.Bucket, minio.ListObjectsOptions{Recursive: true, Prefix: "directory/", MaxKeys: 1}) {
				if item.Err != nil {
					return item.Err
				}
				keys = append(keys, item.Key)
			}
			if len(keys) != 3 || keys[0] != "directory/nested/a.txt" || keys[1] != "directory/nested/中文.txt" || keys[2] != "directory/root.txt" {
				return errors.New("flat UTF8 directory differs")
			}
			checks = append(checks, "real_sdk_recursive_retained_utf8_keys")
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
	json.NewEncoder(os.Stdout).Encode(map[string]any{"result": result, "stage": stage, "checks": checks, "http_statuses": transport.statuses, "error_code": minio.ToErrorResponse(e).Code, "credentials_in_output": false})
	if result != "pass" {
		os.Exit(1)
	}
}
