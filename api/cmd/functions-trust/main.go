// functions-trust generates operator-owned Functions trust files on Linux.
// It never uploads keys, prints secret values or overwrites existing material.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

func run() error {
	output := flag.String("output-dir", "", "New private directory; must not already exist")
	host := flag.String("artifact-hostname", "neon-functions-artifact.neon.svc.cluster.local", "Private artifact TLS DNS name")
	flag.Parse()
	if *output == "" || !regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`).MatchString(*host) {
		return fmt.Errorf("valid output directory and artifact hostname required")
	}
	if err := os.Mkdir(*output, 0700); err != nil {
		return fmt.Errorf("new private output directory required")
	}
	write := func(name string, data []byte) error {
		f, err := os.OpenFile(filepath.Join(*output, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("trust file cannot be created")
		}
		defer f.Close()
		if _, err = f.Write(data); err != nil {
			return fmt.Errorf("trust file write failed")
		}
		return f.Sync()
	}
	serial := func() (*big.Int, error) {
		n, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if e == nil {
			n.Add(n, big.NewInt(1))
		}
		return n, e
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("trust key generation failed")
	}
	now := time.Now()
	rootSerial, err := serial()
	if err != nil {
		return fmt.Errorf("trust serial generation failed")
	}
	root := &x509.Certificate{SerialNumber: rootSerial, Subject: pkix.Name{CommonName: "Neon self-hosted Functions private CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(2, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("trust certificate generation failed")
	}
	rootKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("trust key encoding failed")
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("artifact key generation failed")
	}
	leafSerial, err := serial()
	if err != nil {
		return fmt.Errorf("artifact serial generation failed")
	}
	leaf := &x509.Certificate{SerialNumber: leafSerial, Subject: pkix.Name{CommonName: *host}, DNSNames: []string{*host}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 1, 0), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, key)
	if err != nil {
		return fmt.Errorf("artifact certificate generation failed")
	}
	private, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return fmt.Errorf("artifact key encoding failed")
	}
	mac := make([]byte, 32)
	if _, err = rand.Read(mac); err != nil {
		return fmt.Errorf("backing key generation failed")
	}
	files := map[string][]byte{
		"guest-ca.crt":     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}),
		"guest-ca.key":     pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rootKey}),
		"artifact-tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		"artifact-tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}),
		"backing-mac":      []byte(base64.StdEncoding.EncodeToString(mac) + "\n"),
	}
	for _, name := range []string{"guest-ca.crt", "guest-ca.key", "artifact-tls.crt", "artifact-tls.key", "backing-mac"} {
		if err = write(name, files[name]); err != nil {
			return err
		}
	}
	fmt.Println("Created five private trust files. Retain and back them up; do not regenerate for retries.")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
