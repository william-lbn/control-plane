package control

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStorageUploadCapabilityBindingAndDomainSeparation(t *testing.T) {
	s := &server{idempotencyKey: bytes.Repeat([]byte("k"), 32)}
	v := storageUpload{Branch: "br_0123456789abcdef", Bucket: "upload-bucket", Key: "folder/中文.txt", Actor: "actor", Generation: 3, Expires: time.Now().Unix() + 300, SHA256: strings.Repeat("a", 64), Size: 4, ContentType: "text/plain"}
	token := s.signStorageUpload(v)
	got, ok := s.decodeStorageUpload(token)
	if !ok || got != v {
		t.Fatal("valid upload token failed")
	}
	if _, ok = s.decodeStorageDownload(token); ok {
		t.Fatal("upload token accepted as download")
	}
	download := s.signStorageDownload(storageDownload{Branch: v.Branch, Bucket: v.Bucket, Key: v.Key, Actor: v.Actor, Generation: v.Generation, Expires: v.Expires})
	if _, ok = s.decodeStorageUpload(download); ok {
		t.Fatal("read token grants write")
	}
	for _, change := range []func(*storageUpload){func(x *storageUpload) { x.Expires = time.Now().Unix() - 1 }, func(x *storageUpload) { x.Expires = time.Now().Unix() + 901 }, func(x *storageUpload) { x.Size = storageObjectLimit + 1 }, func(x *storageUpload) { x.Key = "../escape" }, func(x *storageUpload) { x.SHA256 = "invalid" }, func(x *storageUpload) { x.Actor = "" }, func(x *storageUpload) { x.Generation = 0 }} {
		bad := v
		change(&bad)
		if _, ok = s.decodeStorageUpload(s.signStorageUpload(bad)); ok {
			t.Error("invalid capability accepted")
		}
	}
	if _, ok = s.decodeStorageUpload(token + "tamper"); ok {
		t.Fatal("tampered signature accepted")
	}
}
func TestStorageUploadRejectsBeforeDatabaseOrWake(t *testing.T) {
	s := &server{idempotencyKey: bytes.Repeat([]byte("k"), 32)}
	v := storageUpload{Branch: "br_0123456789abcdef", Bucket: "upload-bucket", Key: "new.txt", Actor: "actor", Generation: 1, Expires: time.Now().Unix() + 300, SHA256: strings.Repeat("a", 64), Size: 4, ContentType: "text/plain"}
	for _, name := range []string{"invalid_token", "wrong_branch", "wrong_key", "content_type", "create_condition", "payload"} {
		t.Run(name, func(t *testing.T) {
			token := s.signStorageUpload(v)
			if name == "invalid_token" {
				token += "bad"
			}
			req := httptest.NewRequest(http.MethodPut, "/storage/v1/"+v.Branch+"/"+v.Bucket+"?key=new.txt&token="+token, strings.NewReader("xxxx"))
			req.SetPathValue("storageBranch", v.Branch)
			req.SetPathValue("bucket", v.Bucket)
			req.Header.Set("Content-Type", v.ContentType)
			req.Header.Set("If-None-Match", "*")
			switch name {
			case "wrong_branch":
				req.SetPathValue("storageBranch", "br_other")
			case "wrong_key":
				req.URL.RawQuery = strings.Replace(req.URL.RawQuery, "new.txt", "other.txt", 1)
			case "content_type":
				req.Header.Set("Content-Type", "application/octet-stream")
			case "create_condition":
				req.Header.Del("If-None-Match")
			}
			w := httptest.NewRecorder()
			s.storagePublicUpload(w, req)
			if w.Code != 403 && !(name == "payload" && w.Code == 400) {
				t.Fatalf("invalid capability reached database: %d", w.Code)
			}
		})
	}
}
