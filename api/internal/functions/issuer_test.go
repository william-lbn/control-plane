package functions

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func issuerFixture(t *testing.T, curve elliptic.Curve, isCA bool, now time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "functions-test-root"}, IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(8 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}

func TestGuestIssuerFreshScopedLeafVerifiesAndOmitsPrivateKey(t *testing.T) {
	now := time.Now()
	ca, key := issuerFixture(t, elliptic.P256(), true, now)
	issuer, err := NewGuestIssuer(ca, key, now)
	if err != nil {
		t.Fatal(err)
	}
	b := bootstrapFixture(t)
	first, err := issuer.Issue(b.Scope, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := issuer.Issue(b.Scope, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first.PrivateKey == second.PrivateKey || first.Certificate == second.Certificate {
		t.Fatal("instance leaf reused private key or serial")
	}
	b.ManagerCA = first.CA
	b.ManagerCertificate = first.Certificate
	b.ManagerPrivateKey = first.PrivateKey
	if _, err := b.ManagerTLS(now); err != nil {
		t.Fatal("issued leaf cannot serve actual guest TLS", err)
	}
	b.Scope.InstanceID = "fni_0000000000000002"
	if _, err := b.ManagerTLS(now); err == nil {
		t.Fatal("leaf accepted for a different instance")
	}
	public, err := json.Marshal(first)
	if err != nil || bytes.Contains(public, []byte("PRIVATE KEY")) || bytes.Contains(public, []byte(first.PrivateKey)) {
		t.Fatal("identity JSON disclosed root-only private key")
	}
	if _, err := issuer.Issue(b.Scope, now, 5*time.Hour); err == nil {
		t.Fatal("unbounded leaf lifetime accepted")
	}
	if _, err := issuer.Issue(b.Scope, now.Add(9*time.Hour), time.Hour); err == nil {
		t.Fatal("expired root accepted")
	}
}

func TestGuestIssuerRejectsWeakForeignAndNonCARoots(t *testing.T) {
	now := time.Now()
	ca, key := issuerFixture(t, elliptic.P256(), true, now)
	otherCA, otherKey := issuerFixture(t, elliptic.P256(), true, now)
	weakCA, weakKey := issuerFixture(t, elliptic.P224(), true, now)
	leafCA, leafKey := issuerFixture(t, elliptic.P256(), false, now)
	for _, pair := range [][2][]byte{{ca, otherKey}, {otherCA, key}, {weakCA, weakKey}, {leafCA, leafKey}, {append(append([]byte(nil), ca...), otherCA...), key}, {ca, append(append([]byte(nil), key...), otherKey...)}, {ca, []byte("fixture-private-invalid-key")}} {
		if _, err := NewGuestIssuer(pair[0], pair[1], now); err == nil {
			t.Fatal("weak, foreign or ambiguous Functions root accepted")
		}
	}
	if _, err := NewGuestIssuer(ca, key, now.Add(9*time.Hour)); err == nil {
		t.Fatal("expired Functions root accepted")
	}
}
