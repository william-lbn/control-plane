package control

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestS3SettingsTypedValidation(t *testing.T) {
	tags, e := normalizeStorageTags([]storageTag{{Key: "z", Value: "last"}, {Key: "环境", Value: "验证"}, {Key: "a", Value: ""}}, 10)
	if e != nil || tags[0].Key != "a" {
		t.Fatal("valid canonical Unicode tags", e)
	}
	for _, tags := range [][]storageTag{{{Key: "", Value: "x"}}, {{Key: "a"}, {Key: "a"}}, {{Key: "aws:reserved"}}, {{Key: "line\nbreak"}}, {{Key: "a", Value: strings.Repeat("x", 257)}}} {
		if _, e := normalizeStorageTags(tags, 10); e == nil {
			t.Fatal("invalid tags accepted")
		}
	}
	eleven := []storageTag{}
	for _, k := range "abcdefghijk" {
		eleven = append(eleven, storageTag{Key: string(k)})
	}
	if _, e = normalizeStorageTags(eleven, 10); e == nil {
		t.Fatal("object tag limit bypassed")
	}
	if _, e = normalizeStorageTags(eleven, 50); e != nil {
		t.Fatal("bucket tag limit confused with object limit")
	}
	days := 1
	good := storageBucketSettings{Tags: tags, Lifecycle: &storageLifecycle{Rules: []storageLifecycleRule{{ID: "stored-only", Status: "Enabled", Expiration: &storageExpiration{Days: &days}}}}}
	if _, e = normalizeStorageSettings(good); e != nil {
		t.Fatal(e)
	}
	zero := 0
	bad := good
	bad.Lifecycle = &storageLifecycle{Rules: []storageLifecycleRule{{Status: "Enabled", Expiration: &storageExpiration{Days: &zero}}}}
	if _, e = normalizeStorageSettings(bad); e == nil {
		t.Fatal("zero expiry accepted")
	}
	bad = good
	bad.Versioning.MFADelete = "Enabled"
	if _, e = normalizeStorageSettings(bad); e == nil {
		t.Fatal("unsupported MFA enforcement claimed")
	}
	for _, headers := range []map[string]string{{"Cache-Control": "public\r\nX-Injected: yes"}, {"Content-Disposition": "attachment; filename=../x"}, {"Set-Cookie": "session=wrong"}, {"cache-control": "a", "Cache-Control": "b"}} {
		if _, e = normalizeStorageResponseHeaders(headers); e == nil {
			t.Fatal("unsafe response header accepted")
		}
	}
	if _, e = normalizeStorageResponseHeaders(map[string]string{"content-disposition": "attachment; filename=report.txt", "cache-control": "private, max-age=60"}); e != nil {
		t.Fatal(e)
	}
	if !storageAnonymousAllowed("public_read", storageBucketSettings{}) || storageAnonymousAllowed("private", good) {
		t.Fatal("anonymous ACL boundary")
	}
	for _, block := range []storagePublicAccessBlock{{IgnorePublicACLs: true}, {RestrictPublicBuckets: true}} {
		if storageAnonymousAllowed("public_read", storageBucketSettings{PublicAccessBlock: &block}) {
			t.Fatal("public block bypassed")
		}
	}
	if storageSettingsETag("br_parent", "bucket", "private", 1, good) == storageSettingsETag("br_child", "bucket", "private", 1, good) {
		t.Fatal("branch configuration ETag shared")
	}
}
func TestS3SettingsXMLClosedGrammar(t *testing.T) {
	good := `<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet><Tag><Key>环境</Key><Value>验证</Value></Tag></TagSet></Tagging>`
	var v struct {
		Tags []storageTag `xml:"TagSet>Tag"`
	}
	if !decodeStorageConfiguration([]byte(good), "Tagging", &v) || len(v.Tags) != 1 {
		t.Fatal("valid native tagging XML")
	}
	for _, raw := range []string{good + good, strings.Replace(good, "</Key>", "</Key><Key>duplicate</Key>", 1), strings.Replace(good, "<Value>验证</Value>", "", 1), strings.Replace(good, "</Tag>", "<Unknown/></Tag>", 1), strings.Replace(good, s3XMLNamespace, "urn:wrong", 1), `<!DOCTYPE x SYSTEM "file:///etc/passwd">` + good, `<Tagging/>`} {
		if decodeStorageConfiguration([]byte(raw), "Tagging", &v) {
			t.Fatal("ambiguous or external XML accepted")
		}
	}
	w := httptest.NewRecorder()
	s3ConfigurationXML(w, "Tagging", struct {
		Tags []storageTag `xml:"TagSet>Tag"`
	}{Tags: v.Tags})
	var decoded struct {
		XMLName xml.Name
		Tags    []storageTag `xml:"TagSet>Tag"`
	}
	if w.Code != 200 || xml.Unmarshal(w.Body.Bytes(), &decoded) != nil || decoded.XMLName.Local != "Tagging" {
		t.Fatal("typed anonymous structure did not produce native XML")
	}
	for _, query := range []string{"tagging&cors", "acl&delete", "lifecycle&uploads", "attributes&policy"} {
		q, _ := url.ParseQuery(query)
		if s3QuerySupported(q) {
			t.Fatal("ambiguous subresource admitted")
		}
	}
	for _, query := range []string{"tagging&versionId=null&x-id=GetObjectTagging", "uploadId=known&partNumber=1", "list-type=2&prefix=x&fetch-owner=true"} {
		q, _ := url.ParseQuery(query)
		if !s3QuerySupported(q) {
			t.Fatal("supported SDK selector rejected", query)
		}
	}
}

type settingsPGFixture struct {
	ctx         context.Context
	admin, conn *pgx.Conn
	config      *pgx.ConnConfig
	db          string
	p           objectStoragePayload
}

func newSettingsPGFixture(t *testing.T) *settingsPGFixture {
	t.Helper()
	cfg, e := pgx.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if e != nil || cfg.Database != "control_ci" {
		t.Fatal("Dedicated control_ci required")
	}
	ctx := context.Background()
	admin, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	suffix := newID("")[:16]
	name := "s3_settings_ci_" + suffix
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(name)); e != nil {
		admin.Close(ctx)
		t.Fatal(e)
	}
	target := cfg.Copy()
	target.Database = name
	conn, e := pgx.ConnectConfig(ctx, target)
	if e != nil {
		t.Fatal(e)
	}
	f := &settingsPGFixture{ctx: ctx, admin: admin, conn: conn, config: target, db: name, p: objectStoragePayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Generation: 1, Spec: ObjectStorageSpec{Database: name}}}
	t.Cleanup(func() {
		f.conn.Close(ctx)
		if _, e := admin.Exec(ctx, "DROP DATABASE "+pgQuote(name)+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
		admin.Close(ctx)
	})
	tx, e := conn.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = applyObjectStorageSQLTx(ctx, tx, f.p); e != nil {
		tx.Rollback(ctx)
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.buckets(name,access) VALUES('settings-bucket','private')`); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('settings-bucket','source.txt',$1,repeat('a',64),4,'text/plain')`, f.p.ProjectID+"/blob_original"); e != nil {
		t.Fatal(e)
	}
	return f
}
func requireSettingsFault(t *testing.T, e error, status int) {
	t.Helper()
	var fault *storageMutationFault
	if !errors.As(e, &fault) || fault.Status != status {
		t.Fatalf("expected functional rejection %d, got %v", status, e)
	}
}

func TestS3SettingsBucketPostgresReplayAndClone(t *testing.T) {
	f := newSettingsPGFixture(t)
	ctx, conn, p := f.ctx, f.conn, f.p
	_, etag, _, e := nativeStorageBucketSettings(ctx, conn, p, "settings-bucket", "", "", "", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	v := storageBucketSettings{Tags: []storageTag{{Key: "owner", Value: "parent"}}, PublicAccessBlock: &storagePublicAccessBlock{BlockPublicACLs: true}}
	input := &storageBucketSettingsInput{Access: "private", Settings: &v}
	original, next, replay, e := nativeStorageBucketSettings(ctx, conn, p, "settings-bucket", "actor", keyHash("original-settings-key"), "original-request", etag, input)
	if e != nil || replay || next == etag {
		t.Fatal("settings commit", e)
	}
	input.Access = "public_read"
	_, _, _, e = nativeStorageBucketSettings(ctx, conn, p, "settings-bucket", "actor", keyHash("grant-public-key"), "grant-request", next, input)
	requireSettingsFault(t, e, 409)
	input.Access = "private"
	again, replayedETag, replay, e := nativeStorageBucketSettings(ctx, conn, p, "settings-bucket", "actor", keyHash("original-settings-key"), "original-request", etag, input)
	a, _ := json.Marshal(original)
	b, _ := json.Marshal(again)
	if e != nil || !replay || replayedETag != next || !bytes.Equal(canonicalSettingsJSON(t, a), canonicalSettingsJSON(t, b)) {
		t.Fatal("original reply not replayed exactly", e)
	}
	_, _, _, e = nativeStorageBucketSettings(ctx, conn, p, "settings-bucket", "actor", keyHash("original-settings-key"), "different-request", etag, input)
	requireSettingsFault(t, e, 409)
	_, _, _, e = nativeStorageBucketSettings(ctx, conn, p, "settings-bucket", "actor", keyHash("stale-state-key"), "stale-request", etag, input)
	requireSettingsFault(t, e, 412)
	for _, sql := range []string{`UPDATE neon_storage.setting_mutations SET request_hash='forged'`, `DELETE FROM neon_storage.setting_mutations`} {
		if _, e = conn.Exec(ctx, sql); e == nil {
			t.Fatal("receipt history mutable")
		}
	}
	if e = conn.Close(ctx); e != nil {
		t.Fatal(e)
	}
	child := f.db + "_child"
	if _, e = f.admin.Exec(ctx, "CREATE DATABASE "+pgQuote(child)+" TEMPLATE "+pgQuote(f.db)); e != nil {
		t.Fatal(e)
	}
	childCfg := f.config.Copy()
	childCfg.Database = child
	clone, e := pgx.ConnectConfig(ctx, childCfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		clone.Close(ctx)
		if _, e = f.admin.Exec(ctx, "DROP DATABASE "+pgQuote(child)+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	childP := p
	childP.BranchID = "br_" + newID("")[:16]
	tx, e := clone.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = applyObjectStorageSQLTx(ctx, tx, childP); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	_, childETag, _, e := nativeStorageBucketSettings(ctx, clone, childP, "settings-bucket", "", "", "", "", nil)
	if e != nil || childETag == next {
		t.Fatal("child identity not distinct", e)
	}
	v.Tags[0].Value = "child"
	_, _, replay, e = nativeStorageBucketSettings(ctx, clone, childP, "settings-bucket", "actor", keyHash("original-settings-key"), "child-request", childETag, input)
	if e != nil || replay {
		t.Fatal("parent receipt applied to child", e)
	}
	f.conn, e = pgx.ConnectConfig(ctx, f.config)
	if e != nil {
		t.Fatal(e)
	}
	var value string
	if e = f.conn.QueryRow(ctx, `SELECT settings->'tags'->0->>'value' FROM neon_storage.buckets WHERE name='settings-bucket'`).Scan(&value); e != nil || value != "parent" {
		t.Fatal("child settings changed parent", e)
	}
}

func TestS3SettingsObjectPostgresCASAndCopy(t *testing.T) {
	f := newSettingsPGFixture(t)
	ctx, conn, p := f.ctx, f.conn, f.p
	_, etag, _, e := nativeStorageObjectSettings(ctx, conn, p, "settings-bucket", "source.txt", "", "", "", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	v := storageObjectSettings{Tags: []storageTag{{Key: "kind", Value: "original"}}, ResponseHeaders: map[string]string{"cache-control": "private, max-age=60", "content-disposition": "attachment; filename=report.txt"}}
	original, next, replay, e := nativeStorageObjectSettings(ctx, conn, p, "settings-bucket", "source.txt", "actor", keyHash("object-original-key"), "object-request", etag, &v)
	if e != nil || replay || next == etag {
		t.Fatal(e)
	}
	_, e = copyStorageObject(ctx, conn, p.ProjectID, "settings-bucket", "source.txt", "settings-bucket", "copied.txt", strings.Repeat("a", 64), "", "*")
	if e != nil {
		t.Fatal(e)
	}
	var tags []storageTag
	var headers map[string]string
	if e = conn.QueryRow(ctx, `SELECT tags,response_headers FROM neon_storage.objects WHERE key='copied.txt'`).Scan(&tags, &headers); e != nil || !reflect.DeepEqual(tags, v.Tags) || !reflect.DeepEqual(headers, v.ResponseHeaders) {
		t.Fatal("COPY lost source settings", e)
	}
	replacement := []storageTag{{Key: "kind", Value: "replaced"}}
	_, e = copyStorageObject(ctx, conn, p.ProjectID, "settings-bucket", "source.txt", "settings-bucket", "tags-only.txt", strings.Repeat("a", 64), "", "*", storageCopyMetadata{Tags: &replacement, OnlyTags: true})
	if e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(ctx, `SELECT tags,response_headers FROM neon_storage.objects WHERE key='tags-only.txt'`).Scan(&tags, &headers); e != nil || tags[0].Value != "replaced" || !reflect.DeepEqual(headers, v.ResponseHeaders) {
		t.Fatal("tagging directive rewrote HTTP metadata", e)
	}
	if _, e = conn.Exec(ctx, `UPDATE neon_storage.objects SET blob_key=$1 WHERE key='source.txt'`, p.ProjectID+"/blob_successor"); e != nil {
		t.Fatal(e)
	}
	_, _, _, e = nativeStorageObjectSettings(ctx, conn, p, "settings-bucket", "source.txt", "actor", keyHash("new-change-key"), "new-change", next, &v)
	requireSettingsFault(t, e, 412)
	again, replayETag, replay, e := nativeStorageObjectSettings(ctx, conn, p, "settings-bucket", "source.txt", "actor", keyHash("object-original-key"), "object-request", etag, &v)
	a, _ := json.Marshal(original)
	b, _ := json.Marshal(again)
	if e != nil || !replay || replayETag != next || !bytes.Equal(canonicalSettingsJSON(t, a), canonicalSettingsJSON(t, b)) {
		t.Fatal("unknown reply overwrote a successor object", e)
	}
	var blob string
	if e = conn.QueryRow(ctx, `SELECT blob_key FROM neon_storage.objects WHERE key='source.txt'`).Scan(&blob); e != nil || !strings.HasSuffix(blob, "/blob_successor") {
		t.Fatal("successor identity changed", e)
	}
}

func TestS3SettingsNativeWirePostgresConfigurations(t *testing.T) {
	f := newSettingsPGFixture(t)
	s := &server{}
	call := func(method, key, query, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://s3.test/settings-bucket/"+key+"?"+query, strings.NewReader(body))
		w := httptest.NewRecorder()
		q, _ := url.ParseQuery(query)
		if !s.s3Settings(w, r, f.conn, f.p, "settings-bucket", key, []byte(body), q) {
			t.Fatal("native subresource not dispatched")
		}
		return w
	}
	if w := call("PUT", "", "tagging", `<Tagging><TagSet><Tag><Key>环境</Key><Value>验证</Value></Tag></TagSet></Tagging>`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("GET", "", "tagging", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "验证") {
		t.Fatal("stored tagging response")
	}
	if w := call("GET", "", "acl", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "FULL_CONTROL") {
		t.Fatal("read-only branch ACL")
	}
	if w := call("PUT", "", "acl", `<AccessControlPolicy/>`); w.Code != 501 {
		t.Fatal("ACL wire writes accepted")
	}
	for _, input := range []struct{ query, body, root string }{{"versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, "VersioningConfiguration"}, {"lifecycle", `<LifecycleConfiguration><Rule><ID>stored</ID><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`, "LifecycleConfiguration"}, {"object-lock", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`, "ObjectLockConfiguration"}, {"publicAccessBlock", `<PublicAccessBlockConfiguration><IgnorePublicAcls>true</IgnorePublicAcls></PublicAccessBlockConfiguration>`, "PublicAccessBlockConfiguration"}} {
		if w := call("PUT", "", input.query, input.body); w.Code != 200 {
			t.Fatal(input.query, w.Code, w.Body.String())
		}
		w := call("GET", "", input.query, "")
		var root struct{ XMLName xml.Name }
		if w.Code != 200 || xml.Unmarshal(w.Body.Bytes(), &root) != nil || root.XMLName.Local != input.root {
			t.Fatal("native configuration response root", input.query, w.Code, w.Body.String())
		}
	}
	if w := call("PUT", "source.txt", "tagging", `<Tagging><TagSet><Tag><Key>kind</Key><Value>object</Value></Tag></TagSet></Tagging>`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("GET", "source.txt", "tagging", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "object") {
		t.Fatal("object tagging")
	}
	r := httptest.NewRequest("GET", "http://s3.test/settings-bucket/source.txt?attributes", nil)
	r.Header.Set("X-Amz-Object-Attributes", "ETag,ObjectSize,Checksum")
	w := httptest.NewRecorder()
	s.s3ObjectAttributes(w, r, f.conn, f.p, "settings-bucket", "source.txt")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "FULL_OBJECT") || !strings.Contains(w.Body.String(), "<ObjectSize>4</ObjectSize>") {
		t.Fatal("object attributes", w.Code, w.Body.String())
	}
	for _, query := range []string{"lifecycle", "publicAccessBlock"} {
		if w := call("DELETE", "", query, ""); w.Code != 204 {
			t.Fatal("configuration delete")
		}
		if w := call("GET", "", query, ""); w.Code != 404 {
			t.Fatal("removed configuration silently present")
		}
	}
	var count int
	if e := f.conn.QueryRow(f.ctx, `SELECT count(*) FROM neon_storage.objects`).Scan(&count); e != nil || count != 1 {
		t.Fatal("configuration writes removed object references", e)
	}
}

func TestS3SettingsMultipartPostgresMetadataReceipt(t *testing.T) {
	f := newSettingsPGFixture(t)
	s := &server{}
	input := multipartCreate{Key: "pending.txt", ContentType: "text/plain", Size: -1, Tags: []storageTag{{Key: "kind", Value: "multipart"}}, ResponseHeaders: map[string]string{"cache-control": "private, max-age=10"}}
	a, e := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "original-upload-key", input)
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "original-upload-key", input)
	if e != nil || a.ID != b.ID || !reflect.DeepEqual(a.Tags, input.Tags) || !reflect.DeepEqual(a.ResponseHeaders, input.ResponseHeaders) {
		t.Fatal("original upload settings or receipt lost", e)
	}
	input.Tags[0].Value = "changed"
	_, e = s.createMultipart(f.ctx, f.conn, f.p, "actor", "settings-bucket", "original-upload-key", input)
	requireSettingsFault(t, e, 409)
	// Optional new empty fields must leave the old native request fingerprint unchanged.
	empty := multipartCreate{Key: "old-shape.txt", ContentType: "text/plain", Metadata: map[string]string{}, Size: -1}
	raw, _ := json.Marshal(empty)
	if strings.Contains(string(raw), "tags") || strings.Contains(string(raw), "response_headers") {
		t.Fatal("legacy original input shape changed")
	}
}

func TestS3SettingsAnonymousAuthenticationDoesNotDowngrade(t *testing.T) {
	t.Setenv("NEON_S3_PUBLIC_ORIGIN", "http://s3.test")
	s := &server{}
	cases := []struct{ path, authorization string }{{"/s3/br_0123456789abcdef/public-bucket/key?X-Amz-Date=malformed", ""}, {"/s3/br_0123456789abcdef/public-bucket/key", "invalid"}, {"/s3/br_0123456789abcdef/public-bucket?list-type=2", ""}, {"/s3/br_0123456789abcdef/public-bucket/key?tagging", ""}}
	for _, input := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://s3.test"+input.path, nil)
		r.Header.Set("Authorization", input.authorization)
		w := httptest.NewRecorder()
		s.s3Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("authenticated error or anonymous listing downgraded", w.Code)
		}
	}
}

func canonicalSettingsJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if e := json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	result, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return result
}
