package control

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

var s3SettingResources = []string{"acl", "policy", "tagging", "versioning", "lifecycle", "object-lock", "publicAccessBlock", "attributes"}

func s3SettingResource(q url.Values) (string, bool) {
	selected := ""
	count := 0
	for _, name := range append(append([]string{}, s3SettingResources...), "cors", "delete", "uploads", "uploadId", "partNumber", "location") {
		if q.Has(name) {
			count++
			selected = name
		}
	}
	// uploadId and partNumber form the existing multipart operation selector.
	if q.Has("uploadId") && q.Has("partNumber") {
		count--
	}
	for _, name := range s3SettingResources {
		if selected == name && count == 1 {
			return name, true
		}
	}
	return "", count <= 1
}
func s3ConfigurationGrammar(root string) (map[string][]string, map[string]bool) {
	g := map[string][]string{}
	repeat := map[string]bool{}
	switch root {
	case "Tagging":
		g[root] = []string{"TagSet"}
		g[root+"/TagSet"] = []string{"Tag"}
		g[root+"/TagSet/Tag"] = []string{"Key", "Value"}
		repeat[root+"/TagSet/Tag"] = true
	case "VersioningConfiguration":
		g[root] = []string{"Status", "MfaDelete"}
	case "PublicAccessBlockConfiguration":
		g[root] = []string{"BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"}
	case "ObjectLockConfiguration":
		g[root] = []string{"ObjectLockEnabled", "Rule"}
		g[root+"/Rule"] = []string{"DefaultRetention"}
		g[root+"/Rule/DefaultRetention"] = []string{"Mode", "Days", "Years"}
	case "LifecycleConfiguration":
		g[root] = []string{"Rule"}
		repeat[root+"/Rule"] = true
		p := root + "/Rule"
		g[p] = []string{"ID", "Status", "Prefix", "Filter", "Expiration", "Transition", "AbortIncompleteMultipartUpload"}
		repeat[p+"/Transition"] = true
		g[p+"/Filter"] = []string{"Prefix", "Tag", "And"}
		g[p+"/Filter/Tag"] = []string{"Key", "Value"}
		g[p+"/Filter/And"] = []string{"Prefix", "Tag"}
		g[p+"/Filter/And/Tag"] = []string{"Key", "Value"}
		repeat[p+"/Filter/And/Tag"] = true
		g[p+"/Expiration"] = []string{"Days", "Date", "ExpiredObjectDeleteMarker"}
		g[p+"/Transition"] = []string{"Days", "Date", "StorageClass"}
		g[p+"/AbortIncompleteMultipartUpload"] = []string{"DaysAfterInitiation"}
	}
	return g, repeat
}
func decodeStorageConfiguration(data []byte, root string, out any) bool {
	g, repeat := s3ConfigurationGrammar(root)
	return len(g) > 0 && strictStorageXML(data, root, g, repeat) && xml.Unmarshal(data, out) == nil
}
func s3ConfigurationXML(w http.ResponseWriter, root string, value any) {
	var data bytes.Buffer
	start := xml.StartElement{Name: xml.Name{Local: root}, Attr: []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: s3XMLNamespace}}}
	if e := xml.NewEncoder(&data).EncodeElement(value, start); e != nil {
		http.Error(w, "S3 configuration unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	w.Write(append([]byte(xml.Header), data.Bytes()...))
}

func (s *server) s3Settings(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket, key string, data []byte, q url.Values) bool {
	resource, valid := s3SettingResource(q)
	if !valid {
		s3Error(w, r, 400, "InvalidRequest")
		return true
	}
	if resource == "" {
		return false
	}
	if q.Has("versionId") && q.Get("versionId") != "null" {
		s3Error(w, r, 404, "NoSuchVersion")
		return true
	}
	if resource == "attributes" {
		if key == "" || r.Method != http.MethodGet {
			s3Error(w, r, 400, "InvalidRequest")
			return true
		}
		s.s3ObjectAttributes(w, r, conn, p, bucket, key)
		return true
	}
	if key != "" && resource != "acl" && resource != "tagging" {
		s3Error(w, r, 400, "InvalidRequest")
		return true
	}
	tx, e := conn.Begin(r.Context())
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return true
	}
	defer tx.Rollback(r.Context())
	if storageDirectoryLock(r.Context(), tx) != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return true
	}
	access, version, settings, e := readStorageSettings(r, tx, bucket)
	if isNoRows(e) {
		s3Error(w, r, 404, "NoSuchBucket")
		return true
	}
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return true
	}
	if key != "" {
		var exists bool
		if e = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM neon_storage.objects WHERE bucket=$1 AND key=$2)`, bucket, key).Scan(&exists); e != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return true
		}
		if !exists {
			s3Error(w, r, 404, "NoSuchKey")
			return true
		}
		w.Header().Set("X-Amz-Version-Id", "null")
	}
	if resource == "acl" || resource == "policy" {
		if r.Method != http.MethodGet {
			s3Error(w, r, 501, "NotImplemented")
			return true
		}
		if resource == "acl" {
			s3ReadOnlyACL(w, p.BranchID, access)
			return true
		}
		statements := []any{}
		if access == "public_read" {
			statements = append(statements, record{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::" + bucket + "/*"})
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		jsonResponse(w, 200, record{"Version": "2012-10-17", "Statement": statements})
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		s3Error(w, r, 405, "MethodNotAllowed")
		return true
	}
	var objectTags []storageTag
	if resource == "tagging" && key != "" {
		if e = tx.QueryRow(r.Context(), `SELECT tags FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, key).Scan(&objectTags); e != nil {
			s3Error(w, r, 503, "ServiceUnavailable")
			return true
		}
	}
	if r.Method == http.MethodGet {
		switch resource {
		case "tagging":
			tags := settings.Tags
			if key != "" {
				tags = objectTags
			}
			s3ConfigurationXML(w, "Tagging", struct {
				Tags []storageTag `xml:"TagSet>Tag"`
			}{Tags: tags})
		case "versioning":
			s3ConfigurationXML(w, "VersioningConfiguration", settings.Versioning)
		case "lifecycle":
			if settings.Lifecycle == nil {
				s3Error(w, r, 404, "NoSuchLifecycleConfiguration")
			} else {
				s3ConfigurationXML(w, "LifecycleConfiguration", *settings.Lifecycle)
			}
		case "object-lock":
			if settings.ObjectLock == nil {
				s3Error(w, r, 404, "ObjectLockConfigurationNotFoundError")
			} else {
				s3ConfigurationXML(w, "ObjectLockConfiguration", *settings.ObjectLock)
			}
		case "publicAccessBlock":
			if settings.PublicAccessBlock == nil {
				s3Error(w, r, 404, "NoSuchPublicAccessBlockConfiguration")
			} else {
				s3ConfigurationXML(w, "PublicAccessBlockConfiguration", *settings.PublicAccessBlock)
			}
		}
		return true
	}
	if resource == "versioning" && r.Method == http.MethodDelete || resource == "object-lock" && r.Method == http.MethodDelete {
		s3Error(w, r, 405, "MethodNotAllowed")
		return true
	}
	switch resource {
	case "tagging":
		tags := []storageTag{}
		if r.Method == http.MethodPut {
			var input struct {
				Tags []storageTag `xml:"TagSet>Tag"`
			}
			if !decodeStorageConfiguration(data, "Tagging", &input) {
				s3Error(w, r, 400, "MalformedXML")
				return true
			}
			limit := 50
			if key != "" {
				limit = 10
			}
			tags, e = normalizeStorageTags(input.Tags, limit)
			if e != nil {
				s3Error(w, r, 400, "InvalidTag")
				return true
			}
		}
		if key != "" {
			_, e = tx.Exec(r.Context(), `UPDATE neon_storage.objects SET tags=$3 WHERE bucket=$1 AND key=$2`, bucket, key, tags)
		} else {
			settings.Tags = tags
		}
	case "versioning":
		var v storageVersioning
		if !decodeStorageConfiguration(data, "VersioningConfiguration", &v) || v.Status == "" {
			s3Error(w, r, 400, "MalformedXML")
			return true
		}
		settings.Versioning = v
	case "lifecycle":
		settings.Lifecycle = nil
		if r.Method == http.MethodPut {
			v := &storageLifecycle{}
			if !decodeStorageConfiguration(data, "LifecycleConfiguration", v) {
				s3Error(w, r, 400, "MalformedXML")
				return true
			}
			settings.Lifecycle = v
		}
	case "object-lock":
		v := &storageObjectLock{}
		if !decodeStorageConfiguration(data, "ObjectLockConfiguration", v) {
			s3Error(w, r, 400, "MalformedXML")
			return true
		}
		settings.ObjectLock = v
	case "publicAccessBlock":
		settings.PublicAccessBlock = nil
		if r.Method == http.MethodPut {
			v := &storagePublicAccessBlock{}
			if !decodeStorageConfiguration(data, "PublicAccessBlockConfiguration", v) {
				s3Error(w, r, 400, "MalformedXML")
				return true
			}
			settings.PublicAccessBlock = v
		}
	}
	settings, e2 := normalizeStorageSettings(settings)
	if e2 != nil {
		s3Error(w, r, 400, "InvalidArgument")
		return true
	}
	if e == nil && key == "" {
		_, e = tx.Exec(r.Context(), `UPDATE neon_storage.buckets SET settings=$2,settings_version=settings_version+1 WHERE name=$1 AND settings_version=$3`, bucket, settings, version)
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		s3Error(w, r, 503, "ServiceUnavailable")
		return true
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(200)
	}
	return true
}

func s3ReadOnlyACL(w http.ResponseWriter, owner, access string) {
	type grantee struct {
		Type  string `xml:"xsi:type,attr"`
		XMLNS string `xml:"xmlns:xsi,attr"`
		ID    string `xml:"ID,omitempty"`
		URI   string `xml:"URI,omitempty"`
	}
	type grant struct {
		Grantee    grantee `xml:"Grantee"`
		Permission string  `xml:"Permission"`
	}
	v := struct {
		XMLName xml.Name      `xml:"AccessControlPolicy"`
		XMLNS   string        `xml:"xmlns,attr"`
		Owner   s3ObjectOwner `xml:"Owner"`
		Grants  []grant       `xml:"AccessControlList>Grant"`
	}{XMLNS: s3XMLNamespace, Owner: s3ObjectOwner{ID: owner, Name: owner}, Grants: []grant{{Grantee: grantee{Type: "CanonicalUser", XMLNS: "http://www.w3.org/2001/XMLSchema-instance", ID: owner}, Permission: "FULL_CONTROL"}}}
	if access == "public_read" {
		v.Grants = append(v.Grants, grant{Grantee: grantee{Type: "Group", XMLNS: "http://www.w3.org/2001/XMLSchema-instance", URI: "http://acs.amazonaws.com/groups/global/AllUsers"}, Permission: "READ"})
	}
	s3XML(w, 200, v)
}
func (s *server) s3ObjectAttributes(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, p objectStoragePayload, bucket, key string) {
	requested := map[string]bool{}
	for _, v := range strings.Split(r.Header.Get("X-Amz-Object-Attributes"), ",") {
		v = strings.TrimSpace(v)
		switch v {
		case "ETag", "ObjectSize", "Checksum", "StorageClass":
			if requested[v] {
				s3Error(w, r, 400, "InvalidArgument")
				return
			}
			requested[v] = true
		case "ObjectParts":
			s3Error(w, r, 501, "NotImplemented")
			return
		default:
			s3Error(w, r, 400, "InvalidArgument")
			return
		}
	}
	var blob, digest string
	var size int64
	e := conn.QueryRow(r.Context(), `SELECT blob_key,sha256,size FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, key).Scan(&blob, &digest, &size)
	if isNoRows(e) {
		s3Error(w, r, 404, "NoSuchKey")
		return
	}
	if e != nil || !strings.HasPrefix(blob, p.ProjectID+"/") {
		s3Error(w, r, 503, "ServiceUnavailable")
		return
	}
	type checksum struct {
		SHA256 string `xml:"ChecksumSHA256"`
		Type   string `xml:"ChecksumType"`
	}
	v := struct {
		XMLName      xml.Name  `xml:"GetObjectAttributesOutput"`
		XMLNS        string    `xml:"xmlns,attr"`
		ETag         *string   `xml:"ETag,omitempty"`
		Size         *int64    `xml:"ObjectSize,omitempty"`
		Checksum     *checksum `xml:"Checksum,omitempty"`
		StorageClass *string   `xml:"StorageClass,omitempty"`
	}{XMLNS: s3XMLNamespace}
	if requested["ETag"] {
		v.ETag = &digest
	}
	if requested["ObjectSize"] {
		v.Size = &size
	}
	if requested["StorageClass"] {
		name := "STANDARD"
		v.StorageClass = &name
	}
	if requested["Checksum"] {
		raw, e := hex.DecodeString(digest)
		if e != nil || len(raw) != 32 {
			s3Error(w, r, 503, "ServiceUnavailable")
			return
		}
		v.Checksum = &checksum{SHA256: base64.StdEncoding.EncodeToString(raw), Type: "FULL_OBJECT"}
	}
	w.Header().Set("X-Amz-Version-Id", "null")
	s3XML(w, 200, v)
}

// A failed or malformed authenticated request can never fall back to anonymous
// access. Anonymous paths contain an exact branch, bucket and object key only.
func (s *server) s3AnonymousRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return false
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return false
	}
	for name := range q {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-") {
			return false
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	pieces := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/s3/"), "/", 3)
	if !strings.HasPrefix(r.URL.Path, "/s3/") || len(pieces) != 3 || !dataBranchID.MatchString(pieces[0]) || !storageBucketName.MatchString(pieces[1]) || !validStorageKey(pieces[2]) {
		s3Error(w, r, 403, "AccessDenied")
		return true
	}
	for name, values := range q {
		if name != "versionId" || len(values) != 1 || values[0] != "null" {
			s3Error(w, r, 403, "AccessDenied")
			return true
		}
	}
	conn, p, close, e := s.openObjectStorage(r, "", pieces[0])
	if e != nil {
		s3Error(w, r, 403, "AccessDenied")
		return true
	}
	defer close()
	var access string
	var settings storageBucketSettings
	e = conn.QueryRow(r.Context(), `SELECT access,settings FROM neon_storage.buckets WHERE name=$1`, pieces[1]).Scan(&access, &settings)
	if e != nil || !storageAnonymousAllowed(access, settings) {
		s3Error(w, r, 403, "AccessDenied")
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		rules, e := loadStorageCORS(r, conn, pieces[1])
		if e == nil {
			if rule := matchStorageCORS(rules, origin, r.Method, nil); rule != nil {
				applyStorageCORS(w, rule, origin, nil, false)
			}
		}
	}
	s.serveS3Object(w, r, conn, p, pieces[1], pieces[2])
	return true
}
