package control

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// This is operator-owned platform input, never a deployment parameter. Public
// API mounts the common config/MAC and public CAs; only Worker mounts the root
// signing key and artifact server leaf key. No customer code runs in Worker.
type functionsPlatformInput struct {
	Version                 int    `json:"version"`
	KeyVersion              string `json:"key_version"`
	MACKeyFile              string `json:"mac_key_file"`
	VMImage                 string `json:"vm_image"`
	NodeName                string `json:"node_name"`
	ProxyAddress            string `json:"proxy_address"`
	ProxyHostname           string `json:"proxy_hostname"`
	ProxyCAFile             string `json:"proxy_ca_file"`
	DNSAddress              string `json:"dns_address"`
	ArtifactOrigin          string `json:"artifact_origin"`
	ArtifactBind            string `json:"artifact_bind"`
	GuestCAFile             string `json:"guest_ca_file"`
	GuestKeyFile            string `json:"guest_key_file"`
	ArtifactCertificateFile string `json:"artifact_certificate_file"`
	ArtifactKeyFile         string `json:"artifact_key_file"`
}

type functionsPlatform struct {
	input            functionsPlatformInput
	key              []byte
	proxy            netip.AddrPort
	dns              netip.Addr
	proxyCA, guestCA string
	issuer           *functions.GuestIssuer
	artifactTLS      *tls.Config
}

var errFunctionsPlatform = errors.New("Functions private platform configuration or trust rejected")

func functionPrivateFile(path string, maximum int) ([]byte, error) {
	if path == "" || len(path) > 1024 {
		return nil, errFunctionsPlatform
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > int64(maximum) {
		return nil, errFunctionsPlatform
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maximum {
		return nil, errFunctionsPlatform
	}
	return data, nil
}

func loadFunctionsPlatform(role processRole) (*functionsPlatform, error) {
	if os.Getenv("NEON_FUNCTIONS_PREVIEW_ENABLED") != "true" {
		return nil, nil
	}
	if role != roleAPI && role != roleWorker {
		return nil, errFunctionsPlatform
	}
	raw, err := functionPrivateFile(os.Getenv("NEON_FUNCTIONS_CONFIG_FILE"), 64<<10)
	if err != nil {
		return nil, errFunctionsPlatform
	}
	var input functionsPlatformInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Version != 1 || !functionMACKeyVersion.MatchString(input.KeyVersion) ||
		!functionVMImage.MatchString(input.VMImage) || (input.NodeName != "" && !functionNodeName.MatchString(input.NodeName)) {
		return nil, errFunctionsPlatform
	}
	// Re-marshal/decode does not permit a trailing second JSON value.
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errFunctionsPlatform
	}
	p := &functionsPlatform{input: input}
	p.proxy, err = netip.ParseAddrPort(input.ProxyAddress)
	if err != nil || !p.proxy.Addr().Is4() || !p.proxy.Addr().IsPrivate() || p.proxy.Port() == 0 || input.ProxyHostname == "" || strings.ContainsAny(input.ProxyHostname, "/ :\r\n") {
		return nil, errFunctionsPlatform
	}
	p.dns, err = netip.ParseAddr(input.DNSAddress)
	if err != nil || !p.dns.Is4() || !p.dns.IsPrivate() {
		return nil, errFunctionsPlatform
	}
	origin, err := url.Parse(input.ArtifactOrigin)
	if err != nil || origin.Scheme != "https" || origin.User != nil || origin.Hostname() == "" || origin.Port() != "8790" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, errFunctionsPlatform
	}
	bindHost, bindPort, err := net.SplitHostPort(input.ArtifactBind)
	if err != nil || bindPort != "8790" || (bindHost != "" && bindHost != "0.0.0.0") {
		return nil, errFunctionsPlatform
	}
	key, err := functionPrivateFile(input.MACKeyFile, 256)
	if err != nil {
		return nil, errFunctionsPlatform
	}
	p.key, err = base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(key)))
	if err != nil || len(p.key) != 32 {
		return nil, errFunctionsPlatform
	}
	proxyCA, err := functionPrivateFile(input.ProxyCAFile, 32<<10)
	if err != nil {
		return nil, errFunctionsPlatform
	}
	p.proxyCA = string(proxyCA)
	guestCA, err := functionPrivateFile(input.GuestCAFile, 32<<10)
	if err != nil {
		return nil, errFunctionsPlatform
	}
	p.guestCA = string(guestCA)
	if role == roleWorker {
		issuerKey, err := functionPrivateFile(input.GuestKeyFile, 32<<10)
		if err != nil {
			return nil, errFunctionsPlatform
		}
		p.issuer, err = functions.NewGuestIssuer(guestCA, issuerKey, time.Now())
		if err != nil {
			return nil, errFunctionsPlatform
		}
		certificatePEM, err := functionPrivateFile(input.ArtifactCertificateFile, 32<<10)
		if err != nil {
			return nil, errFunctionsPlatform
		}
		leafKey, err := functionPrivateFile(input.ArtifactKeyFile, 32<<10)
		if err != nil {
			return nil, errFunctionsPlatform
		}
		certificate, err := tls.X509KeyPair(certificatePEM, leafKey)
		if err != nil || len(certificate.Certificate) != 1 {
			return nil, errFunctionsPlatform
		}
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil || leaf.IsCA {
			return nil, errFunctionsPlatform
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(guestCA) {
			return nil, errFunctionsPlatform
		}
		if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: origin.Hostname(), CurrentTime: time.Now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return nil, errFunctionsPlatform
		}
		p.artifactTLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}
	}
	return p, nil
}
