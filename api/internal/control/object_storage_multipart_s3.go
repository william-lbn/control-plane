package control

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Strict multipart subresource validation prevents a typo or conflicting query
// from falling through to ordinary PutObject (or silently losing a checksum).
func validMultipartQuery(q url.Values) bool {
	for k, v := range q {
		if len(v) != 1 {
			return false
		}
		if strings.HasPrefix(k, "X-Amz-") {
			continue
		}
		switch k {
		case "x-id", "uploads", "uploadId", "partNumber", "part-number-marker", "max-parts", "key-marker", "upload-id-marker", "max-uploads", "prefix", "delimiter", "encoding-type":
		default:
			return false
		}
	}
	if q.Has("uploads") {
		if q.Get("uploads") != "" || q.Has("uploadId") || q.Has("partNumber") || q.Has("max-parts") || q.Has("part-number-marker") {
			return false
		}
	} else if q.Get("uploadId") == "" || q.Has("max-uploads") || q.Has("key-marker") || q.Has("upload-id-marker") || q.Has("prefix") || q.Has("delimiter") || q.Has("encoding-type") {
		return false
	}
	return q.Get("encoding-type") == "" || q.Get("encoding-type") == "url"
}
func multipartNumber(q url.Values, name string, def, min, max int) (int, error) {
	if !q.Has(name) {
		return def, nil
	}
	n, e := strconv.Atoi(q.Get(name))
	if e != nil || n < min || n > max {
		return 0, multipartFault(400, "InvalidArgument")
	}
	return n, nil
}
func (s *server) s3Multipart(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, principal, bucket, key string, data []byte, q url.Values) {
	if rejectUnsupportedS3Headers(w, r) {
		return
	}
	if !validMultipartQuery(q) || (key != "" && !validStorageKey(key)) {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	operation := ""
	if r.Method == http.MethodPost {
		if q.Has("uploads") && key != "" {
			operation = "create"
		} else if q.Has("uploadId") {
			operation = "complete"
		}
	} else if r.Method == http.MethodPut && q.Has("partNumber") {
		operation = "part"
	}
	if e := multipartChecksumHeaders(r.Header, operation); e != nil {
		multipartError(w, r, e, true)
		return
	}
	for k := range r.Header {
		v := strings.ToLower(k)
		if strings.HasPrefix(v, "x-amz-server-side-") || (v == "x-amz-acl" || v == "x-amz-storage-class") && !(q.Has("uploads") && r.Method == http.MethodPost && key != "") || (v == "x-amz-tagging" && !(q.Has("uploads") && r.Method == http.MethodPost && key != "")) || strings.HasPrefix(v, "x-amz-object-lock-") || v == "x-amz-expected-bucket-owner" || v == "x-amz-request-payer" {
			s3Error(w, r, 501, "NotImplemented")
			return
		}
	}
	if q.Has("uploads") {
		if key == "" && r.Method == http.MethodGet && len(data) == 0 {
			s.s3ListMultipart(w, r, conn, p, principal, bucket, q)
			return
		}
		if key == "" || r.Method != http.MethodPost || len(data) != 0 || len(qWithoutSignature(q)) != 1 {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		metadata, e := metadataFromHeaders(r, true)
		if e != nil {
			multipartError(w, r, multipartFault(400, "InvalidArgument"), true)
			return
		}
		tags, err := storageTagsHeader(r)
		if err != nil {
			s3Error(w, r, 400, "InvalidTag")
			return
		}
		headers, err := storageResponseHeaders(r)
		if err != nil {
			s3Error(w, r, 400, "InvalidArgument")
			return
		}
		u, e := s.createMultipart(r.Context(), conn, p, principal, bucket, "", multipartCreate{Key: key, ContentType: r.Header.Get("Content-Type"), Metadata: metadata, Size: -1, Tags: tags, ResponseHeaders: headers, ChecksumAlgorithm: r.Header.Get("X-Amz-Checksum-Algorithm"), ChecksumType: r.Header.Get("X-Amz-Checksum-Type")})
		if e != nil {
			multipartError(w, r, e, true)
			return
		}
		if u.ChecksumAlgorithm != "" {
			w.Header().Set("X-Amz-Checksum-Algorithm", u.ChecksumAlgorithm)
			w.Header().Set("X-Amz-Checksum-Type", u.ChecksumType)
		}
		s3XML(w, 200, struct {
			XMLName xml.Name `xml:"InitiateMultipartUploadResult"`
			XMLNS   string   `xml:"xmlns,attr"`
			Bucket  string   `xml:"Bucket"`
			Key     string   `xml:"Key"`
			ID      string   `xml:"UploadId"`
		}{XMLNS: s3XMLNamespace, Bucket: bucket, Key: key, ID: u.ID})
		return
	}
	if key == "" {
		s3Error(w, r, 400, "InvalidRequest")
		return
	}
	id := q.Get("uploadId")
	if q.Has("partNumber") {
		if r.Method != http.MethodPut || len(qWithoutSignature(q)) != 2 {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		n, e := multipartNumber(q, "partNumber", 0, 1, 10000)
		if e != nil {
			multipartError(w, r, e, true)
			return
		}
		part, e := s.putMultipartPart(r.Context(), conn, p, principal, bucket, key, id, n, data, r.Header.Get("Content-MD5"), r.Header)
		if e != nil {
			multipartError(w, r, e, true)
			return
		}
		w.Header().Set("ETag", "\""+part.ETag+"\"")
		// Only the negotiated algorithm is returned, including for UploadPartCopy.
		selected := storagePartChecksumXML(map[string]string{storageChecksumAlgorithms[part.algorithm]: part.Checksums[storageChecksumAlgorithms[part.algorithm]]})
		if name := storageChecksumAlgorithms[part.algorithm]; name != "" {
			w.Header().Set(name, part.Checksums[name])
		}
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			s3XML(w, 200, struct {
				storageAttributeChecksum
				XMLName  xml.Name `xml:"CopyPartResult"`
				XMLNS    string   `xml:"xmlns,attr"`
				ETag     string   `xml:"ETag"`
				Modified string   `xml:"LastModified"`
			}{storageAttributeChecksum: selected, XMLNS: s3XMLNamespace, ETag: "\"" + part.ETag + "\"", Modified: part.Modified.UTC().Format(time.RFC3339Nano)})
		} else {
			w.WriteHeader(200)
		}
		return
	}
	switch r.Method {
	case http.MethodPost:
		if len(qWithoutSignature(q)) != 1 {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		parts, e := parseMultipartCompletion(data)
		if e != nil {
			s3Error(w, r, 400, "MalformedXML")
			return
		}
		opt, e := multipartCompletionHeaders(r.Header)
		if e != nil {
			multipartError(w, r, e, true)
			return
		}
		u, e := s.completeMultipart(r.Context(), conn, p, principal, bucket, key, id, parts, r.Header.Get("If-Match"), r.Header.Get("If-None-Match"), opt)
		if e != nil {
			multipartError(w, r, e, true)
			return
		}
		w.Header().Set("ETag", "\""+u.SHA256+"\"")
		checksum := storageAttributeChecksum{}
		if u.ChecksumPolicy != (storageChecksumPolicy{}) {
			checksum = storagePartChecksumXML(map[string]string{storageChecksumAlgorithms[u.ChecksumAlgorithm]: u.ChecksumPolicy.Value})
			checksum.Type = u.ChecksumType
			w.Header().Set("X-Amz-Checksum-Type", u.ChecksumType)
		}
		s3XML(w, 200, struct {
			storageAttributeChecksum
			XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
			XMLNS    string   `xml:"xmlns,attr"`
			Location string   `xml:"Location"`
			Bucket   string   `xml:"Bucket"`
			Key      string   `xml:"Key"`
			ETag     string   `xml:"ETag"`
		}{storageAttributeChecksum: checksum, XMLNS: s3XMLNamespace, Location: multipartObjectLocation(p.BranchID, bucket, key), Bucket: bucket, Key: key, ETag: "\"" + u.SHA256 + "\""})
	case http.MethodDelete:
		if len(data) != 0 || len(qWithoutSignature(q)) != 1 {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		if e := abortMultipart(r.Context(), conn, p, principal, bucket, key, id); e != nil {
			multipartError(w, r, e, true)
			return
		}
		w.WriteHeader(204)
	case http.MethodGet:
		if len(data) != 0 {
			s3Error(w, r, 400, "InvalidRequest")
			return
		}
		s.s3ListMultipartParts(w, r, conn, p, principal, bucket, key, id, q)
	default:
		s3Error(w, r, 405, "MethodNotAllowed")
	}
}
func qWithoutSignature(q url.Values) url.Values {
	v := url.Values{}
	for k, x := range q {
		if !strings.HasPrefix(k, "X-Amz-") && k != "x-id" {
			v[k] = x
		}
	}
	return v
}

// Reject duplicate fields, foreign namespaces, directives and unknown checksum
// elements. Encoding/xml's default unmarshal silently ignores these elements.
func parseMultipartCompletion(data []byte) ([]multipartSelection, error) {
	bad := errors.New("invalid multipart XML")
	if len(data) == 0 || len(data) > 256<<10 {
		return nil, bad
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	root := false
	field := ""
	value := ""
	seen := map[string]bool{}
	parts := []multipartSelection{}
	var part multipartSelection
	for {
		token, e := d.Token()
		if e == io.EOF {
			if root && depth == 0 && len(parts) > 0 {
				return parts, nil
			}
			return nil, bad
		}
		if e != nil {
			return nil, bad
		}
		switch v := token.(type) {
		case xml.StartElement:
			depth++
			if v.Name.Space != "" && v.Name.Space != s3XMLNamespace {
				return nil, bad
			}
			for _, a := range v.Attr {
				if a.Name.Local != "xmlns" && a.Name.Space != "xmlns" {
					return nil, bad
				}
			}
			if depth == 1 {
				if root || v.Name.Local != "CompleteMultipartUpload" {
					return nil, bad
				}
				root = true
			} else if depth == 2 {
				if v.Name.Local != "Part" || len(parts) >= multipartPartLimit {
					return nil, bad
				}
				part = multipartSelection{}
				seen = map[string]bool{}
			} else if depth == 3 {
				if (v.Name.Local != "PartNumber" && v.Name.Local != "ETag" && (!strings.HasPrefix(v.Name.Local, "Checksum") || storageChecksumAlgorithms[strings.TrimPrefix(v.Name.Local, "Checksum")] == "")) || seen[v.Name.Local] {
					return nil, bad
				}
				field = v.Name.Local
				seen[field] = true
				value = ""
			} else {
				return nil, bad
			}
		case xml.CharData:
			if depth == 3 {
				value += string(v)
			} else if strings.TrimSpace(string(v)) != "" {
				return nil, bad
			}
		case xml.EndElement:
			if depth == 3 {
				if field == "ETag" {
					part.ETag = strings.TrimSpace(value)
				} else if field == "PartNumber" {
					part.Number, e = strconv.Atoi(strings.TrimSpace(value))
					if e != nil {
						return nil, bad
					}
				} else {
					if part.Checksums == nil {
						part.Checksums = map[string]string{}
					}
					name := storageChecksumAlgorithms[strings.TrimPrefix(field, "Checksum")]
					value = strings.TrimSpace(value)
					raw, err := base64.StdEncoding.Strict().DecodeString(value)
					if err != nil || len(raw) != storageChecksumSizes[name] || base64.StdEncoding.EncodeToString(raw) != value {
						return nil, bad
					}
					part.Checksums[name] = value
				}
			}
			if depth == 2 {
				if !seen["PartNumber"] || !seen["ETag"] {
					return nil, bad
				}
				parts = append(parts, part)
			}
			depth--
		case xml.Directive:
			return nil, bad
		}
	}
}
func (s *server) s3ListMultipartParts(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, principal, bucket, key, id string, q url.Values) {
	marker, e := multipartNumber(q, "part-number-marker", 0, 0, 10000)
	if e != nil {
		multipartError(w, r, e, true)
		return
	}
	max, e := multipartNumber(q, "max-parts", 1000, 0, 1000)
	if e != nil {
		multipartError(w, r, e, true)
		return
	}
	tx, e := conn.Begin(r.Context())
	if e != nil {
		multipartError(w, r, e, true)
		return
	}
	defer tx.Rollback(r.Context())
	u, e := loadMultipart(r.Context(), tx, p, principal, bucket, key, id)
	if e != nil {
		multipartError(w, r, e, true)
		return
	}
	if u.State != "uploading" {
		s3Error(w, r, 404, "NoSuchUpload")
		return
	}
	items, e := listMultipartParts(r.Context(), tx, id)
	if e != nil {
		multipartError(w, r, e, true)
		return
	}
	selected := []multipartWirePart{}
	truncated := false
	next := 0
	for _, v := range items {
		if v.Number <= marker {
			continue
		}
		if len(selected) >= max {
			truncated = true
			break
		}
		v.ETag = "\"" + v.ETag + "\""
		selected = append(selected, multipartWirePart{Number: v.Number, ETag: v.ETag, Size: v.Size, Modified: v.Modified, storageAttributeChecksum: storagePartChecksumXML(map[string]string{storageChecksumAlgorithms[u.ChecksumAlgorithm]: v.Checksums[storageChecksumAlgorithms[u.ChecksumAlgorithm]]})})
		next = v.Number
	}
	owner := s3ObjectOwner{ID: principal, Name: principal}
	s3XML(w, 200, struct {
		XMLName   xml.Name            `xml:"ListPartsResult"`
		XMLNS     string              `xml:"xmlns,attr"`
		Bucket    string              `xml:"Bucket"`
		Key       string              `xml:"Key"`
		ID        string              `xml:"UploadId"`
		Marker    int                 `xml:"PartNumberMarker"`
		Next      int                 `xml:"NextPartNumberMarker"`
		Max       int                 `xml:"MaxParts"`
		Truncated bool                `xml:"IsTruncated"`
		Owner     s3ObjectOwner       `xml:"Owner"`
		Initiator s3ObjectOwner       `xml:"Initiator"`
		Storage   string              `xml:"StorageClass"`
		Algorithm string              `xml:"ChecksumAlgorithm,omitempty"`
		Type      string              `xml:"ChecksumType,omitempty"`
		Parts     []multipartWirePart `xml:"Part"`
	}{XMLNS: s3XMLNamespace, Bucket: bucket, Key: key, ID: id, Marker: marker, Next: next, Max: max, Truncated: truncated, Owner: owner, Initiator: owner, Storage: "STANDARD", Algorithm: u.ChecksumAlgorithm, Type: u.ChecksumType, Parts: selected})
}
func (s *server) s3ListMultipart(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, principal, bucket string, q url.Values) {
	max, e := multipartNumber(q, "max-uploads", 1000, 1, 1000)
	if e != nil {
		multipartError(w, r, e, true)
		return
	}
	var exists bool
	if e = conn.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.buckets WHERE name=$1)`, bucket).Scan(&exists); e != nil {
		multipartError(w, r, e, true)
		return
	}
	if !exists {
		s3Error(w, r, 404, "NoSuchBucket")
		return
	}
	marker, idmarker, prefix := q.Get("key-marker"), q.Get("upload-id-marker"), q.Get("prefix")
	delimiter := q.Get("delimiter")
	if !validMultipartFilter(marker) || !validMultipartFilter(prefix) || !validMultipartFilter(delimiter) || (idmarker != "" && (!multipartID.MatchString(idmarker) || marker == "")) {
		s3Error(w, r, 400, "InvalidArgument")
		return
	}
	// Upload IDs are opaque. Resolve a returned marker against the retained
	// initiation receipt, including completed/aborted ones, so a concurrent
	// completion between pages does not skip another upload with the same key.
	var markerTime time.Time
	if idmarker != "" {
		e = conn.QueryRow(r.Context(), `SELECT created_at FROM neon_storage.multipart_uploads WHERE id=$1 AND branch_id=$2 AND principal=$3 AND bucket=$4 AND key=$5`, idmarker, p.BranchID, principal, bucket, marker).Scan(&markerTime)
		if isNoRows(e) {
			s3Error(w, r, 400, "InvalidUploadMarker")
			return
		}
		if e != nil {
			multipartError(w, r, e, true)
			return
		}
	}
	rows, e := conn.Query(r.Context(), `SELECT id,key,created_at,checksum_algorithm,checksum_type FROM neon_storage.multipart_uploads WHERE branch_id=$1 AND principal=$2 AND bucket=$3 AND state='uploading' AND starts_with(key,$4) ORDER BY key COLLATE "C",created_at,id LIMIT $5`, p.BranchID, principal, bucket, prefix, multipartActiveLimit+1)
	if e != nil {
		multipartError(w, r, e, true)
		return
	}
	defer rows.Close()
	type upload struct {
		Key       string        `xml:"Key"`
		ID        string        `xml:"UploadId"`
		Created   string        `xml:"Initiated"`
		Owner     s3ObjectOwner `xml:"Owner"`
		Initiator s3ObjectOwner `xml:"Initiator"`
		Storage   string        `xml:"StorageClass"`
		Algorithm string        `xml:"ChecksumAlgorithm,omitempty"`
		Type      string        `xml:"ChecksumType,omitempty"`
	}
	items := []upload{}
	prefixes := []struct {
		Prefix string `xml:"Prefix"`
	}{}
	groups := map[string]bool{}
	count := 0
	truncated := false
	nextKey, nextID := "", ""
	owner := s3ObjectOwner{ID: principal, Name: principal}
	encode := func(v string) string {
		if q.Get("encoding-type") == "url" {
			return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
		}
		return v
	}
	for rows.Next() {
		var id, key, algorithm, kind string
		var created time.Time
		if e = rows.Scan(&id, &key, &created, &algorithm, &kind); e != nil {
			multipartError(w, r, e, true)
			return
		}
		group := ""
		if delimiter != "" {
			if index := strings.Index(strings.TrimPrefix(key, prefix), delimiter); index >= 0 {
				group = prefix + strings.TrimPrefix(key, prefix)[:index+len(delimiter)]
			}
		}
		if group != "" {
			if groups[group] || group <= marker {
				continue
			}
			groups[group] = true
		} else if key < marker || (key == marker && (idmarker == "" || created.Before(markerTime) || (created.Equal(markerTime) && id <= idmarker))) {
			continue
		}
		if count >= max {
			truncated = true
			break
		}
		count++
		if group != "" {
			prefixes = append(prefixes, struct {
				Prefix string `xml:"Prefix"`
			}{encode(group)})
			nextKey, nextID = group, ""
		} else {
			items = append(items, upload{encode(key), id, created.UTC().Format(time.RFC3339Nano), owner, owner, "STANDARD", algorithm, kind})
			nextKey, nextID = key, id
		}
	}
	if e = rows.Err(); e != nil {
		multipartError(w, r, e, true)
		return
	}
	s3XML(w, 200, struct {
		XMLName   xml.Name `xml:"ListMultipartUploadsResult"`
		XMLNS     string   `xml:"xmlns,attr"`
		Bucket    string   `xml:"Bucket"`
		Marker    string   `xml:"KeyMarker"`
		IDMarker  string   `xml:"UploadIdMarker"`
		NextKey   string   `xml:"NextKeyMarker"`
		NextID    string   `xml:"NextUploadIdMarker"`
		Prefix    string   `xml:"Prefix"`
		Delimiter string   `xml:"Delimiter,omitempty"`
		Max       int      `xml:"MaxUploads"`
		Encoding  string   `xml:"EncodingType,omitempty"`
		Truncated bool     `xml:"IsTruncated"`
		Uploads   []upload `xml:"Upload"`
		Prefixes  []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
	}{XMLNS: s3XMLNamespace, Bucket: bucket, Marker: encode(marker), IDMarker: idmarker, NextKey: encode(nextKey), NextID: nextID, Prefix: encode(prefix), Delimiter: encode(delimiter), Max: max, Encoding: q.Get("encoding-type"), Truncated: truncated, Uploads: items, Prefixes: prefixes})
}
func validMultipartFilter(value string) bool {
	if len(value) > 1024 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func multipartObjectLocation(branch, bucket, key string) string {
	u, _ := url.Parse(s3Origin())
	if u == nil {
		u = &url.URL{}
	}
	u.Path = "/s3/" + branch + "/" + bucket + "/" + key
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
