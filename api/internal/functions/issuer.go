package functions

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"time"
)

// GuestIssuer is Worker-only. Its signing key must come from a dedicated,
// ownership-verified platform Secret, never from a deployment or customer env.
// The public API needs an artifact-server leaf, not this root private key.
type GuestIssuer struct {
	certificate *x509.Certificate
	key         crypto.Signer
	ca          string
}

// GuestIdentity can be serialized as public certificate metadata. Only the
// immutable root-only bootstrap Secret may receive PrivateKey; JSON omits it.
type GuestIdentity struct {
	Certificate string    `json:"certificate"`
	CA          string    `json:"ca"`
	PrivateKey  string    `json:"-"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func NewGuestIssuer(caPEM, keyPEM []byte, now time.Time) (*GuestIssuer, error) {
	invalid := errors.New("dedicated valid Functions root CA and matching strong signing key required")
	if len(caPEM) == 0 || len(caPEM) > 32<<10 || len(keyPEM) == 0 || len(keyPEM) > 32<<10 {
		return nil, invalid
	}
	block, rest := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, invalid
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageCertSign == 0 || now.Before(certificate.NotBefore) || !certificate.NotAfter.After(now.Add(30*time.Minute)) || certificate.CheckSignatureFrom(certificate) != nil {
		return nil, invalid
	}
	block, rest = pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) != 0 {
		return nil, invalid
	}
	private, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, invalid
	}
	signer, ok := private.(crypto.Signer)
	if !ok {
		return nil, invalid
	}
	switch key := signer.(type) {
	case *ecdsa.PrivateKey:
		if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() && key.Curve != elliptic.P521() {
			return nil, invalid
		}
	case *rsa.PrivateKey:
		if key.N.BitLen() < 3072 || key.Validate() != nil {
			return nil, invalid
		}
	case ed25519.PrivateKey:
		if len(key) != ed25519.PrivateKeySize {
			return nil, invalid
		}
	default:
		return nil, invalid
	}
	public, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil || !bytes.Equal(public, certificate.RawSubjectPublicKeyInfo) {
		return nil, invalid
	}
	return &GuestIssuer{certificate: certificate, key: signer, ca: string(caPEM)}, nil
}

// Issue creates a fresh P-256 leaf/key for exactly one immutable instance.
// Expiry is bounded by both the root and a four-hour maximum. Expired guests
// must be drained/replaced; this function never renews a running boot in place.
func (issuer *GuestIssuer) Issue(scope Scope, now time.Time, lifetime time.Duration) (GuestIdentity, error) {
	var result GuestIdentity
	if issuer == nil || issuer.certificate == nil || issuer.key == nil || scope.Validate() != nil || lifetime < 30*time.Minute || lifetime > 4*time.Hour || now.Before(issuer.certificate.NotBefore) || !issuer.certificate.NotAfter.After(now.Add(lifetime)) {
		return result, errors.New("invalid Functions leaf scope or bounded lifetime")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return result, errors.New("Functions leaf key unavailable")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return result, errors.New("Functions leaf serial unavailable")
	}
	// ASN.1 allows zero, but certificates should have a strictly positive serial.
	serial.Add(serial, big.NewInt(1))
	name := scope.InstanceID + ".functions.neon.internal"
	notBefore := now.Add(-time.Minute)
	if notBefore.Before(issuer.certificate.NotBefore) {
		notBefore = issuer.certificate.NotBefore
	}
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: notBefore, NotAfter: now.Add(lifetime), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, issuer.certificate, &key.PublicKey, issuer.key)
	if err != nil {
		return result, errors.New("Functions leaf signature unavailable")
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return result, errors.New("Functions leaf key encoding unavailable")
	}
	result = GuestIdentity{Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), CA: issuer.ca, PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})), ExpiresAt: certificate.NotAfter}
	return result, nil
}
