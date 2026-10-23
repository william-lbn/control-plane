package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func dsgaStorageProfileFixture() dsgaStorageProfile {
	return dsgaStorageProfile{1, "storage-controller", "storage-service-uid", 1234, "storage-management", "storage-secret-uid"}
}

// 仅为本地协议测试签发临时EdDSA token/证书，不把测试密钥用于平台供给。
// 证书只含平台Service DNS；测试改Dial目的地仍保留真实SNI、CA与主机名核验，
// 不用InsecureSkipVerify。这里不模拟K8s路由/RBAC，也不声称实际存储可用。
func dsgaStorageTestObjects(t *testing.T) (*core.Service, *core.Secret, tls.Certificate) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"storage-controller.neon.svc"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	read, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "scrubber"}).SignedString(private)
	if err != nil {
		t.Fatal(err)
	}
	write, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "pageserverapi"}).SignedString(private)
	if err != nil {
		t.Fatal(err)
	}
	p := dsgaStorageProfileFixture()
	service := &core.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "neon", Name: p.ServiceName, UID: types.UID(p.ServiceUID), ResourceVersion: "17"}, Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, ClusterIP: "10.66.0.7", Ports: []core.ServicePort{{Port: int32(p.Port), Protocol: core.ProtocolTCP}}}}
	secret := &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "neon", Name: p.CredentialSecretName, UID: types.UID(p.CredentialSecretUID), ResourceVersion: "18"}, Type: core.SecretTypeOpaque, Data: map[string][]byte{"observe_token": []byte(read), "create_token": []byte(write), "public_key": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: keyDER}), "ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}}
	return service, secret, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
}

func dsgaStorageTestClient(t *testing.T, handler http.Handler) *dsgaStorageClient {
	t.Helper()
	service, secret, cert := dsgaStorageTestObjects(t)
	c, err := dsgaStorageClientSnapshot("neon", dsgaStorageProfileFixture(), service, secret)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(handler)
	s.Config.ErrorLog = nil
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	t.Cleanup(s.Close)
	transport := c.http.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "10.66.0.7:1234" {
			return nil, errors.New("unexpected pinned address")
		}
		return (&net.Dialer{}).DialContext(ctx, network, s.Listener.Addr().String())
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func TestDSGAStorageNativeScopesStrictClaimsAndSignature(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signed := func(method jwt.SigningMethod, claims jwt.MapClaims, key any) string {
		t.Helper()
		value, err := jwt.NewWithClaims(method, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	read := signed(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "scrubber"}, private)
	write := signed(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "pageserverapi"}, private)
	admin := signed(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "admin"}, private)
	if !validDSGAStorageToken(read, public, "scrubber") || !validDSGAStorageToken(write, public, "pageserverapi") || !validDSGAStorageToken(admin, public, "scrubber") || !validDSGAStorageToken(admin, public, "pageserverapi") {
		t.Fatal("official native scopes rejected")
	}
	if validDSGAStorageToken(read, public, "pageserverapi") || validDSGAStorageToken(write, public, "scrubber") {
		t.Fatal("read/create authorities collapsed")
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"scope":"tenant","scope":"scrubber"}`))
	input := header + "." + payload
	signature, err := jwt.SigningMethodEdDSA.Sign(input, private)
	if err != nil {
		t.Fatal(err)
	}
	_, foreign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"duplicate_signed_scope": input + "." + base64.RawURLEncoding.EncodeToString(signature),
		"foreign_key":            signed(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "scrubber"}, foreign),
		"hmac_key_confusion":     signed(jwt.SigningMethodHS256, jwt.MapClaims{"scope": "scrubber"}, []byte(public)),
		"expired":                signed(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "scrubber", "exp": time.Now().Add(-time.Hour).Unix()}, private),
		"future":                 signed(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "scrubber", "nbf": time.Now().Add(time.Hour).Unix()}, private),
		"non_native_scope":       signed(jwt.SigningMethodEdDSA, jwt.MapClaims{"scope": "storage_controller_api"}, private),
	} {
		t.Run(name, func(t *testing.T) {
			if validDSGAStorageToken(value, public, "scrubber") {
				t.Fatal("invalid storage authority accepted")
			}
		})
	}
}

func TestDSGAStorageSnapshotRejectsRebindingAndCopiesCredentials(t *testing.T) {
	service, secret, _ := dsgaStorageTestObjects(t)
	p := dsgaStorageProfileFixture()
	c, err := dsgaStorageClientSnapshot("neon", p, service, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	for name, change := range map[string]func(*core.Service, *core.Secret){
		"service_uid":    func(s *core.Service, _ *core.Secret) { s.UID = "replacement-service" },
		"secret_uid":     func(_ *core.Service, s *core.Secret) { s.UID = "replacement-secret" },
		"public_service": func(s *core.Service, _ *core.Secret) { s.Spec.Type = core.ServiceTypeLoadBalancer },
		"headless":       func(s *core.Service, _ *core.Secret) { s.Spec.ClusterIP = core.ClusterIPNone },
		"scope_swap":     func(_ *core.Service, s *core.Secret) { s.Data["create_token"] = s.Data["observe_token"] },
		"untrusted_ca":   func(_ *core.Service, s *core.Secret) { s.Data["ca.crt"] = []byte("invalid") },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := service.DeepCopy(), secret.DeepCopy()
			change(a, b)
			got, err := dsgaStorageClientSnapshot("neon", p, a, b)
			if err == nil || got != nil {
				t.Fatal("invalid platform source accepted")
			}
		})
	}
	beforeRead, beforeWrite := c.readToken, c.writeToken
	for _, data := range secret.Data {
		clear(data)
	}
	if c.readToken != beforeRead || c.writeToken != beforeWrite || !validDSGAStorageToken(c.readToken, c.publicKey, "scrubber") {
		t.Fatal("Kube object mutation altered accepted snapshot")
	}
}

func TestDSGAStorageRequestUsesTLSNativeScopeAndNeverReplaysPOST(t *testing.T) {
	var gets, posts atomic.Int32
	var c *dsgaStorageClient
	c = dsgaStorageTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		scope, ok := storageRequestScope(r.Method, strings.TrimPrefix(r.URL.Path, "/"))
		if !ok || !validDSGAStorageToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), c.publicKey, scope) || r.TLS == nil || r.TLS.ServerName != "storage-controller.neon.svc" || r.Host != "10.66.0.7:1234" {
			t.Error("request escaped pinned TLS/native scope")
			w.WriteHeader(403)
			return
		}
		if r.Method == http.MethodPost {
			posts.Add(1)
			var body map[string]string
			if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || body["new_tenant_id"] != strings.Repeat("a", 32) {
				t.Error("original POST changed")
			}
			w.WriteHeader(504)
			_, _ = w.Write([]byte(`{"error":"unconfirmed"}`))
			return
		}
		gets.Add(1)
		_, _ = w.Write([]byte(`{"generation":18446744073709551615}`))
	}))
	ctx := context.Background()
	fact, err := c.request(ctx, http.MethodGet, "control/v1/tenant/"+strings.Repeat("a", 32), nil)
	if err != nil || fact["generation"] != json.Number("18446744073709551615") {
		t.Fatal("native read/uint64 precision failed", err)
	}
	_, err = c.request(ctx, http.MethodPost, "v1/tenant", map[string]string{"new_tenant_id": strings.Repeat("a", 32)})
	var status dsgaStorageHTTPError
	if !errors.As(err, &status) || status.code != 504 || posts.Load() != 1 {
		t.Fatal("unknown POST retried or declared success")
	}
	for _, path := range []string{"v1/tenant?host=other", "control/v1/tenant/" + strings.Repeat("a", 32) + "/../foreign", "control/v1/tenant/" + strings.Repeat("a", 32) + "%2fextra"} {
		if _, err := c.request(ctx, http.MethodPost, path, map[string]string{}); err == nil {
			t.Fatal("path escape accepted")
		}
	}
	// 已构造客户端中的凭据也可能随后过期；无效当前token不得发出任何HTTP。
	c.readToken = c.writeToken
	if _, err := c.request(ctx, http.MethodGet, "control/v1/tenant/"+strings.Repeat("a", 32), nil); err == nil || gets.Load() != 1 {
		t.Fatal("send-time authority check bypassed")
	}
}

func TestDSGAStorageResponseIsBoundedStrictJSONAndNoRedirect(t *testing.T) {
	cases := []struct {
		name                        string
		status                      int
		contentType, encoding, body string
	}{
		{"redirect", 302, "application/json", "", `{}`},
		{"wrong_media", 200, "text/plain", "", `{}`},
		{"encoding", 200, "application/json", "gzip", `{}`},
		{"duplicate", 200, "application/json", "", `{"tenant_id":"a","tenant_id":"b"}`},
		{"oversize", 200, "application/json", "", `{"x":"` + strings.Repeat("a", 1<<20) + `"}`},
		{"trailing", 200, "application/json", "", `{} {}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			c := dsgaStorageTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", test.contentType)
				if test.encoding != "" {
					w.Header().Set("Content-Encoding", test.encoding)
				}
				w.Header().Set("Location", "https://foreign.invalid/credential")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			if fact, err := c.request(context.Background(), http.MethodGet, "control/v1/tenant/"+strings.Repeat("a", 32), nil); err == nil || fact != nil || calls.Load() != 1 {
				t.Fatal("untrusted response accepted or redirect followed")
			}
		})
	}
}

func TestDSGAStorageProfileFrozenAndMissingHistoricalSourceRefused(t *testing.T) {
	p, policy, plan := dsgaPlanFixture(t)
	frozen := plan.Storage
	policy.Storage.ServiceUID = "changed-later"
	if plan.Storage != frozen || plan.Storage.validate(plan.Namespace) != nil {
		t.Fatal("original storage profile not frozen")
	}
	plan.Storage = dsgaStorageProfile{}
	if plan.validate(p) != nil {
		t.Fatal("historical plan no longer read-only decodable")
	}
	service, secret, _ := dsgaStorageTestObjects(t)
	if c, err := dsgaStorageClientSnapshot(plan.Namespace, plan.Storage, service, secret); err == nil || c != nil {
		t.Fatal("historical plan silently upgraded storage source")
	}
}
