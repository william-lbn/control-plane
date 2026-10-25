package control

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestMultipartChecksumNegotiationAndOperationGrammar(t *testing.T) {
	for algorithm := range storageChecksumAlgorithms {
		for _, kind := range []string{"FULL_OBJECT", "COMPOSITE"} {
			_, err := normalizeMultipartChecksum(algorithm, kind)
			valid := kind == "FULL_OBJECT" && strings.HasPrefix(algorithm, "CRC") || kind == "COMPOSITE" && algorithm != "CRC64NVME"
			if (err == nil) != valid {
				t.Fatal(algorithm, kind, err)
			}
		}
		kind, err := normalizeMultipartChecksum(algorithm, "")
		if err != nil || (kind == "FULL_OBJECT") != (algorithm == "CRC64NVME") {
			t.Fatal("default", algorithm, kind, err)
		}
	}
	for _, algorithm := range []string{"crc32", "SHA512", "XXHASH64", "UNKNOWN"} {
		if _, err := normalizeMultipartChecksum(algorithm, "FULL_OBJECT"); err == nil {
			t.Fatal("unadvertised algorithm accepted", algorithm)
		}
	}
	if _, err := normalizeMultipartChecksum("", "COMPOSITE"); err == nil {
		t.Fatal("type without algorithm")
	}
	for _, item := range []struct{ operation, name, value string }{
		{"create", "X-Amz-Checksum-Algorithm", "SHA256"}, {"create", "X-Amz-Checksum-Type", "COMPOSITE"}, {"part", "X-Amz-Sdk-Checksum-Algorithm", "SHA256"}, {"part", "X-Amz-Checksum-Sha256", "value"}, {"complete", "X-Amz-Checksum-Type", "COMPOSITE"}, {"complete", "X-Amz-Mp-Object-Size", "9"},
	} {
		h := http.Header{}
		h.Set(item.name, item.value)
		if err := multipartChecksumHeaders(h, item.operation); err != nil {
			t.Fatal(item, err)
		}
		if err := multipartChecksumHeaders(h, ""); err == nil {
			t.Fatal("list/abort ignored checksum", item)
		}
		h.Add(item.name, item.value)
		if err := multipartChecksumHeaders(h, item.operation); err == nil {
			t.Fatal("duplicate checksum declaration")
		}
	}
	for _, size := range []string{"-1", "+9", " 9", "9x", "5368709121"} {
		h := http.Header{}
		h.Set("X-Amz-Mp-Object-Size", size)
		if _, err := multipartCompletionHeaders(h); err == nil {
			t.Fatal("invalid full size", size)
		}
	}
}

func TestMultipartChecksumXMLAndExactLegacyFingerprint(t *testing.T) {
	part := multipartSelection{Number: 1, ETag: strings.Repeat("a", 32)}
	h, err := multipartManifestHash([]multipartSelection{part}, "", "*")
	// The original v1 JSON receipt must survive the additive checksum fields.
	raw := `{"match":"","none":"*","parts":[{"part_number":1,"etag":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`
	if err != nil || h != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) {
		t.Fatal("legacy manifest changed", h, err)
	}
	u := multipartCreate{Key: "key", ContentType: "text/plain", Metadata: map[string]string{}, Size: -1}
	b, _ := json.Marshal(u)
	if strings.Contains(string(b), "checksum") {
		t.Fatal("legacy initiation fingerprint changed", string(b))
	}
	p, _ := smallStoragePayload([]byte("123456789"))
	for algorithm, name := range storageChecksumAlgorithms {
		field := "Checksum" + algorithm
		x := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"` + part.ETag + `"</ETag><` + field + `>` + p.checksums[name] + `</` + field + `></Part></CompleteMultipartUpload>`
		items, err := parseMultipartCompletion([]byte(x))
		if err != nil || len(items) != 1 || items[0].Checksums[name] != p.checksums[name] {
			t.Fatal(algorithm, items, err)
		}
		for _, bad := range []string{strings.Replace(x, "</"+field+">", "</"+field+"><"+field+">"+p.checksums[name]+"</"+field+">", 1), strings.ReplaceAll(x, field, algorithm), strings.Replace(x, p.checksums[name], "bad", 1), strings.ReplaceAll(x, field, "ChecksumUNKNOWN")} {
			if _, err := parseMultipartCompletion([]byte(bad)); err == nil {
				t.Fatal("ambiguous/unknown checksum XML", bad)
			}
		}
	}
}

func TestMultipartChecksumCompositionAndPublicationPolicy(t *testing.T) {
	p, _ := smallStoragePayload([]byte("123456789"))
	for algorithm, name := range storageChecksumAlgorithms {
		if algorithm == "CRC64NVME" {
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(p.checksums[name])
		var expected string
		switch algorithm {
		case "SHA256":
			h := sha256.Sum256(raw)
			expected = base64.StdEncoding.EncodeToString(h[:])
		case "SHA1":
			h := sha1.Sum(raw)
			expected = base64.StdEncoding.EncodeToString(h[:])
		case "CRC32":
			n := crc32.ChecksumIEEE(raw)
			expected = base64.StdEncoding.EncodeToString([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		case "CRC32C":
			n := crc32.Checksum(raw, crc32.MakeTable(crc32.Castagnoli))
			expected = base64.StdEncoding.EncodeToString([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		}
		u := multipartUpload{ChecksumAlgorithm: algorithm, ChecksumType: "COMPOSITE"}
		policy, err := multipartPublishedChecksum(u, []multipartSelection{{Number: 1}}, map[int]multipartPart{1: {Checksums: p.storedChecksums()}}, p, multipartCompletionChecksums{})
		if err != nil || policy.Value != expected+"-1" {
			t.Fatal(algorithm, policy, err)
		}
		visible, kind, err := storagePolicyValues(p.storedChecksums(), policy)
		if err != nil || kind != "COMPOSITE" || len(visible) != 1 || visible[name] != policy.Value {
			t.Fatal("mixed full/composite publication", algorithm, visible, err)
		}
		bad := policy
		bad.Value = expected + "-2"
		if _, _, err := storagePolicyValues(p.storedChecksums(), bad); err == nil {
			t.Fatal("wrong part count")
		}
		bad = policy
		bad.Value = "AAAA-1"
		if _, _, err := storagePolicyValues(p.storedChecksums(), bad); err == nil {
			t.Fatal("wrong digest width")
		}
	}
}

func multipartChecksumWire(t *testing.T, s *server, f *settingsPGFixture, method, key, query string, h http.Header, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/settings-bucket/"+key+query, strings.NewReader(body))
	r.Header = h
	if r.Header == nil {
		r.Header = http.Header{}
	}
	w := httptest.NewRecorder()
	s.s3Multipart(w, r, f.conn, f.p, "checksum-actor", "settings-bucket", key, []byte(body), r.URL.Query())
	return w
}

func TestMultipartChecksumPostgresSevenNegotiatedWireCombinations(t *testing.T) {
	s, f := checksumBlobFixture(t)
	body := []byte("123456789")
	payload, _ := smallStoragePayload(body)
	for algorithm, name := range storageChecksumAlgorithms {
		for _, kind := range []string{"FULL_OBJECT", "COMPOSITE"} {
			if _, err := normalizeMultipartChecksum(algorithm, kind); err != nil {
				continue
			}
			t.Run(algorithm+"-"+kind, func(t *testing.T) {
				key := "negotiated-" + strings.ToLower(algorithm+"-"+kind)
				h := http.Header{}
				h.Set("X-Amz-Checksum-Algorithm", algorithm)
				h.Set("X-Amz-Checksum-Type", kind)
				w := multipartChecksumWire(t, s, f, "POST", key, "?uploads", h, "")
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				var init struct {
					ID string `xml:"UploadId"`
				}
				if err := xml.Unmarshal(w.Body.Bytes(), &init); err != nil || init.ID == "" {
					t.Fatal(err, w.Body.String())
				}
				h = http.Header{}
				h.Set("X-Amz-Sdk-Checksum-Algorithm", algorithm)
				h.Set(name, payload.checksums[name])
				w = multipartChecksumWire(t, s, f, "PUT", key, "?uploadId="+init.ID+"&partNumber=1", h, string(body))
				if w.Code != 200 || w.Header().Get(name) != payload.checksums[name] {
					t.Fatal(w.Code, w.Body.String())
				}
				etag := w.Header().Get("ETag")
				w = multipartChecksumWire(t, s, f, "GET", key, "?uploadId="+init.ID, nil, "")
				if w.Code != 200 || !strings.Contains(w.Body.String(), "<Checksum"+algorithm+">"+payload.checksums[name]) || !strings.Contains(w.Body.String(), "<ChecksumType>"+kind) {
					t.Fatal(w.Code, w.Body.String())
				}
				manifest := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + etag + `</ETag><Checksum` + algorithm + `>` + payload.checksums[name] + `</Checksum` + algorithm + `></Part></CompleteMultipartUpload>`
				h = http.Header{}
				h.Set("X-Amz-Checksum-Type", kind)
				h.Set("X-Amz-Mp-Object-Size", "9")
				w = multipartChecksumWire(t, s, f, "POST", key, "?uploadId="+init.ID, h, manifest)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				var policy storageChecksumPolicy
				var full map[string]string
				if err := f.conn.QueryRow(f.ctx, `SELECT checksum_policy,checksums FROM neon_storage.objects WHERE key=$1`, key).Scan(&policy, &full); err != nil || !reflect.DeepEqual(full, payload.storedChecksums()) || policy.Type != kind {
					t.Fatal(policy, err)
				}
				if !strings.Contains(w.Body.String(), "<Checksum"+algorithm+">"+policy.Value) || !strings.Contains(w.Body.String(), "<ChecksumType>"+kind) {
					t.Fatal("completion receipt", w.Body.String())
				}
				for _, method := range []string{"GET", "HEAD"} {
					r := httptest.NewRequest(method, "/"+key, nil)
					r.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
					read := httptest.NewRecorder()
					s.serveS3Object(read, r, f.conn, f.p, "settings-bucket", key)
					if read.Code != 200 || read.Header().Get(name) != policy.Value || read.Header().Get("X-Amz-Checksum-Type") != kind {
						t.Fatal("read policy", read.Code, read.Body.String())
					}
					for other := range storageChecksumSizes {
						if other != name && read.Header().Get(other) != "" {
							t.Fatal("full checksum mislabeled composite", other)
						}
					}
					if method == "GET" && !bytes.Equal(read.Body.Bytes(), body) {
						t.Fatal("body changed")
					}
				}
				r := httptest.NewRequest("GET", "/"+key+"?attributes", nil)
				r.Header.Set("X-Amz-Object-Attributes", "Checksum,ObjectParts")
				attr := httptest.NewRecorder()
				s.s3ObjectAttributes(attr, r, f.conn, f.p, "settings-bucket", key)
				if attr.Code != 200 || !strings.Contains(attr.Body.String(), "<Checksum"+algorithm+">"+policy.Value) || !strings.Contains(attr.Body.String(), "<Checksum"+algorithm+">"+payload.checksums[name]) {
					t.Fatal("attributes policy/part", attr.Code, attr.Body.String())
				}
			})
		}
	}
}

func TestMultipartChecksumPostgresTwoPartsDenialsAndOriginalReceipt(t *testing.T) {
	s, f := checksumBlobFixture(t)
	u, err := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "original-checksum-receipt", multipartCreate{Key: "two-parts", Size: -1, ChecksumAlgorithm: "SHA256", ChecksumType: "COMPOSITE"})
	if err != nil {
		t.Fatal(err)
	}
	data := [][]byte{bytes.Repeat([]byte("a"), multipartMinimumPart), []byte("tail")}
	parts := []multipartSelection{}
	for i, body := range data {
		p, _ := smallStoragePayload(body)
		h := http.Header{}
		h.Set("X-Amz-Checksum-Sha256", p.checksums["X-Amz-Checksum-Sha256"])
		bad := h.Clone()
		bad.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(make([]byte, 32)))
		if _, err := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, i+1, body, "", bad); err == nil {
			t.Fatal("bad digest published part")
		}
		part, err := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, i+1, body, "", h)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, multipartSelection{Number: i + 1, ETag: part.ETag, Checksums: map[string]string{"X-Amz-Checksum-Sha256": p.checksums["X-Amz-Checksum-Sha256"]}})
	}
	bad := append([]multipartSelection{}, parts...)
	bad[1].Number = 3
	if _, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, bad, "", "*"); err == nil {
		t.Fatal("nonconsecutive explicit checksum parts")
	}
	bad = append([]multipartSelection{}, parts...)
	bad[1].Checksums = nil
	if _, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, bad, "", "*"); err == nil {
		t.Fatal("missing composite manifest digest")
	}
	if _, err := s.completeMultipart(f.ctx, f.conn, f.p, "foreign", "settings-bucket", u.Key, u.ID, parts, "", "*"); err == nil {
		t.Fatal("foreign completion")
	}
	for _, opt := range []multipartCompletionChecksums{{Type: "FULL_OBJECT"}, {Values: map[string]string{"X-Amz-Checksum-Sha256": "wrong-2"}}, {Size: func() *int64 { n := int64(1); return &n }()}} {
		if _, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, parts, "", "*", opt); err == nil {
			t.Fatal("bad completion accepted", opt)
		}
	}
	var count int
	if err := f.conn.QueryRow(f.ctx, `SELECT count(*) FROM neon_storage.objects WHERE key=$1`, u.Key).Scan(&count); err != nil || count != 0 {
		t.Fatal("denial published destination", count, err)
	}
	first, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, parts, "", "*")
	if err != nil {
		t.Fatal(err)
	}
	a := sha256.Sum256(data[0])
	b := sha256.Sum256(data[1])
	joined := append(a[:], b[:]...)
	expected := sha256.Sum256(joined)
	if first.ChecksumPolicy.Value != base64.StdEncoding.EncodeToString(expected[:])+"-2" {
		t.Fatal("two part composition", first.ChecksumPolicy)
	}
	if _, err := f.conn.Exec(f.ctx, `DELETE FROM neon_storage.objects WHERE key=$1`, u.Key); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, parts, "", "*")
	if err != nil || replayed.ChecksumPolicy != first.ChecksumPolicy {
		t.Fatal("original receipt lost after deletion", replayed, err)
	}
	if err := f.conn.QueryRow(f.ctx, `SELECT count(*) FROM neon_storage.objects WHERE key=$1`, u.Key).Scan(&count); err != nil || count != 0 {
		t.Fatal("replay republished deleted key", count, err)
	}
	if _, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, parts, "", "*", multipartCompletionChecksums{Type: "COMPOSITE"}); err == nil {
		t.Fatal("changed original headers replayed")
	}
}

func TestMultipartChecksumPostgresCopyOverwriteAndPartCopy(t *testing.T) {
	s, f := checksumBlobFixture(t)
	data := []byte("123456789")
	payload, _ := smallStoragePayload(data)
	u, err := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "copy-policy-upload", multipartCreate{Key: "composite-source", Size: -1, ChecksumAlgorithm: "CRC32C", ChecksumType: "COMPOSITE"})
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("X-Amz-Checksum-Crc32c", payload.checksums["X-Amz-Checksum-Crc32c"])
	part, err := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, 1, data, "", h)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, []multipartSelection{{Number: 1, ETag: part.ETag, Checksums: map[string]string{"X-Amz-Checksum-Crc32c": h.Get("X-Amz-Checksum-Crc32c")}}}, "", "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		key         string
		replacement []storageCopyMetadata
		expected    storageChecksumPolicy
	}{
		{"composite-copy", nil, first.ChecksumPolicy},
		{"full-copy", []storageCopyMetadata{{OnlyTags: true, ChecksumAlgorithm: "SHA256"}}, storageChecksumPolicy{Algorithm: "SHA256", Type: "FULL_OBJECT", Value: payload.checksums["X-Amz-Checksum-Sha256"], Parts: 1}},
	} {
		if _, err := copyStorageObject(f.ctx, f.conn, f.p.ProjectID, "settings-bucket", u.Key, "settings-bucket", item.key, "", "", "*", item.replacement...); err != nil {
			t.Fatal(err)
		}
		var policy storageChecksumPolicy
		var layout []storageObjectPart
		if err := f.conn.QueryRow(f.ctx, `SELECT checksum_policy,part_layout FROM neon_storage.objects WHERE key=$1`, item.key).Scan(&policy, &layout); err != nil || policy != item.expected || len(layout) != 0 {
			t.Fatal("copy value/type or retained layout", policy, err)
		}
	}
	copyUpload, err := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "copy-part-upload", multipartCreate{Key: "part-copy", Size: -1, ChecksumAlgorithm: "SHA1", ChecksumType: "COMPOSITE"})
	if err != nil {
		t.Fatal(err)
	}
	copyHeader := http.Header{}
	copyHeader.Set("X-Amz-Copy-Source", "/settings-bucket/composite-copy")
	copyHeader.Set("X-Amz-Copy-Source-Range", "bytes=1-3")
	copied, err := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", copyUpload.Key, copyUpload.ID, 1, nil, "", copyHeader)
	expected, _ := smallStoragePayload([]byte("234"))
	if err != nil || copied.Checksums["X-Amz-Checksum-Sha1"] != expected.checksums["X-Amz-Checksum-Sha1"] {
		t.Fatal("copy part checksums actual range", copied, err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/composite-copy", nil)
	s.s3WritePayload(w, r, f.conn, f.p, "settings-bucket", "composite-copy", payload)
	var policy storageChecksumPolicy
	if err := f.conn.QueryRow(f.ctx, `SELECT checksum_policy FROM neon_storage.objects WHERE key='composite-copy'`).Scan(&policy); err != nil || w.Code != 200 || policy != (storageChecksumPolicy{}) {
		t.Fatal("overwrite retained composite generation", policy, w.Code, err)
	}
}

func TestMultipartChecksumPostgresAdditiveUpgradeAndLegacyReceipt(t *testing.T) {
	s, f := checksumBlobFixture(t)
	u, err := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "legacy-checksum-upgrade", multipartCreate{Key: "legacy-completion", Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	part, err := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, 7, []byte("old receipt"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	selection := []multipartSelection{{Number: 7, ETag: part.ETag}}
	first, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, selection, "", "*")
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err = f.conn.QueryRow(f.ctx, `SELECT (to_jsonb(o)-'checksum_policy')::text FROM neon_storage.objects o WHERE key=$1`, u.Key).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = f.conn.Exec(f.ctx, `ALTER TABLE neon_storage.objects DROP COLUMN checksum_policy;ALTER TABLE neon_storage.multipart_parts DROP COLUMN checksums;ALTER TABLE neon_storage.multipart_uploads DROP COLUMN checksum_algorithm,DROP COLUMN checksum_type,DROP COLUMN result_checksum_policy;`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = f.conn.Exec(f.ctx, objectMultipartChecksumsSchema); err != nil {
			t.Fatal(err)
		}
	}
	replay, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, selection, "", "*")
	if err != nil || replay.SHA256 != first.SHA256 || replay.ChecksumPolicy != (storageChecksumPolicy{}) {
		t.Fatal("legacy receipt changed", err)
	}
	var after string
	if err = f.conn.QueryRow(f.ctx, `SELECT (to_jsonb(o)-'checksum_policy')::text FROM neon_storage.objects o WHERE key=$1`, u.Key).Scan(&after); err != nil || after != before {
		t.Fatal("old manifest changed", err)
	}
}

func TestMultipartChecksumPostgresListNegotiationAndCurrentPrincipal(t *testing.T) {
	s, f := checksumBlobFixture(t)
	u, err := s.createMultipart(f.ctx, f.conn, f.p, "checksum-actor", "settings-bucket", "list-own-negotiation", multipartCreate{Key: "list-own", Size: -1, ChecksumAlgorithm: "CRC32C"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.createMultipart(f.ctx, f.conn, f.p, "foreign", "settings-bucket", "list-foreign-negotiation", multipartCreate{Key: "list-foreign", Size: -1, ChecksumAlgorithm: "CRC64NVME"})
	if err != nil {
		t.Fatal(err)
	}
	w := multipartChecksumWire(t, s, f, "GET", "", "?uploads", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), u.ID) || strings.Contains(w.Body.String(), foreign.ID) || !strings.Contains(w.Body.String(), "<ChecksumAlgorithm>CRC32C</ChecksumAlgorithm>") || !strings.Contains(w.Body.String(), "<ChecksumType>COMPOSITE</ChecksumType>") {
		t.Fatal("list negotiation/scope", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/multipart", nil)
	s.storageMultipartList(w, r, f.conn, f.p, "checksum-actor", "settings-bucket")
	var result struct {
		Items []multipartUpload `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Items) != 1 || result.Items[0].ChecksumAlgorithm != "CRC32C" || result.Items[0].ChecksumType != "COMPOSITE" {
		t.Fatal("console list omitted negotiation", w.Code, err, w.Body.String())
	}
}

func TestMultipartChecksumPostgresCorruptLayoutFailsBeforeReadOrCopy(t *testing.T) {
	s, f := checksumBlobFixture(t)
	body := []byte("123456789")
	payload, _ := smallStoragePayload(body)
	u, err := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "layout-integrity", multipartCreate{Key: "layout-integrity", Size: -1, ChecksumAlgorithm: "SHA256"})
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	name := "X-Amz-Checksum-Sha256"
	h.Set(name, payload.checksums[name])
	part, err := s.putMultipartPart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, 1, body, "", h)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.completeMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", u.Key, u.ID, []multipartSelection{{Number: 1, ETag: part.ETag, Checksums: map[string]string{name: payload.checksums[name]}}}, "", "*")
	if err != nil {
		t.Fatal(err)
	}
	bad := first.ChecksumPolicy
	bad.Value = base64.StdEncoding.EncodeToString(make([]byte, 32)) + "-1"
	if _, err = f.conn.Exec(f.ctx, `UPDATE neon_storage.objects SET checksum_policy=$1 WHERE key=$2`, bad, u.Key); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/"+u.Key, nil)
		s.serveS3Object(w, r, f.conn, f.p, "settings-bucket", u.Key)
		if w.Code != 503 || bytes.Contains(w.Body.Bytes(), body) {
			t.Fatal("corrupt composite disclosed body", method, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/"+u.Key+"?attributes", nil)
	r.Header.Set("X-Amz-Object-Attributes", "Checksum,ObjectParts")
	s.s3ObjectAttributes(w, r, f.conn, f.p, "settings-bucket", u.Key)
	if w.Code != 503 {
		t.Fatal("corrupt composite attributes", w.Code)
	}
	if _, err = copyStorageObject(f.ctx, f.conn, f.p.ProjectID, "settings-bucket", u.Key, "settings-bucket", "bad-layout-copy", "", "", "*"); err == nil {
		t.Fatal("corrupt composite copied")
	}
	var count int
	if err = f.conn.QueryRow(f.ctx, `SELECT count(*) FROM neon_storage.objects WHERE key='bad-layout-copy'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid copy published", count, err)
	}
}
