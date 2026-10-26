package control

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func functionArtifactTestZIP(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	f, err := w.Create("index.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("export default { fetch() { return new Response('artifact'); } };")); err != nil || w.Close() != nil {
		t.Fatal("test ZIP creation failed")
	}
	return archive.Bytes()
}

// This is an actual SDK/HTTP wire and recovery boundary test. It is explicitly
// not a real MinIO acceptance; the live-tag test provides that separate gate.
func TestFunctionArtifactConditionalWireRecoveryAndIntegrity(t *testing.T) {
	archive := functionArtifactTestZIP(t)
	var mu sync.Mutex
	var object []byte
	var puts int
	var loseReply, corrupt bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/neon-product-ci/functions/prj_0123456789abcdef/br_0123456789abcdef/sha256/") || r.URL.RawQuery != "" {
			w.WriteHeader(400)
			return
		}
		if r.Method == "PUT" {
			puts++
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("SDK did not preserve immutable creation precondition")
				w.WriteHeader(400)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			if object != nil {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(412)
				_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>")
				return
			}
			object = bytes.Clone(archive)
			if loseReply {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = connection.Close()
				}
				return
			}
			w.Header().Set("ETag", `"wire-test-etag"`)
			return
		}
		if r.Method != "GET" || object == nil {
			w.WriteHeader(404)
			return
		}
		data := bytes.Clone(object)
		if corrupt {
			data[0] ^= 1
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("ETag", `"wire-test-etag"`)
		w.Header().Set("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
		_, _ = w.Write(data)
	}))
	defer peer.Close()
	client, err := minio.New(strings.TrimPrefix(peer.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("ci-access", "ci-disposable-secret", ""), Secure: false, Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
	if err != nil {
		t.Fatal(err)
	}
	b := &storageBlobs{client: client, bucket: "neon-product-ci"}
	ref, err := b.reserveFunctionArtifact(context.Background(), "prj_0123456789abcdef", "br_0123456789abcdef", archive)
	if err != nil {
		t.Fatal("conditional creation failed", err)
	}
	if _, err = b.reserveFunctionArtifact(context.Background(), ref.ProjectID, ref.BranchID, archive); err != nil {
		t.Fatal("existing exact artifact not recovered", err)
	}
	if body, err := b.readFunctionArtifact(context.Background(), ref); err != nil || !bytes.Equal(body, archive) {
		t.Fatal("actual GET did not verify content", err)
	}
	public, _ := json.Marshal(ref)
	if bytes.Contains(public, []byte(ref.Key)) || bytes.Contains(public, archive) {
		t.Fatal("private artifact backing serialized")
	}
	mu.Lock()
	object, loseReply = nil, true
	mu.Unlock()
	if _, err = b.reserveFunctionArtifact(context.Background(), ref.ProjectID, ref.BranchID, archive); err != nil {
		t.Fatal("unknown immutable PUT outcome not recovered by GET", err)
	}
	mu.Lock()
	corrupt = true
	previous := puts
	mu.Unlock()
	if _, err = b.reserveFunctionArtifact(context.Background(), ref.ProjectID, ref.BranchID, archive); err == nil {
		t.Fatal("existing corrupt artifact adopted")
	}
	if _, err = b.readFunctionArtifact(context.Background(), ref); err == nil {
		t.Fatal("corrupt read accepted")
	}
	wrong := ref
	wrong.BranchID = "br_ffffffffffffffff"
	if _, err = b.readFunctionArtifact(context.Background(), wrong); err == nil {
		t.Fatal("foreign branch key accepted")
	}
	if _, err = b.reserveFunctionArtifact(context.Background(), ref.ProjectID, "../other", archive); err == nil {
		t.Fatal("scope traversal accepted")
	}
	if _, err = b.reserveFunctionArtifact(context.Background(), ref.ProjectID, ref.BranchID, []byte("not ZIP")); err == nil {
		t.Fatal("invalid archive accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != previous+1 || !bytes.Equal(object, archive) {
		t.Fatal("validation retried a mutation or corrupt object was replaced")
	}
}
