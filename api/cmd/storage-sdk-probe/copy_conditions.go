package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/signer"
)

func probeCopyConditions(ctx context.Context, client *minio.Client, c config, transport *onceTransport, checkpoint func(string)) error {
	const sourceKey = "conditions/source.txt"
	source, err := client.StatObject(ctx, c.Bucket, sourceKey, minio.StatObjectOptions{})
	if err != nil {
		return err
	}
	original, err := client.GetObject(ctx, c.Bucket, sourceKey, minio.GetObjectOptions{})
	if err != nil {
		return err
	}
	content, err := io.ReadAll(io.LimitReader(original, 8192))
	original.Close()
	if err != nil || len(content) == 0 {
		return errors.New("Owned original bytes missing")
	}
	past, future := source.LastModified.Add(-time.Hour), source.LastModified.Add(time.Hour)
	for n, tc := range []struct {
		name   string
		source minio.CopySrcOptions
		allow  bool
	}{
		{"none_match_different", minio.CopySrcOptions{NoMatchETag: strings.Repeat("0", 64)}, true},
		{"none_match_same", minio.CopySrcOptions{NoMatchETag: source.ETag}, false},
		{"modified_since_past", minio.CopySrcOptions{MatchModifiedSince: past}, true},
		{"modified_since_future", minio.CopySrcOptions{MatchModifiedSince: future}, false},
		{"unmodified_since_future", minio.CopySrcOptions{MatchUnmodifiedSince: future}, true},
		{"unmodified_since_past", minio.CopySrcOptions{MatchUnmodifiedSince: past}, false},
		{"match_overrides_unmodified", minio.CopySrcOptions{MatchETag: source.ETag, MatchUnmodifiedSince: past}, true},
		{"none_match_blocks_even_if_modified", minio.CopySrcOptions{NoMatchETag: source.ETag, MatchModifiedSince: past}, false},
		{"none_match_overrides_modified", minio.CopySrcOptions{NoMatchETag: strings.Repeat("0", 64), MatchModifiedSince: future}, true},
	} {
		key := fmt.Sprintf("conditions/case-%d.txt", n)
		if _, err = client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
			return errors.New("Inspect original conditional-copy target before retry")
		}
		tc.source.Bucket = c.Bucket
		tc.source.Object = sourceKey
		copied, e := client.CopyObject(ctx, minio.CopyDestOptions{Bucket: c.Bucket, Object: key}, tc.source)
		if !tc.allow {
			if minio.ToErrorResponse(e).StatusCode != 412 {
				return errors.New("Expected conditional-copy 412: " + tc.name)
			}
			if _, e = client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{}); minio.ToErrorResponse(e).Code != "NoSuchKey" {
				return errors.New("Failed copy published a target")
			}
		} else {
			if e != nil {
				return e
			}
			if copied.ETag != source.ETag {
				return errors.New("Content ETag changed")
			}
			object, e := client.GetObject(ctx, c.Bucket, key, minio.GetObjectOptions{})
			if e != nil {
				return e
			}
			got, e := io.ReadAll(io.LimitReader(object, 8192))
			object.Close()
			if e != nil || !bytes.Equal(got, content) {
				return errors.New("Conditional copy bytes differ")
			}
		}
		checkpoint("real_sdk_copy_source_" + tc.name)
	}
	// Sign malformed and tampered conditions with the same SDK signer. Do not
	// relax service validation or retry these mutations behind the test's back.
	for n, tc := range []struct {
		name, date string
		tamper     bool
		status     int
	}{
		{"malformed_date", "not-a-date", false, 400},
		{"tampered_signed_date", past.UTC().Format(http.TimeFormat), true, 403},
	} {
		key := fmt.Sprintf("conditions/rejected-%d.txt", n)
		r, err := http.NewRequestWithContext(ctx, http.MethodPut, c.Endpoint+"/"+c.Bucket+"/"+key, nil)
		if err != nil {
			return err
		}
		r.Header.Set("X-Amz-Copy-Source", "/"+c.Bucket+"/"+sourceKey)
		r.Header.Set("X-Amz-Copy-Source-If-Modified-Since", tc.date)
		r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		r = signer.SignV4(*r, c.Access, c.Secret, "", c.Region)
		if tc.tamper {
			r.Header.Set("X-Amz-Copy-Source-If-Modified-Since", future.UTC().Format(http.TimeFormat))
		}
		response, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(r)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
		response.Body.Close()
		if response.StatusCode != tc.status {
			return errors.New("Unexpected condition validation response: " + tc.name)
		}
		if _, err = client.StatObject(ctx, c.Bucket, key, minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
			return errors.New("Rejected signed request published a target")
		}
		checkpoint("real_sdk_copy_source_" + tc.name + "_rejected_without_mutation")
	}
	return nil
}
