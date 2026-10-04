package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"time"
)

func newComputeCertificate(endpoint, namespace, host string) (caPEM, certPEM, keyPEM []byte, err error) {
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "neon-compute-control/" + endpoint}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		return
	}
	// Parse the emitted CA to retain its generated SubjectKeyId. Passing the
	// original template as parent omits the leaf AuthorityKeyId and fails
	// strict RFC 5280 verification (for example Python 3.13/OpenSSL).
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		return
	}
	serial, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return
	}
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: kubeName(endpoint) + "-management"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 3, 0),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{kubeName(endpoint) + "-management." + namespace + ".svc", kubeName(endpoint) + "-management." + namespace + ".svc.cluster.local"}}
	if ip := net.ParseIP(host); ip != nil {
		leaf.IPAddresses = []net.IP{ip}
	} else {
		leaf.DNSNames = append(leaf.DNSNames, host)
	}
	certDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, public, caKey)
	if err != nil {
		return
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return
}

func (k *kubeClient) computeTLSIdentity(ctx context.Context, project, endpoint, host string) (map[string]string, error) {
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(endpoint)+"-credentials"), nil)
		if err != nil || !owned(secret, project, endpoint) {
			return nil, errors.New("compute TLS credential unavailable or unowned")
		}
		values := map[string]string{}
		for _, name := range []string{"controlCA", "controlCert", "controlTLSKey"} {
			values[name] = stringVal(nested(secret, "data", name))
		}
		if validComputeCertificate(values, endpoint, k.namespace) {
			return values, nil
		}
		ca, cert, key, err := newComputeCertificate(endpoint, k.namespace, host)
		if err != nil {
			return nil, err
		}
		for name, b := range map[string][]byte{"controlCA": ca, "controlCert": cert, "controlTLSKey": key} {
			values[name] = base64.StdEncoding.EncodeToString(b)
			secret["data"].(map[string]any)[name] = values[name]
		}
		_, err = k.request(ctx, http.MethodPut, k.path("secret", kubeName(endpoint)+"-credentials"), secret)
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == 409 {
			continue
		}
		if err != nil {
			return nil, err
		}
		return values, nil
	}
	return nil, errors.New("compute TLS identity reservation conflict")
}

func validComputeCertificate(values map[string]string, endpoint, namespace string) bool {
	ca, err := base64.StdEncoding.DecodeString(values["controlCA"])
	if err != nil {
		return false
	}
	cert, err := base64.StdEncoding.DecodeString(values["controlCert"])
	if err != nil {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(values["controlTLSKey"])
	if err != nil {
		return false
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || len(leaf.AuthorityKeyId) == 0 || time.Until(leaf.NotAfter) < 24*time.Hour {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return false
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: kubeName(endpoint) + "-management." + namespace + ".svc"})
	return err == nil
}
func installComputeTLS(config map[string]any) {
	ctl := config["compute_ctl_config"].(map[string]any)
	ctl["tls"] = map[string]string{"key_path": "/run/neon-lab/control.key", "cert_path": "/run/neon-lab/control.crt"}
}
func (s *server) nativeComputeRequest(ctx context.Context, p catalogPayload, config map[string]any, key ed25519.PrivateKey) error {
	name := kubeName(p.EndpointID) + "-management"
	service, err := s.kube.request(ctx, http.MethodGet, s.kube.path("service", name), nil)
	if err != nil {
		return err
	}
	if !owned(service, p.ProjectID, p.EndpointID) {
		return errors.New("compute management Service ownership mismatch")
	}
	gateway := env("NEON_COMPUTE_GATEWAY_URL", "https://"+s.proxyHost+":30480")
	if s.kube.token != "" {
		gateway = env("NEON_COMPUTE_GATEWAY_URL", "https://neon-compute-management-gateway."+s.kube.namespace+".svc:8443")
	}
	credential, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(p.EndpointID)+"-credentials"), nil)
	if err != nil {
		return err
	}
	ca, err := secretText(credential, "controlCA")
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ca)) {
		return errors.New("invalid compute control CA")
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway+"/computes/"+kubeName(p.EndpointID)+"/configure", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+computeJWT(key, p.EndpointID, time.Now()))
	if request.URL.Scheme != "https" || request.URL.User != nil {
		return errors.New("compute gateway requires HTTPS without URL credentials")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: name + "." + s.kube.namespace + ".svc"}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 35 * time.Second, Transport: transport, CheckRedirect: func(r *http.Request, via []*http.Request) error { return errors.New("native compute redirect refused") }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// Never propagate native errors: they may include config/SQL/credentials.
	if response.StatusCode != 200 {
		return kubeError{Status: response.StatusCode, Message: "Native compute configuration rejected"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &result) != nil || result.Status != "running" {
		return errors.New("native compute did not confirm running status")
	}
	return nil
}
