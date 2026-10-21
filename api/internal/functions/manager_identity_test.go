package functions

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func renewalIssuer(t *testing.T) *GuestIssuer {
	t.Helper()
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "renewal-disposable"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewGuestIssuer(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), now)
	if err != nil {
		t.Fatal(err)
	}
	return issuer
}

func renewalPeer(t *testing.T, lost *atomic.Bool) (*InstanceClient, *Manager, Bootstrap, *GuestIssuer) {
	t.Helper()
	b := bootstrapFixture(t)
	issuer := renewalIssuer(t)
	leaf, err := issuer.Issue(b.Scope, time.Now().Add(-90*time.Minute), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b.ManagerCA, b.ManagerCertificate, b.ManagerPrivateKey = leaf.CA, leaf.Certificate, leaf.PrivateKey
	node := httptest.NewServer(http.HandlerFunc(readyNode))
	t.Cleanup(node.Close)
	key, _ := secretKey(b.ManagerKey)
	m, err := NewManager(b.Scope, key, node.URL, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	config, err := m.EnableIdentityRenewal(b, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/internal/v1/manager-identity" && lost != nil && lost.Swap(false) {
			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, r)
			if rec.Code != 204 {
				t.Error("lost reply fixture did not commit update")
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		m.ServeHTTP(w, r)
	}))
	server.TLS = config
	server.StartTLS()
	t.Cleanup(server.Close)
	c, err := NewInstanceClient(b.Scope, netip.MustParseAddrPort("127.0.0.1:9090"), key, b.ManagerCA)
	if err != nil {
		t.Fatal(err)
	}
	c.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	t.Cleanup(c.Close)
	return c, m, b, issuer
}

func renewalUpdate(t *testing.T, m *Manager, issuer *GuestIssuer, old ManagerIdentity) ManagerIdentityUpdate {
	t.Helper()
	leaf, err := issuer.Issue(m.scope, time.Now(), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return ManagerIdentityUpdate{Scope: m.scope, BootID: m.bootID, PreviousFingerprint: old.Fingerprint, Certificate: leaf.Certificate, PrivateKey: leaf.PrivateKey}
}

func TestManagerIdentityRenewalNewHandshakeSameBootAndChild(t *testing.T) {
	c, m, b, issuer := renewalPeer(t, nil)
	ctx := context.Background()
	old, err := c.ManagerIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cap, err := c.Capabilities(ctx)
	if err != nil || !cap.ManagerIdentityRenewal || cap.BootID != old.BootID {
		t.Fatal("renewal capability missing", err)
	}
	update := renewalUpdate(t, m, issuer, old)
	if err = c.RenewManagerIdentity(ctx, update); err != nil {
		t.Fatal(err)
	}
	after, err := c.ManagerIdentity(ctx)
	if err != nil || after.BootID != old.BootID || after.Scope != old.Scope || after.Renewals != 1 || after.Fingerprint != ManagerCertificateFingerprint(update.Certificate) || !after.NotAfter.After(old.NotAfter) {
		t.Fatal("new handshake or original boot rejected", err)
	}
	if err = c.RenewManagerIdentity(ctx, update); err != nil || m.identity.current().Renewals != 1 {
		t.Fatal("same committed update replay changed identity", err)
	}
	observed, err := c.Status(ctx)
	if err != nil || observed.BootID != status.BootID || !observed.LastActivity.Equal(status.LastActivity) || !observed.NodeReady {
		t.Fatal("certificate refresh executed or replaced child", err)
	}
	// Advance only the isolated client's verification clock beyond the original
	// leaf expiry. The new handshake still verifies; boot decoder stays strict.
	future := time.Now().Add(45 * time.Minute)
	c.transport.TLSClientConfig.Time = func() time.Time { return future }
	if _, err = c.ManagerIdentity(ctx); err != nil {
		t.Fatal("new leaf failed after original expiry", err)
	}
	if _, err = b.ManagerTLS(future); err == nil {
		t.Fatal("original expired boot certificate accepted")
	}
	r, err := c.Invoke(ctx, invocationFor(m))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("same child invocation failed after refresh")
	}
}

func TestManagerIdentityRenewalLostReplyAndRejection(t *testing.T) {
	var lost atomic.Bool
	c, m, bootstrap, issuer := renewalPeer(t, &lost)
	ctx := context.Background()
	old, err := c.ManagerIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"boot", "scope", "previous", "foreign-root", "foreign-san", "wrong-key", "same-key", "expired", "long-lived"} {
		t.Run(kind, func(t *testing.T) {
			u := renewalUpdate(t, m, issuer, old)
			switch kind {
			case "boot":
				u.BootID = strings.Repeat("e", 32)
			case "scope":
				u.Scope.Generation++
			case "previous":
				u.PreviousFingerprint = strings.Repeat("e", 64)
			case "foreign-root":
				other := renewalUpdate(t, m, renewalIssuer(t), old)
				u.Certificate, u.PrivateKey = other.Certificate, other.PrivateKey
			case "foreign-san":
				scope := m.scope
				scope.InstanceID = "fni_0000000000000002"
				leaf, e := issuer.Issue(scope, time.Now(), 2*time.Hour)
				if e != nil {
					t.Fatal(e)
				}
				u.Certificate, u.PrivateKey = leaf.Certificate, leaf.PrivateKey
			case "same-key":
				pair, e := tls.X509KeyPair([]byte(bootstrap.ManagerCertificate), []byte(bootstrap.ManagerPrivateKey))
				if e != nil {
					t.Fatal(e)
				}
				leaf := &x509.Certificate{SerialNumber: big.NewInt(66), DNSNames: []string{m.scope.InstanceID + ".functions.neon.internal"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(2 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
				der, e := x509.CreateCertificate(rand.Reader, leaf, issuer.certificate, pair.PrivateKey.(crypto.Signer).Public(), issuer.key)
				if e != nil {
					t.Fatal(e)
				}
				u.Certificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
				u.PrivateKey = bootstrap.ManagerPrivateKey
			case "wrong-key":
				other := renewalUpdate(t, m, issuer, old)
				u.PrivateKey = other.PrivateKey
			case "expired":
				leaf, e := issuer.Issue(m.scope, time.Now().Add(-3*time.Hour), 2*time.Hour)
				if e != nil {
					t.Fatal(e)
				}
				u.Certificate, u.PrivateKey = leaf.Certificate, leaf.PrivateKey
			case "long-lived":
				leaf, e := issuer.Issue(m.scope, time.Now(), 4*time.Hour)
				if e != nil {
					t.Fatal(e)
				}
				u.Certificate, u.PrivateKey = leaf.Certificate, leaf.PrivateKey
			}
			if c.RenewManagerIdentity(ctx, u) == nil || m.identity.current() != old {
				t.Fatal("invalid renewal changed original identity")
			}
		})
	}
	u := renewalUpdate(t, m, issuer, old)
	lost.Store(true)
	if c.RenewManagerIdentity(ctx, u) == nil {
		t.Fatal("lost acknowledgement was reported as acknowledged")
	}
	after, err := c.ManagerIdentity(ctx)
	if err != nil || after.Renewals != 1 || after.Fingerprint != ManagerCertificateFingerprint(u.Certificate) || after.BootID != old.BootID {
		t.Fatal("unknown result could not be observed on original boot", err)
	}
	if c.RenewManagerIdentity(ctx, u) != nil || m.identity.current().Renewals != 1 {
		t.Fatal("replay after unknown result changed identity")
	}
	stale := renewalUpdate(t, m, issuer, old)
	if c.RenewManagerIdentity(ctx, stale) == nil {
		t.Fatal("stale fingerprint replaced current leaf")
	}
	m.mu.Lock()
	m.draining = true
	m.mu.Unlock()
	if c.RenewManagerIdentity(ctx, renewalUpdate(t, m, issuer, after)) == nil {
		t.Fatal("draining instance accepted new identity")
	}
}

func TestManagerIdentityRetainedBootstrapNeverAdmitsExpiredBoot(t *testing.T) {
	b := bootstrapFixture(t)
	issuer := renewalIssuer(t)
	leaf, err := issuer.Issue(b.Scope, time.Now().Add(-3*time.Hour), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b.ManagerCA, b.ManagerCertificate, b.ManagerPrivateKey = leaf.CA, leaf.Certificate, leaf.PrivateKey
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeBootstrap(raw); err == nil {
		t.Fatal("expired bootstrap admitted a new Guest")
	}
	retained, err := DecodeRetainedBootstrap(raw)
	if err != nil || retained.ManagerCertificate != b.ManagerCertificate || retained.ManagerKey != b.ManagerKey {
		t.Fatal("retained immutable identity lost", err)
	}
	if _, err = retained.ManagerTLS(time.Now()); err == nil {
		t.Fatal("archival decode bypassed live TLS validity")
	}
	b.Scope.InstanceID = "fni_0000000000000002"
	raw, _ = json.Marshal(b)
	if _, err = DecodeRetainedBootstrap(raw); err == nil {
		t.Fatal("retained foreign SAN accepted")
	}
}
