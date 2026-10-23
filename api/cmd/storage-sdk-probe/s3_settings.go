package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/minio/minio-go/v7/pkg/signer"
	"github.com/minio/minio-go/v7/pkg/tags"
)

type sdkTrailerSHA struct{ hash.Hash }

func (sdkTrailerSHA) Close() {}

// Every mutation is checkpointed before dispatch. A lost response requires
// inspection of that original action, never a new upload with the same name.
func probeS3Settings(ctx context.Context, client *minio.Client, c config, transport *onceTransport, stage, check func(string)) error {
	if !strings.HasPrefix(c.Output, "/run/e2e-private/") || c.Branch == "" {
		return errors.New("protected settings checkpoint and branch required")
	}
	state := struct {
		Bucket  string   `json:"bucket"`
		Checks  []string `json:"checks"`
		Running string   `json:"running"`
	}{Bucket: c.Bucket, Checks: []string{}}
	if raw, e := os.ReadFile(c.Output); e == nil {
		if json.Unmarshal(raw, &state) != nil || state.Bucket != c.Bucket {
			return errors.New("original SDK checkpoint differs")
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
			return errors.New("inspect original SDK action before continuing")
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
	raw := func(method, key, query string, body []byte, headers http.Header) (int, []byte, http.Header, error) {
		targetURL, e := url.Parse(c.Endpoint)
		if e != nil {
			return 0, nil, nil, e
		}
		targetURL.Path = "/" + c.Bucket
		if key != "" {
			targetURL.Path += "/" + key
		}
		if query != "" {
			targetURL.RawQuery = query
		}
		r, e := http.NewRequestWithContext(ctx, method, targetURL.String(), bytes.NewReader(body))
		if e != nil {
			return 0, nil, nil, e
		}
		r.Header = headers.Clone()
		if r.Header == nil {
			r.Header = http.Header{}
		}
		sum := sha256.Sum256(body)
		r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
		r = signer.SignV4(*r, c.Access, c.Secret, "", c.Region)
		response, e := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(r)
		if e != nil {
			return 0, nil, nil, e
		}
		defer response.Body.Close()
		data, e := io.ReadAll(io.LimitReader(response.Body, 131073))
		return response.StatusCode, data, response.Header, e
	}
	require := func(e error, ok bool) error {
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("new S3 settings response differs")
		}
		return nil
	}
	content := []byte("Owned S3 settings bytes, preserved across tags, headers and configuration.\n")
	verify := func(key string) error {
		o, e := client.GetObject(ctx, c.Bucket, key, minio.GetObjectOptions{})
		if e != nil {
			return e
		}
		data, e := io.ReadAll(io.LimitReader(o, 8192))
		o.Close()
		return require(e, bytes.Equal(data, content))
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"SDK_reads_UI_bucket_tags_and_stored_declarations", func() error {
			v, e := client.GetBucketTagging(ctx, c.Bucket)
			if e != nil {
				return e
			}
			if v.ToMap()["phase"] != "ui-successor" {
				return errors.New("original UI successor config missing")
			}
			vrs, e := client.GetBucketVersioning(ctx, c.Bucket)
			return require(e, vrs.Status == "Suspended")
		}},
		{"SDK_bucket_tags_put_get_delete", func() error {
			v, e := tags.NewTags(map[string]string{"phase": "sdk-bucket"}, false)
			if e != nil {
				return e
			}
			if e = client.SetBucketTagging(ctx, c.Bucket, v); e != nil {
				return e
			}
			v, e = client.GetBucketTagging(ctx, c.Bucket)
			if e != nil || v.ToMap()["phase"] != "sdk-bucket" {
				return require(e, false)
			}
			if e = client.RemoveBucketTagging(ctx, c.Bucket); e != nil {
				return e
			}
			v, e = client.GetBucketTagging(ctx, c.Bucket)
			if e != nil {
				return e
			}
			return require(nil, len(v.ToMap()) == 0)
		}},
		{"SDK_versioning_lifecycle_and_object_lock_are_stored_only", func() error {
			if e := client.SetBucketVersioning(ctx, c.Bucket, minio.BucketVersioningConfiguration{Status: "Enabled"}); e != nil {
				return e
			}
			v, e := client.GetBucketVersioning(ctx, c.Bucket)
			if e != nil || v.Status != "Enabled" {
				return require(e, false)
			}
			cfg := &lifecycle.Configuration{Rules: []lifecycle.Rule{{ID: "stored-only", Status: "Disabled", RuleFilter: lifecycle.Filter{Prefix: "settings/"}, Expiration: lifecycle.Expiration{Days: 30}}}}
			if e = client.SetBucketLifecycle(ctx, c.Bucket, cfg); e != nil {
				return e
			}
			cfg, e = client.GetBucketLifecycle(ctx, c.Bucket)
			if e != nil || len(cfg.Rules) != 1 || cfg.Rules[0].ID != "stored-only" {
				return require(e, false)
			}
			if e = client.SetBucketLifecycle(ctx, c.Bucket, nil); e != nil {
				return e
			}
			mode, unit, days := minio.Governance, minio.Days, uint(2)
			if e = client.SetBucketObjectLockConfig(ctx, c.Bucket, &mode, &days, &unit); e != nil {
				return e
			}
			actualMode, actualDays, actualUnit, e := client.GetBucketObjectLockConfig(ctx, c.Bucket)
			return require(e, actualMode != nil && actualDays != nil && actualUnit != nil && *actualMode == mode && *actualDays == days && *actualUnit == unit)
		}},
		{"SDK_public_access_block_round_trip_and_read_only_ACL_policy", func() error {
			status, _, _, e := raw("PUT", "", "publicAccessBlock", []byte(`<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`), nil)
			if e != nil || status != 200 {
				return require(e, false)
			}
			status, data, _, e := raw("GET", "", "publicAccessBlock", nil, nil)
			if e != nil || status != 200 || !bytes.Contains(data, []byte("<IgnorePublicAcls>true")) {
				return require(e, false)
			}
			status, _, _, e = raw("DELETE", "", "publicAccessBlock", nil, nil)
			if e != nil || status != 204 {
				return require(e, false)
			}
			status, data, _, e = raw("GET", "", "acl", nil, nil)
			if e != nil || status != 200 || !bytes.Contains(data, []byte(c.Branch)) {
				return require(e, false)
			}
			policy, e := client.GetBucketPolicy(ctx, c.Bucket)
			return require(e, strings.Contains(policy, "Statement"))
		}},
		{"SDK_put_tags_headers_and_object_attributes", func() error {
			_, e := client.PutObject(ctx, c.Bucket, "settings/source.txt", bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{ContentType: "text/plain", ContentDisposition: `attachment; filename="source.txt"`, CacheControl: "private, max-age=7", UserTags: map[string]string{"phase": "source"}, UserMetadata: map[string]string{"purpose": "settings"}, SendContentMd5: true, DisableMultipart: true})
			if e != nil {
				return e
			}
			st, e := client.StatObject(ctx, c.Bucket, "settings/source.txt", minio.StatObjectOptions{})
			if e != nil || st.Metadata.Get("Cache-Control") != "private, max-age=7" || st.Metadata.Get("Content-Disposition") == "" {
				return require(e, false)
			}
			tag, e := client.GetObjectTagging(ctx, c.Bucket, "settings/source.txt", minio.GetObjectTaggingOptions{})
			if e != nil || tag.ToMap()["phase"] != "source" {
				return require(e, false)
			}
			status, data, _, e := raw("GET", "settings/source.txt", "attributes", nil, http.Header{"X-Amz-Object-Attributes": []string{"ETag,ObjectSize,Checksum,StorageClass"}})
			var attributes minio.ObjectAttributesResponse
			if e != nil || status != 200 || xml.Unmarshal(data, &attributes) != nil || attributes.ObjectSize != len(content) || attributes.StorageClass != "STANDARD" {
				return require(e, false)
			}
			return verify("settings/source.txt")
		}},
		{"SDK_object_tag_CRUD_and_independent_COPY_directives", func() error {
			v, e := tags.NewTags(map[string]string{"phase": "updated"}, true)
			if e != nil {
				return e
			}
			if e = client.PutObjectTagging(ctx, c.Bucket, "settings/source.txt", v, minio.PutObjectTaggingOptions{VersionID: "null"}); e != nil {
				return e
			}
			src := minio.CopySrcOptions{Bucket: c.Bucket, Object: "settings/source.txt"}
			if _, e = client.CopyObject(ctx, minio.CopyDestOptions{Bucket: c.Bucket, Object: "settings/copied.txt"}, src); e != nil {
				return e
			}
			v, e = client.GetObjectTagging(ctx, c.Bucket, "settings/copied.txt", minio.GetObjectTaggingOptions{})
			if e != nil || v.ToMap()["phase"] != "updated" {
				return require(e, false)
			}
			st, e := client.StatObject(ctx, c.Bucket, "settings/copied.txt", minio.StatObjectOptions{})
			if e != nil || st.Metadata.Get("Cache-Control") != "private, max-age=7" {
				return require(e, false)
			}
			if _, e = client.CopyObject(ctx, minio.CopyDestOptions{Bucket: c.Bucket, Object: "settings/replaced.txt", ReplaceMetadata: true, ContentType: "text/plain", CacheControl: "no-cache", ContentDisposition: `inline; filename="copy.txt"`, ReplaceTags: true, UserTags: map[string]string{"phase": "replaced"}}, src); e != nil {
				return e
			}
			v, e = client.GetObjectTagging(ctx, c.Bucket, "settings/replaced.txt", minio.GetObjectTaggingOptions{})
			if e != nil || v.ToMap()["phase"] != "replaced" {
				return require(e, false)
			}
			if e = client.RemoveObjectTagging(ctx, c.Bucket, "settings/source.txt", minio.RemoveObjectTaggingOptions{}); e != nil {
				return e
			}
			v, e = client.GetObjectTagging(ctx, c.Bucket, "settings/source.txt", minio.GetObjectTaggingOptions{})
			if e != nil {
				return e
			}
			return require(nil, len(v.ToMap()) == 0)
		}},
		{"SDK_multipart_keeps_initiation_tags_and_response_headers", func() error {
			core := minio.Core{Client: client}
			key := "settings/multipart.txt"
			id, e := core.NewMultipartUpload(ctx, c.Bucket, key, minio.PutObjectOptions{ContentType: "text/plain", UserTags: map[string]string{"phase": "multipart"}, CacheControl: "no-store", ContentDisposition: `attachment; filename="multipart.txt"`})
			if e != nil {
				return e
			}
			part, e := core.PutObjectPart(ctx, c.Bucket, key, id, 1, bytes.NewReader(content), int64(len(content)), minio.PutObjectPartOptions{})
			if e != nil {
				return e
			}
			if _, e = core.CompleteMultipartUpload(ctx, c.Bucket, key, id, []minio.CompletePart{{PartNumber: 1, ETag: part.ETag}}, minio.PutObjectOptions{}); e != nil {
				return e
			}
			v, e := client.GetObjectTagging(ctx, c.Bucket, key, minio.GetObjectTaggingOptions{})
			if e != nil || v.ToMap()["phase"] != "multipart" {
				return require(e, false)
			}
			st, e := client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{})
			if e != nil || st.Metadata.Get("Cache-Control") != "no-store" {
				return require(e, false)
			}
			return verify(key)
		}},
	}
	for _, v := range steps {
		if e := step(v.name, v.run); e != nil {
			return e
		}
	}
	if e := step("SDK_signed_and_unsigned_trailers_verified_before_publication", func() error {
		for _, signed := range []bool{true, false} {
			key, algorithm := "settings/signed-trailer.txt", "crc32c"
			digest := hash.Hash(crc32.New(crc32.MakeTable(crc32.Castagnoli)))
			if !signed {
				key, algorithm = "settings/unsigned-trailer.txt", "sha256"
				digest = sha256.New()
			}
			digest.Write(content)
			r, _ := http.NewRequestWithContext(ctx, "PUT", c.Endpoint+"/"+c.Bucket+"/"+key, bytes.NewReader(content))
			r.Header.Set("Content-Type", "text/plain")
			r.Header.Set("Content-Encoding", "aws-chunked")
			r.Trailer = http.Header{}
			r.Trailer.Set("X-Amz-Checksum-"+algorithm, base64.StdEncoding.EncodeToString(digest.Sum(nil)))
			if signed {
				r = signer.StreamingSignV4(r, c.Access, c.Secret, "", c.Region, int64(len(content)), time.Now().UTC(), sdkTrailerSHA{sha256.New()})
			} else {
				r.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
				r.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(content)))
				r.Header.Set("X-Amz-Trailer", "x-amz-checksum-"+algorithm)
				r = signer.StreamingUnsignedV4(r, "", int64(len(content)), time.Now().UTC())
				r = signer.SignV4(*r, c.Access, c.Secret, "", c.Region)
			}
			r.TransferEncoding = nil
			response, e := (&http.Client{Transport: transport}).Do(r)
			if e != nil {
				return e
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				return errors.New("live SDK trailer rejected")
			}
			if e = verify(key); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		return e
	}
	return step("SDK_browser_POST_policy_success_and_denials", func() error {
		p := minio.NewPostPolicy()
		for _, e := range []error{p.SetBucket(c.Bucket), p.SetKeyStartsWith("forms/"), p.SetExpires(time.Now().Add(10 * time.Minute)), p.SetContentType("text/plain"), p.SetContentDisposition(`attachment; filename=browser.txt`), p.SetContentLengthRange(1, 8192), p.SetUserMetadata("purpose", "browser-post"), p.SetSuccessStatusAction("201")} {
			if e != nil {
				return e
			}
		}
		target, fields, e := client.PresignedPostPolicy(ctx, p)
		if e != nil {
			return e
		}
		fields["key"] = "forms/${filename}"
		submit := func(target string, values map[string]string, filename string) (int, []byte, error) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			names := []string{}
			for k := range values {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if e := writer.WriteField(k, values[k]); e != nil {
					return 0, nil, e
				}
			}
			part, e := writer.CreateFormFile("file", filename)
			if e != nil {
				return 0, nil, e
			}
			part.Write(content)
			writer.Close()
			r, _ := http.NewRequestWithContext(ctx, "POST", target, &body)
			r.Header.Set("Content-Type", writer.FormDataContentType())
			response, e := (&http.Client{Transport: transport.RoundTripper, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(r)
			if e != nil {
				return 0, nil, e
			}
			defer response.Body.Close()
			data, e := io.ReadAll(io.LimitReader(response.Body, 8192))
			transport.mu.Lock()
			transport.statuses = append(transport.statuses, response.StatusCode)
			transport.mu.Unlock()
			return response.StatusCode, data, e
		}
		status, body, e := submit(target.String(), fields, "browser.txt")
		if e != nil || status != 201 || !bytes.Contains(body, []byte("<PostResponse>")) {
			return require(e, false)
		}
		if e = verify("forms/browser.txt"); e != nil {
			return e
		}
		bad := map[string]string{}
		for k, v := range fields {
			bad[k] = v
		}
		bad["x-amz-signature"] = strings.Repeat("0", 64)
		status, _, e = submit(target.String(), bad, "rejected-signature.txt")
		if e != nil || status != 403 {
			return require(e, false)
		}
		bad = map[string]string{}
		for k, v := range fields {
			bad[k] = v
		}
		bad["key"] = "outside/rejected-prefix.txt"
		status, _, e = submit(target.String(), bad, "file.txt")
		if e != nil || status != 403 {
			return require(e, false)
		}
		for _, key := range []string{"forms/rejected-signature.txt", "outside/rejected-prefix.txt"} {
			if _, e = client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{}); minio.ToErrorResponse(e).Code != "NoSuchKey" {
				return errors.New("rejected POST published an object")
			}
		}
		return nil
	})
}
