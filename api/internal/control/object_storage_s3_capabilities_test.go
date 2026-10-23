package control

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestS3UserMetadataUsesLowercaseWireNamesForBotocore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeStorageMetadata(w, map[string]string{"sdk": "boto3", "user-key": "retained"})
		w.WriteHeader(200)
	}))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: metadata.test\r\nConnection: close\r\n\r\n")
	reader := bufio.NewReader(conn)
	count := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "x-amz-meta-") {
			if !strings.HasPrefix(line, "x-amz-meta-sdk:") && !strings.HasPrefix(line, "x-amz-meta-user-key:") {
				t.Fatal("metadata key case changed on wire", line)
			}
			count++
		}
	}
	if count != 2 {
		t.Fatal("metadata headers missing")
	}
}

func TestS3CapabilityHeadersRejectBeforeNativeOrBlobAccess(t *testing.T) {
	for _, header := range []struct{ name, value string }{
		{"X-Amz-Grant-Read", `uri="http://acs.amazonaws.com/groups/global/AllUsers"`},
		{"X-Amz-Grant-Full-Control", `id="foreign"`},
		{"X-Amz-Server-Side-Encryption", "AES256"},
		{"X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256"},
		{"X-Amz-Copy-Source-Server-Side-Encryption-Customer-Algorithm", "AES256"},
		{"X-Amz-Object-Lock-Mode", "GOVERNANCE"},
		{"X-Amz-Website-Redirect-Location", "https://invalid.example"},
		{"X-Amz-Acl", "public-read"}, {"X-Amz-Storage-Class", "GLACIER"},
	} {
		for _, operation := range []string{"object", "bucket", "multipart"} {
			r := httptest.NewRequest(http.MethodPut, "http://s3.test/owned/object", nil)
			r.Header.Set(header.name, header.value)
			w := httptest.NewRecorder()
			s := &server{}
			switch operation {
			case "object":
				s.s3WriteObject(w, r, nil, objectStoragePayload{}, "owned", "object", []byte("not-published"))
			case "bucket":
				s.s3Bucket(w, r, nil, objectStoragePayload{}, "owned", nil, nil)
			case "multipart":
				r.Method = http.MethodPost
				s.s3Multipart(w, r, nil, objectStoragePayload{}, "actor", "owned", "object", nil, url.Values{"uploads": {""}})
			}
			if w.Code != 501 {
				t.Fatalf("%s %s acknowledged unsupported semantics: %d", operation, header.name, w.Code)
			}
		}
	}
	for _, header := range []struct{ name, value string }{{"X-Amz-Acl", "private"}, {"X-Amz-Storage-Class", "STANDARD"}, {"X-Amz-Tagging", "phase=allowed"}, {"Cache-Control", "private"}} {
		r := httptest.NewRequest("PUT", "http://s3.test/owned/object", nil)
		r.Header.Set(header.name, header.value)
		if rejectUnsupportedS3Headers(httptest.NewRecorder(), r) {
			t.Fatal("supported write metadata rejected", header.name)
		}
	}
}

func TestS3ObjectACLReadAndWireWritesRemainClosed(t *testing.T) {
	f := newSettingsPGFixture(t)
	s := &server{}
	for _, input := range []struct {
		method, key, resource string
		status                int
	}{{"GET", "source.txt", "acl", 200}, {"GET", "missing.txt", "acl", 404}, {"PUT", "source.txt", "acl", 501}, {"PUT", "", "policy", 501}, {"PUT", "", "acl", 501}} {
		r := httptest.NewRequest(input.method, "http://s3.test/settings-bucket/"+input.key+"?"+input.resource, nil)
		w := httptest.NewRecorder()
		if !s.s3Settings(w, r, f.conn, f.p, "settings-bucket", input.key, nil, url.Values{input.resource: {""}}) || w.Code != input.status {
			t.Fatalf("ACL/policy contract: %+v %d", input, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "http://s3.test/settings-bucket/private-init?uploads", nil)
	r.Header.Set("X-Amz-Acl", "private")
	r.Header.Set("X-Amz-Storage-Class", "STANDARD")
	w := httptest.NewRecorder()
	s.s3Multipart(w, r, f.conn, f.p, "actor", "settings-bucket", "private-init", nil, url.Values{"uploads": {""}})
	if w.Code != 200 {
		t.Fatal("explicit private/STANDARD multipart initiation rejected", w.Code, w.Body.String())
	}
}
