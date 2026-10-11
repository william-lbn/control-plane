package functionsql

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func runtimeTLSFixture(t *testing.T, hostname string) (string, tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Disposable SQL fixture CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: hostname}, DNSNames: []string{hostname}, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * time.Minute)}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}))
	certificate, err := tls.X509KeyPair([]byte(leafPEM), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	return ca, certificate, leafPEM
}

func runtimePolicyFixture(t *testing.T) ConnectionPolicy {
	ca, _, _ := runtimeTLSFixture(t, "sql.example.test")
	return ConnectionPolicy{Scope: Scope{FunctionID: "fnc_0123456789abcdef", ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", Database: "neondb", Schema: "app", ProvisioningRole: "control_probe"},
		WriterID: "ep_0123456789abcdef", Target: netip.MustParseAddrPort("127.0.0.1:5432"), Hostname: "sql.example.test", CA: ca, Timeout: 10 * time.Second}
}

func TestFunctionsSQLConnectionPolicyIgnoresAmbient(t *testing.T) {
	for name, value := range map[string]string{"PGHOST": "foreign.example.test", "PGPORT": "1", "PGUSER": "cloud_admin", "PGPASSWORD": "foreign-password", "PGDATABASE": "other",
		"PGOPTIONS": "-c statement_timeout=1s -c search_path=public", "PGSSLMODE": "disable", "PGSSLROOTCERT": "/missing/foreign-ca", "PGSSLCERT": "/missing/foreign-cert", "PGSSLKEY": "/missing/foreign-key", "PGAPPNAME": "foreign",
		"PGSSLNEGOTIATION": "direct", "PGTARGETSESSIONATTRS": "read-only", "PGCONNECT_TIMEOUT": "99", "PGMINPROTOCOLVERSION": "latest", "PGMAXPROTOCOLVERSION": "latest"} {
		t.Setenv(name, value)
	}
	p := runtimePolicyFixture(t)
	password := strings.Repeat("private-disposable-", 2)
	cfg, err := runtimeConnectionConfig(p, password)
	if err != nil {
		t.Fatal("explicit policy rejected", err)
	}
	if cfg.Host != p.Hostname || cfg.Port != p.Target.Port() || cfg.Database != p.Scope.Database || cfg.User != p.Scope.Role() || cfg.Password != password ||
		cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.MinVersion != tls.VersionTLS13 || cfg.TLSConfig.ServerName != p.Hostname ||
		cfg.TLSConfig.RootCAs == nil || len(cfg.TLSConfig.Certificates) != 0 || len(cfg.Fallbacks) != 0 || cfg.ConnectTimeout != p.Timeout ||
		cfg.SSLNegotiation != "postgres" || cfg.ChannelBinding != "prefer" || cfg.MinProtocolVersion != "3.0" || cfg.MaxProtocolVersion != "3.0" || cfg.ValidateConnect != nil ||
		len(cfg.RuntimeParams) != 2 || cfg.RuntimeParams["options"] != "endpoint=ep-0123456789abcdef" || cfg.RuntimeParams["application_name"] != "neon-function-probe" {
		t.Fatal("connection policy inherited an ambient credential, fallback or session override")
	}
	addresses, err := cfg.LookupFunc(context.Background(), "foreign.example.test")
	if err != nil || len(addresses) != 1 || addresses[0] != "127.0.0.1" {
		t.Fatal("trusted address changed")
	}
	data, err := json.Marshal(p)
	if err != nil || string(data) != "{}" {
		t.Fatal("private connection policy serialized")
	}
}

func authenticationFrame(code uint32) []byte {
	frame := make([]byte, 9)
	frame[0] = 'R'
	binary.BigEndian.PutUint32(frame[1:5], 8)
	binary.BigEndian.PutUint32(frame[5:], code)
	return frame
}

func TestFunctionsSQLRejectsAuthenticationDowngrade(t *testing.T) {
	for name, codes := range map[string][]uint32{
		"trust": {0}, "cleartext": {3}, "md5": {5}, "gss": {7},
		"early_auth_ok": {10, 0}, "unsolicited_continue": {11}, "unsolicited_final": {12},
		"repeated_sasl": {10, 10}, "repeated_final": {10, 11, 12, 12},
	} {
		t.Run(name, func(t *testing.T) {
			var source []byte
			for _, code := range codes {
				source = append(source, authenticationFrame(code)...)
			}
			r := &runtimeAuthenticationReader{reader: bytes.NewReader(source)}
			if _, err := io.ReadAll(r); err == nil || r.authenticated {
				t.Fatal("authentication downgrade admitted")
			}
		})
	}
	t.Run("bounded_headers", func(t *testing.T) {
		for _, length := range []uint32{0, 3, 7, 32769, 0xffffffff} {
			frame := authenticationFrame(10)
			binary.BigEndian.PutUint32(frame[1:5], length)
			r := &runtimeAuthenticationReader{reader: bytes.NewReader(frame)}
			if _, err := io.ReadAll(r); err == nil || r.authenticated {
				t.Fatal("invalid auth packet admitted")
			}
		}
	})
	t.Run("complete_scram_then_unmodified_stream", func(t *testing.T) {
		var source []byte
		for _, code := range []uint32{10, 11, 12, 0} {
			source = append(source, authenticationFrame(code)...)
		}
		// Mechanism/server-proof validation is still pgx's responsibility. This
		// reader only enforces ordering and method; it does not authenticate these
		// synthetic frames. Actual SCRAM is exercised in the dedicated PG test.
		source = append(source, []byte("subsequent-pgx-stream")...)
		r := &runtimeAuthenticationReader{reader: bytes.NewReader(source)}
		var result []byte
		for {
			var part [2]byte
			n, err := r.Read(part[:])
			result = append(result, part[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(result, source) || !r.authenticated {
			t.Fatal("partial reads changed protocol bytes")
		}
	})
	ca, _, _ := runtimeTLSFixture(t, "sql.example.test")
	if _, err := runtimeRoots(ca, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired trust anchor accepted")
	}
	if _, err := runtimeRoots(ca, time.Now().Add(-2*time.Hour)); err == nil {
		t.Fatal("future trust anchor accepted")
	}
}

func TestFunctionsSQLConnectionPolicyRejectsUnsafeInputs(t *testing.T) {
	p := runtimePolicyFixture(t)
	_, _, leaf := runtimeTLSFixture(t, "sql.example.test")
	password := strings.Repeat("p", 32)
	for name, change := range map[string]func(*ConnectionPolicy){
		"foreign_writer":       func(v *ConnectionPolicy) { v.WriterID = "ep-other" },
		"scope":                func(v *ConnectionPolicy) { v.Scope.BranchID = "br_other" },
		"public_target":        func(v *ConnectionPolicy) { v.Target = netip.MustParseAddrPort("192.0.2.2:5432") },
		"ipv6":                 func(v *ConnectionPolicy) { v.Target = netip.MustParseAddrPort("[::1]:5432") },
		"zero_port":            func(v *ConnectionPolicy) { v.Target = netip.MustParseAddrPort("127.0.0.1:0") },
		"missing_hostname":     func(v *ConnectionPolicy) { v.Hostname = "" },
		"hostname_url":         func(v *ConnectionPolicy) { v.Hostname = "https://sql.example.test" },
		"hostname_empty_label": func(v *ConnectionPolicy) { v.Hostname = "sql..example.test" },
		"hostname_long_label":  func(v *ConnectionPolicy) { v.Hostname = strings.Repeat("a", 64) + ".test" },
		"hostname_dash":        func(v *ConnectionPolicy) { v.Hostname = "sql.-example.test" },
		"uppercase":            func(v *ConnectionPolicy) { v.Hostname = "SQL.example.test" },
		"missing_ca":           func(v *ConnectionPolicy) { v.CA = "" },
		"leaf_as_ca":           func(v *ConnectionPolicy) { v.CA = leaf },
		"duplicate_ca":         func(v *ConnectionPolicy) { v.CA += v.CA },
		"junk_prefix":          func(v *ConnectionPolicy) { v.CA = "junk\n" + v.CA },
		"junk_suffix":          func(v *ConnectionPolicy) { v.CA += "junk" },
		"timeout_zero":         func(v *ConnectionPolicy) { v.Timeout = 0 },
		"timeout_excessive":    func(v *ConnectionPolicy) { v.Timeout = 121 * time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			changed := p
			change(&changed)
			if _, err := runtimeConnectionConfig(changed, password); err == nil || strings.Contains(err.Error(), password) {
				t.Fatal("unsafe policy admitted or private value returned")
			}
		})
	}
	for _, bad := range []string{"", strings.Repeat("p", 31), strings.Repeat("p", 129), "private-password-with-unapproved!"} {
		if _, err := runtimeConnectionConfig(p, bad); err == nil {
			t.Fatal("unbounded password admitted")
		}
	}
}
