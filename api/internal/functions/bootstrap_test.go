package functions

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func bootstrapFixture(t *testing.T) Bootstrap {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable-ci"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	anchor := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"fni_0000000000000001.functions.neon.internal"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err = x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	scope := Scope{InstanceID: "fni_0000000000000001", DeploymentID: "fdp_0000000000000002", ProjectID: "prj_0000000000000003", BranchID: "br_0000000000000004", Slug: "hello", Generation: 1}
	b := Bootstrap{Scope: scope, ManagerKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{12}, 32)), ArtifactKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{13}, 32)), ManagerCA: anchor, SQLCA: anchor, ArtifactCA: anchor, ManagerCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), ManagerPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE" + " KEY", Bytes: private})), BundleDigest: digest, ProxyIP: "192.0.2.2", ProxyHostname: "sql.example.test", ProxyPort: 5432, DNSIP: "192.0.2.53", DatabaseURL: "postgresql://fn_0000000000000005:012345678901234567890123456789@sql.example.test:5432/neondb?sslmode=verify-full&sslrootcert=%2Fetc%2Fneon-function%2Fsql-ca.crt", Environment: map[string]string{}}
	b.ArtifactURL = "https://artifact.example.test" + b.artifactPath()
	return b
}

func TestBootstrapRejectsCredentialScopeAndTrustChanges(t *testing.T) {
	b := bootstrapFixture(t)
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Bootstrap){
		"instance":     func(v *Bootstrap) { v.Scope.InstanceID = "fni_0000000000000009" },
		"untrusted-ca": func(v *Bootstrap) { v.ManagerCA = bootstrapFixture(t).ManagerCA },
		"same-key":     func(v *Bootstrap) { v.ArtifactKey = v.ManagerKey },
		"sql-superuser": func(v *Bootstrap) {
			v.DatabaseURL = strings.Replace(v.DatabaseURL, "fn_0000000000000005", "cloud_admin", 1)
		},
		"sql-no-verify":        func(v *Bootstrap) { v.DatabaseURL = strings.Replace(v.DatabaseURL, "verify-full", "require", 1) },
		"sql-other-target":     func(v *Bootstrap) { v.ProxyHostname = "another.example.test" },
		"sql-host-injection":   func(v *Bootstrap) { v.ProxyHostname = "sql.example.test\n127.0.0.1 injected" },
		"sql-host-empty-label": func(v *Bootstrap) { v.ProxyHostname = "sql..example.test" },
		"sql-query-option":     func(v *Bootstrap) { v.DatabaseURL += "&options=-csearch_path=public" },
		"sql-malformed-query":  func(v *Bootstrap) { v.DatabaseURL += "&%zz=bad" },
		"artifact-http":        func(v *Bootstrap) { v.ArtifactURL = strings.Replace(v.ArtifactURL, "https:", "http:", 1) },
		"artifact-cross-instance": func(v *Bootstrap) {
			v.ArtifactURL = strings.Replace(v.ArtifactURL, "fni_0000000000000001", "fni_0000000000000009", 1)
		},
		"artifact-url-key": func(v *Bootstrap) { v.ArtifactURL += "?token=private" },
		"bad-environment":  func(v *Bootstrap) { v.Environment = map[string]string{"DATABASE_URL": "customer-override-secret"} },
	} {
		t.Run(name, func(t *testing.T) {
			value := b
			change(&value)
			if err := value.Validate(); err == nil || strings.Contains(err.Error(), "customer-override-secret") {
				t.Fatal("unsafe bootstrap accepted or value leaked")
			}
		})
	}
	if _, err := b.ManagerTLS(time.Now().Add(2 * time.Hour)); err == nil {
		t.Fatal("expired identity accepted")
	}
	wrongInstance := b
	wrongInstance.Scope.InstanceID = "fni_0000000000000009"
	if _, err := wrongInstance.ManagerTLS(time.Now()); err == nil {
		t.Fatal("manager TLS identity accepted for another instance")
	}
	encoded, _ := json.Marshal(b)
	if _, err := DecodeBootstrap(encoded); err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded[:len(encoded)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeBootstrap(encoded); err == nil {
		t.Fatal("unknown bootstrap field accepted")
	}
	if _, err := DecodeBootstrap(bytes.Repeat([]byte("x"), MaxBootstrapBytes+1)); err == nil {
		t.Fatal("unbounded bootstrap accepted")
	}
}

func artifactFixture(t *testing.T) (Bootstrap, []byte, *httptest.Server) {
	t.Helper()
	b := bootstrapFixture(t)
	archive := archiveFor(t, "index.mjs")
	bundle, err := ValidateBundle(archive)
	if err != nil {
		t.Fatal(err)
	}
	b.BundleDigest = bundle.Digest
	key, _ := secretKey(b.ArtifactKey)
	handler, err := NewArtifactHandler(b.Scope, key, b.BundleDigest, archive)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	server.TLS.MinVersion = tls.VersionTLS13
	t.Cleanup(server.Close)
	b.ArtifactURL = server.URL + b.artifactPath()
	b.ArtifactCA = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	return b, archive, server
}

func TestTrustedArtifactFetchRejectsTamperRedirectAndReplay(t *testing.T) {
	b, archive, server := artifactFixture(t)
	actual, err := b.FetchBundle(context.Background())
	if err != nil || !bytes.Equal(actual, archive) {
		t.Fatal("trusted artifact fetch failed", err)
	}
	// Handler copies immutable bytes; mutation of the constructor input cannot
	// swap code after validation while preserving its expected deployment hash.
	archive[0] ^= 1
	if _, err := b.FetchBundle(context.Background()); err != nil {
		t.Fatal(err)
	}
	key, _ := secretKey(b.ArtifactKey)
	request, _ := http.NewRequest(http.MethodGet, b.ArtifactURL, nil)
	if err := SignManagerRequest(request, nil, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("artifact authentication replay accepted")
	}
	wrong := b
	wrong.ArtifactCA = bootstrapFixture(t).ArtifactCA
	if _, err = wrong.FetchBundle(context.Background()); err == nil {
		t.Fatal("foreign artifact CA accepted")
	}
	key[0] ^= 1
	wrong = b
	wrong.ArtifactKey = base64.StdEncoding.EncodeToString(key)
	if _, err = wrong.FetchBundle(context.Background()); err == nil {
		t.Fatal("wrong artifact credential accepted")
	}
	// Same trusted peer, wrong bytes/hash or redirects: no second host follows.
	for name, handler := range map[string]http.HandlerFunc{
		"tamper": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("different artifact")) },
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://must-not-follow.example.test/private", 302)
		},
		"overflow": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), MaxArchiveBytes+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			peer := httptest.NewTLSServer(handler)
			defer peer.Close()
			value := b
			value.ArtifactURL = peer.URL + value.artifactPath()
			value.ArtifactCA = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))
			if _, err := value.FetchBundle(context.Background()); err == nil {
				t.Fatal("unsafe artifact accepted")
			}
		})
	}
	if _, err := NewArtifactHandler(b.Scope, key, hex.EncodeToString(bytes.Repeat([]byte{1}, 32)), archive); err == nil {
		t.Fatal("bad artifact constructor accepted")
	}
}
