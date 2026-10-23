package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type storageTag struct {
	Key   string `json:"key" xml:"Key"`
	Value string `json:"value" xml:"Value"`
}
type storagePublicAccessBlock struct {
	BlockPublicACLs       bool `json:"block_public_acls" xml:"BlockPublicAcls"`
	IgnorePublicACLs      bool `json:"ignore_public_acls" xml:"IgnorePublicAcls"`
	BlockPublicPolicy     bool `json:"block_public_policy" xml:"BlockPublicPolicy"`
	RestrictPublicBuckets bool `json:"restrict_public_buckets" xml:"RestrictPublicBuckets"`
}
type storageVersioning struct {
	Status    string `json:"status,omitempty" xml:"Status,omitempty"`
	MFADelete string `json:"mfa_delete,omitempty" xml:"MfaDelete,omitempty"`
}
type storageLifecycleFilter struct {
	Prefix *string              `json:"prefix,omitempty" xml:"Prefix,omitempty"`
	Tag    *storageTag          `json:"tag,omitempty" xml:"Tag,omitempty"`
	And    *storageLifecycleAnd `json:"and,omitempty" xml:"And,omitempty"`
}
type storageLifecycleAnd struct {
	Prefix *string      `json:"prefix,omitempty" xml:"Prefix,omitempty"`
	Tags   []storageTag `json:"tags,omitempty" xml:"Tag"`
}
type storageExpiration struct {
	Days                      *int    `json:"days,omitempty" xml:"Days,omitempty"`
	Date                      *string `json:"date,omitempty" xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker *bool   `json:"expired_object_delete_marker,omitempty" xml:"ExpiredObjectDeleteMarker,omitempty"`
}
type storageTransition struct {
	Days         *int    `json:"days,omitempty" xml:"Days,omitempty"`
	Date         *string `json:"date,omitempty" xml:"Date,omitempty"`
	StorageClass string  `json:"storage_class" xml:"StorageClass"`
}
type storageLifecycleRule struct {
	ID                             string                  `json:"id,omitempty" xml:"ID,omitempty"`
	Status                         string                  `json:"status" xml:"Status"`
	Prefix                         *string                 `json:"prefix,omitempty" xml:"Prefix,omitempty"`
	Filter                         *storageLifecycleFilter `json:"filter,omitempty" xml:"Filter,omitempty"`
	Expiration                     *storageExpiration      `json:"expiration,omitempty" xml:"Expiration,omitempty"`
	Transitions                    []storageTransition     `json:"transitions,omitempty" xml:"Transition"`
	AbortIncompleteMultipartUpload *struct {
		Days int `json:"days_after_initiation" xml:"DaysAfterInitiation"`
	} `json:"abort_incomplete_multipart_upload,omitempty" xml:"AbortIncompleteMultipartUpload,omitempty"`
}
type storageLifecycle struct {
	Rules []storageLifecycleRule `json:"rules" xml:"Rule"`
}
type storageRetention struct {
	Mode  string `json:"mode" xml:"Mode"`
	Days  *int   `json:"days,omitempty" xml:"Days,omitempty"`
	Years *int   `json:"years,omitempty" xml:"Years,omitempty"`
}
type storageObjectLock struct {
	Enabled string `json:"object_lock_enabled" xml:"ObjectLockEnabled"`
	Rule    *struct {
		DefaultRetention storageRetention `json:"default_retention" xml:"DefaultRetention"`
	} `json:"rule,omitempty" xml:"Rule,omitempty"`
}
type storageBucketSettings struct {
	Tags              []storageTag              `json:"tags"`
	PublicAccessBlock *storagePublicAccessBlock `json:"public_access_block"`
	Versioning        storageVersioning         `json:"versioning"`
	Lifecycle         *storageLifecycle         `json:"lifecycle"`
	ObjectLock        *storageObjectLock        `json:"object_lock"`
}

func (v storageBucketSettings) publicBlock() storagePublicAccessBlock {
	if v.PublicAccessBlock != nil {
		return *v.PublicAccessBlock
	}
	return storagePublicAccessBlock{}
}

func cleanStorageString(s string, limit int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > limit {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func normalizeStorageTags(tags []storageTag, limit int) ([]storageTag, error) {
	if len(tags) > limit {
		return nil, fmt.Errorf("tag capacity exceeded")
	}
	result := append([]storageTag{}, tags...)
	seen := map[string]bool{}
	for _, t := range result {
		if t.Key == "" || !cleanStorageString(t.Key, 128) || !cleanStorageString(t.Value, 256) || strings.HasPrefix(strings.ToLower(t.Key), "aws:") || seen[t.Key] {
			return nil, fmt.Errorf("invalid tags")
		}
		seen[t.Key] = true
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}
func storageUTCOffset(t time.Time) int { _, offset := t.Zone(); return offset }
func storageDateValid(value *string) bool {
	if value == nil {
		return false
	}
	t, e := time.Parse(time.RFC3339, *value)
	return e == nil && t.Year() >= 2000 && t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 && storageUTCOffset(t) == 0
}
func normalizeStorageSettings(v storageBucketSettings) (storageBucketSettings, error) {
	var e error
	v.Tags, e = normalizeStorageTags(v.Tags, 50)
	if e != nil {
		return v, e
	}
	if v.Versioning.Status != "" && v.Versioning.Status != "Enabled" && v.Versioning.Status != "Suspended" || v.Versioning.MFADelete != "" && v.Versioning.MFADelete != "Disabled" {
		return v, fmt.Errorf("unsupported versioning configuration")
	}
	if v.Lifecycle != nil {
		if len(v.Lifecycle.Rules) < 1 || len(v.Lifecycle.Rules) > 1000 {
			return v, fmt.Errorf("invalid lifecycle rule count")
		}
		seen := map[string]bool{}
		for _, r := range v.Lifecycle.Rules {
			if !cleanStorageString(r.ID, 255) || r.ID != "" && seen[r.ID] || r.Status != "Enabled" && r.Status != "Disabled" || r.Prefix != nil && (!cleanStorageString(*r.Prefix, 1024) || r.Filter != nil) {
				return v, fmt.Errorf("invalid lifecycle rule")
			}
			if r.ID != "" {
				seen[r.ID] = true
			}
			if r.Filter != nil {
				count := 0
				if r.Filter.Prefix != nil {
					count++
					if !cleanStorageString(*r.Filter.Prefix, 1024) {
						return v, fmt.Errorf("invalid filter prefix")
					}
				}
				if r.Filter.Tag != nil {
					count++
					if _, e = normalizeStorageTags([]storageTag{*r.Filter.Tag}, 1); e != nil {
						return v, e
					}
				}
				if r.Filter.And != nil {
					count++
					a := r.Filter.And
					if a.Prefix != nil && !cleanStorageString(*a.Prefix, 1024) {
						return v, fmt.Errorf("invalid filter prefix")
					}
					if _, e = normalizeStorageTags(a.Tags, 50); e != nil || len(a.Tags)+boolInt(a.Prefix != nil) < 2 {
						return v, fmt.Errorf("invalid conjunctive filter")
					}
				}
				if count > 1 {
					return v, fmt.Errorf("ambiguous lifecycle filter")
				}
			}
			if r.Expiration != nil {
				x := r.Expiration
				n := boolInt(x.Days != nil) + boolInt(x.Date != nil) + boolInt(x.ExpiredObjectDeleteMarker != nil)
				if n != 1 || x.Days != nil && (*x.Days < 1 || *x.Days > 36500) || x.Date != nil && !storageDateValid(x.Date) {
					return v, fmt.Errorf("invalid expiration")
				}
			}
			if len(r.Transitions) > 10 {
				return v, fmt.Errorf("transition capacity exceeded")
			}
			for _, x := range r.Transitions {
				if boolInt(x.Days != nil)+boolInt(x.Date != nil) != 1 || x.Days != nil && (*x.Days < 0 || *x.Days > 36500) || x.Date != nil && !storageDateValid(x.Date) {
					return v, fmt.Errorf("invalid transition")
				}
				switch x.StorageClass {
				case "STANDARD_IA", "ONEZONE_IA", "INTELLIGENT_TIERING", "GLACIER", "DEEP_ARCHIVE", "GLACIER_IR":
				default:
					return v, fmt.Errorf("invalid storage class")
				}
			}
			if r.AbortIncompleteMultipartUpload != nil && (r.AbortIncompleteMultipartUpload.Days < 1 || r.AbortIncompleteMultipartUpload.Days > 36500) {
				return v, fmt.Errorf("invalid incomplete upload rule")
			}
			if r.Expiration == nil && len(r.Transitions) == 0 && r.AbortIncompleteMultipartUpload == nil {
				return v, fmt.Errorf("empty lifecycle action")
			}
		}
	}
	if v.ObjectLock != nil {
		if v.ObjectLock.Enabled != "Enabled" {
			return v, fmt.Errorf("invalid object lock flag")
		}
		if v.ObjectLock.Rule != nil {
			r := v.ObjectLock.Rule.DefaultRetention
			if r.Mode != "GOVERNANCE" && r.Mode != "COMPLIANCE" || boolInt(r.Days != nil)+boolInt(r.Years != nil) != 1 || r.Days != nil && (*r.Days < 1 || *r.Days > 36500) || r.Years != nil && (*r.Years < 1 || *r.Years > 100) {
				return v, fmt.Errorf("invalid default retention")
			}
		}
	}
	raw, _ := json.Marshal(v)
	if len(raw) > 65536 {
		return v, fmt.Errorf("settings capacity exceeded")
	}
	return v, nil
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func storageSettingsETag(branch, bucket, access string, version int64, v storageBucketSettings) string {
	raw, _ := json.Marshal([]any{"neon-storage-settings-v1", branch, bucket, access, version, v})
	return fmt.Sprintf(`"%x"`, sha256.Sum256(raw))
}
func storageAnonymousAllowed(access string, v storageBucketSettings) bool {
	return access == "public_read" && !v.publicBlock().IgnorePublicACLs && !v.publicBlock().RestrictPublicBuckets
}

// Leaf-only typed XML parsing follows a closed path grammar. It rejects unknown
// fields, duplicates, foreign namespaces, directives and multiple documents.
func strictStorageXML(data []byte, root string, grammar map[string][]string, repeated map[string]bool) bool {
	if len(data) == 0 || len(data) > 65536 {
		return false
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	stack := []string{}
	counts := []map[string]int{}
	started, ended := false, false
	nodes := 0
	for {
		token, e := d.Token()
		if e == io.EOF {
			return started && ended && len(stack) == 0
		}
		if e != nil {
			return false
		}
		switch t := token.(type) {
		case xml.StartElement:
			nodes++
			if nodes > 10000 || len(stack) > 8 || t.Name.Space != "" && t.Name.Space != s3XMLNamespace {
				return false
			}
			for _, a := range t.Attr {
				if a.Name.Local != "xmlns" && a.Name.Space != "xmlns" {
					return false
				}
			}
			if len(stack) == 0 {
				if started || t.Name.Local != root {
					return false
				}
				started = true
			} else {
				path := strings.Join(stack, "/")
				allowed := false
				for _, v := range grammar[path] {
					allowed = allowed || v == t.Name.Local
				}
				if !allowed {
					return false
				}
				counts[len(counts)-1][t.Name.Local]++
				if counts[len(counts)-1][t.Name.Local] > 1 && !repeated[path+"/"+t.Name.Local] {
					return false
				}
			}
			stack = append(stack, t.Name.Local)
			counts = append(counts, map[string]int{})
		case xml.EndElement:
			if len(stack) == 0 {
				return false
			}
			path := strings.Join(stack, "/")
			if strings.HasSuffix(path, "/Tag") && (counts[len(counts)-1]["Key"] != 1 || counts[len(counts)-1]["Value"] != 1) {
				return false
			}
			if path == "Tagging" && counts[len(counts)-1]["TagSet"] != 1 {
				return false
			}
			stack = stack[:len(stack)-1]
			counts = counts[:len(counts)-1]
			if len(stack) == 0 {
				ended = true
			}
		case xml.CharData:
			if (len(stack) == 0 || len(grammar[strings.Join(stack, "/")]) > 0) && strings.TrimSpace(string(t)) != "" {
				return false
			}
		case xml.Directive:
			return false
		case xml.ProcInst:
			if t.Target != "xml" || started {
				return false
			}
		}
	}
}

func readStorageSettings(r *http.Request, tx pgx.Tx, bucket string) (string, int64, storageBucketSettings, error) {
	var access string
	var version int64
	var v storageBucketSettings
	e := tx.QueryRow(r.Context(), `SELECT access,settings_version,settings FROM neon_storage.buckets WHERE name=$1`, bucket).Scan(&access, &version, &v)
	if e == nil {
		v, e = normalizeStorageSettings(v)
	}
	return access, version, v, e
}
func storageSettingsReceipt(branch, bucket, access string, version int64, v storageBucketSettings) record {
	return record{"branch_id": branch, "bucket": bucket, "access": access, "version": version, "settings": v, "lifecycle_rules_enforced": false, "object_versioning_enforced": false, "object_lock_retention_enforced": false}
}

func (s *server) storageBucketSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	bucket := r.PathValue("bucket")
	if !storageBucketName.MatchString(bucket) {
		fail(w, r, 422, "invalid_bucket", "Valid bucket required")
		return
	}
	var input storageBucketSettingsInput
	var key, requestHash string
	if r.Method != http.MethodGet {
		if len(s.idempotencyKey) < 32 {
			fail(w, r, 503, "storage_configuration_unavailable", "Storage settings key is unavailable")
			return
		}
		var ok bool
		key, ok = creationKey(w, r)
		if !ok {
			return
		}
		if readJSON(r, &input) != nil || input.Settings == nil || input.Access != "private" && input.Access != "public_read" {
			fail(w, r, 422, "invalid_storage_settings", "A complete typed settings document and access are required")
			return
		}
		var e error
		normalized, normalizeErr := normalizeStorageSettings(*input.Settings)
		e = normalizeErr
		input.Settings = &normalized
		if e != nil {
			fail(w, r, 422, "invalid_storage_settings", "Storage configuration is invalid or exceeds supported bounds")
			return
		}
		requestHash = s.createRequestHash("storage_settings:"+r.PathValue("project")+":"+r.PathValue("branch")+":"+bucket, record{"input": input, "etag": r.Header.Get("If-Match")})
	}
	r, done, ok := storageAdmit(w, r)
	if !ok {
		return
	}
	defer done()
	conn, p, close, e := s.openObjectStorage(r, r.PathValue("project"), r.PathValue("branch"))
	if e != nil {
		s.storageFailure(w, r, e)
		return
	}
	defer close()
	var desired *storageBucketSettingsInput
	if r.Method != http.MethodGet {
		desired = &input
	}
	response, etag, replayed, e := nativeStorageBucketSettings(r.Context(), conn, p, bucket, userFrom(r).ID, keyHash(key), requestHash, r.Header.Get("If-Match"), desired)
	if e != nil {
		s.storageSettingsFailure(w, r, e)
		return
	}
	w.Header().Set("ETag", etag)
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	jsonResponse(w, 200, response)
	if desired != nil && !replayed {
		close()
		s.audit(r.Context(), userFrom(r).ID, "set_bucket_settings", "object_storage", p.BranchID, "succeeded", requestID(r))
	}
}
func (s *server) storageSettingsFailure(w http.ResponseWriter, r *http.Request, e error) {
	if f, ok := e.(*storageMutationFault); ok {
		fail(w, r, f.Status, "storage_settings_conflict", "Configuration, scope, identity or version was rejected; refresh the original state")
	} else {
		s.storageFailure(w, r, e)
	}
}
