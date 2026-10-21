package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

// Actual SDK/HTTP wire plus disposable metadata. Native SQL permissions and
// Neon timelines are verified separately; this fixture claims neither.
func TestFunctionBranchCopyBackingRewrapLostReplyAndOriginalUID(t *testing.T) {
	f := newStorageEventFixture(t)
	t.Setenv("NEON_FUNCTION_BRANCH_COPY_ENABLED", "true")
	const child = "br_2123456789abcdef"
	const sourceFn = "fnc_1123456789abcdef"
	const sourceDID = "fdp_3123456789abcdef"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	archive := functionArtifactTestZIP(t)
	bundle, err := functions.ValidateBundle(archive)
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"APP_CONTEXT": "disposable-copy-value"}
	sourceRef := functionEnvironmentReference{Namespace: "neon", ProjectID: f.p.ProjectID, BranchID: f.p.BranchID, DeploymentID: sourceDID, Name: "fdp-3123456789abcdef-environment", UID: "11111111-1111-4111-8111-111111111111"}
	mac, canonical, err := functionEnvironmentMAC(f.s.functions.key, sourceRef, environment)
	if err != nil {
		t.Fatal(err)
	}
	sourceRef.MAC = mac
	sourceKey, _ := functionArtifactKey(f.p.ProjectID, f.p.BranchID, bundle.Digest)
	destKey, _ := functionArtifactKey(f.p.ProjectID, child, bundle.Digest)
	var mu sync.Mutex
	blobs := map[string][]byte{sourceKey: bytes.Clone(archive)}
	blobPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		defer r.Body.Close()
		key := strings.TrimPrefix(r.URL.Path, "/neon-product-ci/")
		if key != sourceKey && key != destKey {
			w.WriteHeader(400)
			return
		}
		if r.Method == "PUT" {
			if key != destKey || r.Header.Get("If-None-Match") != "*" {
				w.WriteHeader(400)
				return
			}
			io.Copy(io.Discard, r.Body)
			if _, found := blobs[key]; found {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(412)
				io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
				return
			}
			blobs[key] = bytes.Clone(archive)
			w.Header().Set("ETag", `"copy-wire"`)
			return
		}
		body, found := blobs[key]
		if r.Method != "GET" || !found {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("ETag", `"copy-wire"`)
		w.Header().Set("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
		w.Write(body)
	}))
	defer blobPeer.Close()
	client, err := minio.New(strings.TrimPrefix(blobPeer.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("ci-access", "ci-disposable-secret", ""), Secure: false, Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
	if err != nil {
		t.Fatal(err)
	}
	f.s.storage = &storageBlobs{client: client, bucket: "neon-product-ci"}
	secrets := map[string]map[string]any{sourceRef.Name: {"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true, "metadata": map[string]any{"name": sourceRef.Name, "namespace": "neon", "uid": sourceRef.UID, "labels": functionEnvironmentLabels(sourceRef), "annotations": map[string]any{"neon-control/environment-mac": mac}}, "data": map[string]any{"environment.json": base64.StdEncoding.EncodeToString(canonical)}}}
	posts := 0
	secretPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/neon/secrets/")
			v, found := secrets[name]
			if !found {
				w.WriteHeader(404)
				io.WriteString(w, `{"message":"absent"}`)
				return
			}
			json.NewEncoder(w).Encode(v)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/namespaces/neon/secrets" {
			w.WriteHeader(400)
			return
		}
		var v map[string]any
		if json.NewDecoder(r.Body).Decode(&v) != nil {
			w.WriteHeader(400)
			return
		}
		name := stringVal(nested(v, "metadata", "name"))
		if _, found := secrets[name]; found {
			w.WriteHeader(409)
			return
		}
		posts++
		v["metadata"].(map[string]any)["uid"] = "22222222-2222-4222-8222-222222222222"
		secrets[name] = v
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			connection.Close()
		}
	}))
	defer secretPeer.Close()
	f.s.kube = &kubeClient{base: secretPeer.URL, namespace: "neon", http: secretPeer.Client()}
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_2123456789abcdef',$1,$2,'copy-writer','neonvm','copy-writer','copy-writer','suspended','cloud_admin','neondb',now(),now())`, f.p.ProjectID, child)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,finished_at) VALUES('copy_source_original',$1,'function',$2,'deploy_function','succeeded','event_actor','copy-source-ci','{}',now())`, f.p.ProjectID, sourceFn)
	exec(`INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,environment_names,actor_id,operation_id,state,finished_at) VALUES($1,$2,$3,$4,'nodejs24',$5,$6,'index.mjs',$7,$8,ARRAY['APP_CONTEXT'],'event_actor','copy_source_original','completed',now())`, sourceDID, sourceFn, f.p.ProjectID, f.p.BranchID, bundle.Digest, len(archive), sourceKey, sourceRef.Name)
	exec(`INSERT INTO function_deployment_backings(deployment_id,function_id,project_id,branch_id,environment_namespace,environment_secret_ref,environment_secret_uid,environment_mac,mac_key_version) VALUES($1,$2,$3,$4,'neon',$5,$6,$7,'event-ci-v1')`, sourceDID, sourceFn, f.p.ProjectID, f.p.BranchID, sourceRef.Name, sourceRef.UID, mac)
	input, _ := json.Marshal(functionCopyInput{SourceBranch: f.p.BranchID, SourceSlug: "onupload", SourceDeployment: sourceDID, Slug: "wirecopy", Acknowledge: true})
	request := httptest.NewRequest("POST", "/api/v1/projects/"+f.p.ProjectID+"/branches/"+child+"/functions-copy", bytes.NewReader(input))
	request.Header.Set("Cookie", "neon_v2_session=event_actor-session; neon_v2_csrf=event_actor-csrf")
	request.Header.Set("X-CSRF-Token", "event_actor-csrf")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "copy-backing-wire01")
	request.Header.Set("If-Match", `"1"`)
	response := httptest.NewRecorder()
	f.s.routes().ServeHTTP(response, request)
	if response.Code != 202 {
		t.Fatal("wire copy admission rejected", response.Code, response.Body.String())
	}
	var admitted struct {
		Operation struct {
			ID      string                 `json:"id"`
			Payload functionInstanceTarget `json:"payload"`
		} `json:"operation"`
	}
	if json.Unmarshal(response.Body.Bytes(), &admitted) != nil {
		t.Fatal("admission JSON rejected")
	}
	var payload []byte
	if err = f.db.QueryRow(f.ctx, `SELECT payload FROM operations WHERE id=$1`, admitted.Operation.ID).Scan(&payload); err != nil || json.Unmarshal(payload, &admitted.Operation.Payload) != nil {
		t.Fatal("original copy target missing", err)
	}
	p := admitted.Operation.Payload
	worker := "copy-wire-worker"
	exec(`UPDATE operations SET state='running',lease_owner=$2,lease_expires_at=$3 WHERE id=$1`, admitted.Operation.ID, worker, time.Now().Add(time.Minute))
	source, err := f.s.loadFunctionCopySource(f.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = f.s.prepareFunctionCopyBacking(f.ctx, admitted.Operation.ID, worker, p, source); err != nil {
			t.Fatal("lost POST reply/restart did not preserve copied backing", err)
		}
	}
	backing, err := f.s.loadFunctionDeploymentBacking(f.ctx, admitted.Operation.ID, worker, p)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := f.s.kube.readFunctionEnvironment(f.ctx, f.s.functions.key, backing.Environment)
	if err != nil || !reflect.DeepEqual(actual, environment) || backing.Environment.UID == sourceRef.UID || backing.Environment.MAC == sourceRef.MAC || backing.Environment.BranchID != child || posts != 1 {
		t.Fatal("environment not rewrapped into original new scope", err)
	}
	if !bytes.Equal(blobs[sourceKey], archive) || !bytes.Equal(blobs[destKey], archive) {
		t.Fatal("copy changed original code bytes")
	}
	mu.Lock()
	secrets[backing.Environment.Name]["metadata"].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
	mu.Unlock()
	if f.s.prepareFunctionCopyBacking(f.ctx, admitted.Operation.ID, worker, p, source) == nil {
		t.Fatal("replacement destination UID adopted")
	}
	mu.Lock()
	secrets[backing.Environment.Name]["metadata"].(map[string]any)["uid"] = backing.Environment.UID
	secrets[sourceRef.Name]["metadata"].(map[string]any)["annotations"].(map[string]any)["neon-control/environment-mac"] = strings.Repeat("f", 64)
	mu.Unlock()
	if f.s.prepareFunctionCopyBacking(f.ctx, admitted.Operation.ID, worker, p, source) == nil {
		t.Fatal("altered original environment MAC accepted")
	}
	mu.Lock()
	secrets[sourceRef.Name]["metadata"].(map[string]any)["annotations"].(map[string]any)["neon-control/environment-mac"] = mac
	blobs[sourceKey][0] ^= 1
	mu.Unlock()
	if f.s.prepareFunctionCopyBacking(f.ctx, admitted.Operation.ID, worker, p, source) == nil {
		t.Fatal("corrupt original ZIP copied")
	}
	var count int
	if err = f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_instances WHERE function_id=$1`, p.FunctionID).Scan(&count); err != nil || count != 0 {
		t.Fatal("backing preparation executed a Guest", err)
	}
}
