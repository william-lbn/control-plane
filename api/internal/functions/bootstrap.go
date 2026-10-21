package functions

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxBootstrapBytes = 128 << 10

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var sqlRolePattern = regexp.MustCompile(`^fn_[a-f0-9]{16}$`)
var writerEndpointPattern = regexp.MustCompile(`^ep_[a-f0-9]{16}$`)

func validDNSHostname(value string) bool {
	if len(value) > 253 || strings.ToLower(value) != value {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return !netipParseOK(value)
}

// Bootstrap is a root-only, immutable instance contract. It is read from the
// dedicated Secret CD-ROM, never inherited by Node or used as CLI arguments.
// Each trust anchor is public; all signing/SQL/leaf private keys stay in memory
// after bootstrap detach. ArtifactKey is independent of ManagerKey.
type Bootstrap struct {
	Scope              Scope             `json:"scope"`
	ManagerKey         string            `json:"manager_key"`
	ManagerCertificate string            `json:"manager_certificate"`
	ManagerPrivateKey  string            `json:"manager_private_key"`
	ManagerCA          string            `json:"manager_ca"`
	ArtifactURL        string            `json:"artifact_url"`
	ArtifactKey        string            `json:"artifact_key"`
	ArtifactCA         string            `json:"artifact_ca"`
	BundleDigest       string            `json:"bundle_digest"`
	DatabaseURL        string            `json:"database_url"`
	WriterEndpointID   string            `json:"writer_endpoint_id"`
	SQLCA              string            `json:"sql_ca"`
	ProxyIP            string            `json:"proxy_ip"`
	ProxyHostname      string            `json:"proxy_hostname"`
	ProxyPort          uint16            `json:"proxy_port"`
	DNSIP              string            `json:"dns_ip"`
	Environment        map[string]string `json:"environment"`
	AllowPublicHTTPS   bool              `json:"allow_public_https"`
}

func DecodeBootstrap(content []byte) (Bootstrap, error) {
	var b Bootstrap
	if len(content) == 0 || len(content) > MaxBootstrapBytes || strictJSON(content, &b) != nil {
		return b, errors.New("invalid bounded guest bootstrap")
	}
	if err := b.Validate(); err != nil {
		return Bootstrap{}, err
	}
	return b, nil
}

// DecodeRetainedBootstrap is for the trusted control plane's MAC/UID-verified
// immutable receipt, never Guest boot or new admission. Verify the original
// leaf at its signed issuance window; a live connection still independently
// requires a currently valid TLS leaf. Renewal never rewrites bootstrap.
func DecodeRetainedBootstrap(content []byte) (Bootstrap, error) {
	var b Bootstrap
	if len(content) == 0 || len(content) > MaxBootstrapBytes || strictJSON(content, &b) != nil {
		return Bootstrap{}, errors.New("invalid retained bootstrap")
	}
	pair, err := tls.X509KeyPair([]byte(b.ManagerCertificate), []byte(b.ManagerPrivateKey))
	if err != nil || len(pair.Certificate) == 0 {
		return Bootstrap{}, errors.New("invalid retained manager identity")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.NotBefore.After(time.Now()) || leaf.NotAfter.Sub(leaf.NotBefore) > 4*time.Hour+time.Minute {
		return Bootstrap{}, errors.New("invalid retained manager validity")
	}
	if err = b.validateAt(leaf.NotBefore.Add(time.Minute)); err != nil {
		return Bootstrap{}, err
	}
	return b, nil
}

func secretKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("independent 256-bit bootstrap keys required")
	}
	return key, nil
}

func trustPool(pem string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if len(pem) == 0 || len(pem) > 32<<10 || !pool.AppendCertsFromPEM([]byte(pem)) {
		return nil, errors.New("bounded explicit CA required")
	}
	return pool, nil
}

func (b Bootstrap) artifactPath() string {
	return "/internal/v1/function-artifacts/" + b.Scope.InstanceID + "/" + b.Scope.DeploymentID + "/" + b.BundleDigest
}

func (b Bootstrap) Validate() error {
	return b.validateAt(time.Now())
}

func (b Bootstrap) validateAt(at time.Time) error {
	if b.Scope.Validate() != nil || !digestPattern.MatchString(b.BundleDigest) || ValidateEnvironment(b.Environment) != nil {
		return errors.New("invalid guest deployment contract")
	}
	manager, err := secretKey(b.ManagerKey)
	if err != nil {
		return err
	}
	artifact, err := secretKey(b.ArtifactKey)
	if err != nil || string(manager) == string(artifact) {
		return errors.New("artifact and manager require independent keys")
	}
	proxy, proxyErr := netip.ParseAddr(b.ProxyIP)
	dns, dnsErr := netip.ParseAddr(b.DNSIP)
	if proxyErr != nil || dnsErr != nil || (GuestBoundary{ProxyIP: proxy, ProxyPort: b.ProxyPort, DNSIP: dns}).Validate() != nil {
		return errors.New("invalid guest network destinations")
	}
	if !validDNSHostname(b.ProxyHostname) {
		return errors.New("explicit SQL certificate hostname required")
	}
	u, err := url.Parse(b.ArtifactURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != b.artifactPath() || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("artifact URL must bind this immutable instance and digest over HTTPS")
	}
	if _, err = trustPool(b.ArtifactCA); err != nil {
		return err
	}
	if _, err = trustPool(b.SQLCA); err != nil {
		return err
	}
	u, err = url.Parse(b.DatabaseURL)
	if err != nil || u.Scheme != "postgresql" || u.Hostname() != b.ProxyHostname || u.Port() != strconv.Itoa(int(b.ProxyPort)) || u.User == nil || !sqlRolePattern.MatchString(u.User.Username()) || u.Path == "" || u.Path == "/" || u.Fragment != "" {
		return errors.New("branch restricted SQL URL required")
	}
	password, exists := u.User.Password()
	query, queryErr := url.ParseQuery(u.RawQuery)
	selector := strings.Replace(b.WriterEndpointID, "ep_", "ep-", 1)
	if queryErr != nil || !exists || len(password) < 24 || !writerEndpointPattern.MatchString(b.WriterEndpointID) || len(query) != 3 || query.Get("sslmode") != "verify-full" || query.Get("sslrootcert") != "/etc/neon-function/sql-ca.crt" || query.Get("options") != "endpoint="+selector || len(query["sslmode"]) != 1 || len(query["sslrootcert"]) != 1 || len(query["options"]) != 1 {
		return errors.New("restricted SQL requires fixed Writer selector and verify-full CA")
	}
	_, err = b.ManagerTLS(at)
	return err
}

func netipParseOK(value string) bool { _, err := netip.ParseAddr(value); return err == nil }

// ManagerTLS verifies chain, purpose, hostname and leaf key at boot. Serving a
// certificate without these checks would make the bootstrap's scope ambiguous.
func (b Bootstrap) ManagerTLS(now time.Time) (*tls.Config, error) {
	certificate, err := tls.X509KeyPair([]byte(b.ManagerCertificate), []byte(b.ManagerPrivateKey))
	if err != nil || len(certificate.Certificate) == 0 {
		return nil, errors.New("invalid manager TLS identity")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid manager TLS identity")
	}
	if leaf.IsCA || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != b.Scope.InstanceID+".functions.neon.internal" || len(leaf.IPAddresses) != 0 || len(leaf.URIs) != 0 || len(leaf.EmailAddresses) != 0 {
		return nil, errors.New("unique instance TLS SAN required")
	}
	roots, err := trustPool(b.ManagerCA)
	if err != nil {
		return nil, err
	}
	intermediates := x509.NewCertPool()
	for _, der := range certificate.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, errors.New("invalid manager TLS chain")
		}
		intermediates.AddCert(cert)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: b.Scope.InstanceID + ".functions.neon.internal", CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, errors.New("manager certificate does not verify for this instance")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}, nil
}

// ArtifactFetchError contains only fixed codes and HTTP status, never a URL,
// response body, bootstrap credential or underlying network error.
type ArtifactFetchError struct {
	Reason           string
	Status, Attempts int
}

func (e *ArtifactFetchError) Error() string { return "trusted artifact fetch failed: " + e.Reason }

func transientArtifactNetwork(err error) bool {
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	if errors.As(err, &certificate) || errors.As(err, &record) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTimeout || dns.IsTemporary
	}
	var operation *net.OpError
	var network net.Error
	return errors.As(err, &operation) || (errors.As(err, &network) && network.Timeout()) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// FetchBundle permits one exact HTTPS artifact, signs the exact path, refuses
// redirects/proxies/system CA fallbacks, bounds bytes and verifies archive hash
// before ZIP validation or any file creation. Only safe artifact GETs retry on
// known transient failures (three freshly signed attempts, 95 seconds total).
// No customer invocation, authentication rejection or integrity failure retries.
func (b Bootstrap) FetchBundle(ctx context.Context) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	roots, _ := trustPool(b.ArtifactCA)
	key, _ := secretKey(b.ArtifactKey)
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}, ResponseHeaderTimeout: 15 * time.Second, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(ctx, 95*time.Second)
	defer cancel()
	for attempt := 1; attempt <= 3; attempt++ {
		if ctx.Err() != nil {
			return nil, &ArtifactFetchError{Reason: "cancelled", Attempts: attempt - 1}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.ArtifactURL, nil)
		if err != nil || SignManagerRequest(request, nil, key, time.Now()) != nil {
			return nil, &ArtifactFetchError{Reason: "signing", Attempts: attempt}
		}
		response, err := client.Do(request)
		failure := &ArtifactFetchError{Reason: "network", Attempts: attempt}
		retry := false
		if err != nil {
			retry = transientArtifactNetwork(err)
			if !retry {
				failure.Reason = "transport_rejected"
			}
		} else {
			failure.Status = response.StatusCode
			if response.StatusCode != http.StatusOK {
				failure.Reason = "http_rejected"
				retry = response.StatusCode == 429 || response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				_ = response.Body.Close()
			} else {
				content, readErr := io.ReadAll(io.LimitReader(response.Body, MaxArchiveBytes+1))
				_ = response.Body.Close()
				if readErr != nil {
					failure.Reason, retry = "read_interrupted", transientArtifactNetwork(readErr)
				} else {
					if len(content) == 0 || len(content) > MaxArchiveBytes {
						return nil, &ArtifactFetchError{Reason: "size_rejected", Status: 200, Attempts: attempt}
					}
					digest := sha256.Sum256(content)
					if hex.EncodeToString(digest[:]) != b.BundleDigest {
						return nil, &ArtifactFetchError{Reason: "digest_rejected", Status: 200, Attempts: attempt}
					}
					if _, err := ValidateBundle(content); err != nil {
						return nil, &ArtifactFetchError{Reason: "archive_rejected", Status: 200, Attempts: attempt}
					}
					return content, nil
				}
			}
		}
		if ctx.Err() != nil {
			failure.Reason, retry = "cancelled", false
		}
		if !retry || attempt == 3 {
			return nil, failure
		}
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &ArtifactFetchError{Reason: "cancelled", Attempts: attempt}
		case <-timer.C:
		}
	}
	panic("bounded artifact attempts exhausted")
}
