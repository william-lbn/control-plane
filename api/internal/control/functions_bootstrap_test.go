package control

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// Certificate/keys are ephemeral fixture material, never saved or logged.
func functionBootstrapFixture(t *testing.T, requested ...functions.Scope) (functionBootstrapReference, functions.Bootstrap) {
	t.Helper()
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bootstrap-ci"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(8 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := functions.NewGuestIssuer(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE" + " KEY", Bytes: private}), now)
	if err != nil {
		t.Fatal(err)
	}
	scope := functions.Scope{InstanceID: "fni_0123456789abcdef", DeploymentID: "fdp_0123456789abcdef", ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", Slug: "hello", Generation: 1}
	if len(requested) == 1 {
		scope = requested[0]
	}
	identity, err := issuer.Issue(scope, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ref := functionBootstrapReference{Namespace: "neon-ci", Name: "fn-" + strings.TrimPrefix(scope.InstanceID, "fni_") + "-bootstrap", MACKeyVersion: "ci-v1", FunctionID: "fnc_0123456789abcdef", WriterID: "ep_0123456789abcdef", BundleDigest: strings.Repeat("a", 64), Scope: scope}
	b := functions.Bootstrap{Scope: scope, ManagerKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{12}, 32)), ArtifactKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{13}, 32)), ManagerCertificate: identity.Certificate, ManagerPrivateKey: identity.PrivateKey, ManagerCA: identity.CA, SQLCA: identity.CA, ArtifactCA: identity.CA, BundleDigest: ref.BundleDigest, WriterEndpointID: ref.WriterID, ProxyIP: "192.0.2.2", ProxyHostname: "sql.example.test", ProxyPort: 5432, DNSIP: "192.0.2.53", Environment: map[string]string{"GREETING": "hello"},
		DatabaseURL: "postgresql://fn_0123456789abcdef:012345678901234567890123456789@sql.example.test:5432/neondb?sslmode=verify-full&sslrootcert=%2Fetc%2Fneon-function%2Fsql-ca.crt&options=endpoint%3Dep-0123456789abcdef",
		ArtifactURL: "https://artifact.example.test/internal/v1/function-artifacts/" + scope.InstanceID + "/" + scope.DeploymentID + "/" + ref.BundleDigest}
	if b.Validate() != nil {
		t.Fatal("invalid bootstrap test contract")
	}
	return ref, b
}

func TestFunctionBootstrapLostReplyRestartAndOwnership(t *testing.T) {
	ref, config := functionBootstrapFixture(t)
	key := bytes.Repeat([]byte{17}, 32)
	var mu sync.Mutex
	var stored map[string]any
	posts := 0
	denied := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.RawQuery != "" || !strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/neon-ci/secrets") {
			w.WriteHeader(400)
			return
		}
		if r.Method == http.MethodGet {
			if denied {
				w.WriteHeader(503)
				return
			}
			if stored == nil {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(stored)
			return
		}
		if r.Method != http.MethodPost || stored != nil {
			w.WriteHeader(409)
			return
		}
		posts++
		if json.NewDecoder(r.Body).Decode(&stored) != nil {
			t.Error("invalid bootstrap create")
		}
		stored["metadata"].(map[string]any)["uid"] = "11111111-2222-3333-4444-555555555555"
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: ref.Namespace, http: peer.Client()}
	actual, err := k.reserveFunctionBootstrap(context.Background(), key, ref, config)
	if err != nil || actual.Reference.UID == "" {
		t.Fatal("lost committed bootstrap reply did not recover")
	}
	restarted := &kubeClient{base: peer.URL, namespace: ref.Namespace, http: peer.Client()}
	// Deliberately lose the caller's generated material/receipt: recover the
	// original keys and leaf from a MAC-verified GET without POST or key renewal.
	found, exists, err := restarted.findFunctionBootstrap(context.Background(), key, ref)
	if err != nil || !exists || found.Reference != actual.Reference || found.Config.ManagerPrivateKey != config.ManagerPrivateKey || found.Config.ArtifactKey != config.ArtifactKey {
		t.Fatal("restart changed the bootstrap identity")
	}
	if b, _ := json.Marshal(found); string(b) != "{}" {
		t.Fatal("bootstrap private content serialized")
	}
	read, err := restarted.readFunctionBootstrap(context.Background(), key, actual.Reference)
	if err != nil || read.Reference != actual.Reference {
		t.Fatal("durable UID not readable")
	}
	changed := config
	changed.Environment = map[string]string{"GREETING": "changed"}
	if _, err = k.reserveFunctionBootstrap(context.Background(), key, ref, changed); err == nil {
		t.Fatal("existing bootstrap overwritten or adopted for different content")
	}
	wrong := actual.Reference
	wrong.UID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err = k.readFunctionBootstrap(context.Background(), key, wrong); err == nil {
		t.Fatal("replacement UID accepted")
	}
	wrong = ref
	wrong.FunctionID = "fnc_ffffffffffffffff"
	if _, err = k.reserveFunctionBootstrap(context.Background(), key, wrong, config); err == nil {
		t.Fatal("SQL role not bound to Function definition")
	}
	mu.Lock()
	original, _ := json.Marshal(stored)
	mu.Unlock()
	for name, change := range map[string]func(map[string]any){
		"namespace": func(v map[string]any) { v["metadata"].(map[string]any)["namespace"] = "foreign" },
		"name":      func(v map[string]any) { v["metadata"].(map[string]any)["name"] = "foreign" },
		"immutable": func(v map[string]any) { v["immutable"] = false },
		"deleting":  func(v map[string]any) { v["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-11T00:00:00Z" },
		"branch": func(v map[string]any) {
			v["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/branch-id"] = "br_ffffffffffffffff"
		},
		"instance": func(v map[string]any) {
			v["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/instance-id"] = "fni_ffffffffffffffff"
		},
		"key-version": func(v map[string]any) {
			v["metadata"].(map[string]any)["annotations"].(map[string]any)["neon-control/bootstrap-key-version"] = "ci-v2"
		},
		"mac": func(v map[string]any) {
			v["metadata"].(map[string]any)["annotations"].(map[string]any)["neon-control/bootstrap-mac"] = strings.Repeat("b", 64)
		},
		"extra-key": func(v map[string]any) { v["data"].(map[string]any)["other"] = "eA==" },
		"noncanonical": func(v map[string]any) {
			v["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(append([]byte(" "), mustBootstrapJSON(t, config)...))
		},
		"changed-value": func(v map[string]any) {
			c := config
			c.Environment = map[string]string{"GREETING": "other"}
			v["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(mustBootstrapJSON(t, c))
		},
	} {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			_ = json.Unmarshal(original, &stored)
			change(stored)
			mu.Unlock()
			if _, err := k.readFunctionBootstrap(context.Background(), key, actual.Reference); err != errFunctionBootstrap {
				t.Fatal("changed backing accepted or error leaked private content")
			}
		})
	}
	mu.Lock()
	stored = nil
	mu.Unlock()
	if _, err = k.reserveFunctionBootstrap(context.Background(), key, actual.Reference, config); err != errFunctionBootstrap {
		t.Fatal("missing persisted UID admitted a replacement")
	}
	mu.Lock()
	denied = true
	mu.Unlock()
	if _, err = k.reserveFunctionBootstrap(context.Background(), key, ref, config); err != errFunctionBootstrap {
		t.Fatal("read failure admitted create")
	}
	if _, err = k.reserveFunctionBootstrap(context.Background(), key[:8], ref, config); err != errFunctionBootstrap {
		t.Fatal("weak verification key admitted")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatal("recovery repeated a create or mutated backing")
	}
}

func mustBootstrapJSON(t *testing.T, b functions.Bootstrap) []byte {
	t.Helper()
	v, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
