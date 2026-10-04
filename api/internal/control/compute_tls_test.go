package control

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
)

func TestComputeCertificateHasStrictChainAndEndpointIsolation(t *testing.T) {
	endpoint := "ep_0123456789abcdef"
	ca, cert, key, err := newComputeCertificate(endpoint, "neon", "192.168.146.100")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(ca)
	parent, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(cert)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.SubjectKeyId) == 0 || len(leaf.AuthorityKeyId) == 0 || string(parent.SubjectKeyId) != string(leaf.AuthorityKeyId) {
		t.Fatal("missing chain key identifiers")
	}
	values := map[string]string{"controlCA": base64.StdEncoding.EncodeToString(ca), "controlCert": base64.StdEncoding.EncodeToString(cert), "controlTLSKey": base64.StdEncoding.EncodeToString(key)}
	if !validComputeCertificate(values, endpoint, "neon") {
		t.Fatal("valid endpoint certificate rejected")
	}
	if validComputeCertificate(values, "ep_fedcba9876543210", "neon") {
		t.Fatal("foreign endpoint accepted")
	}
	leaf.AuthorityKeyId = nil
	// Legacy certificates lacking AKI must trigger a controlled renewal.
	values["controlCert"] = "invalid"
	if validComputeCertificate(values, endpoint, "neon") {
		t.Fatal("broken certificate accepted")
	}
}
