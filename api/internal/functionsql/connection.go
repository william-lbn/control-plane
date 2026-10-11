package functionsql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// ConnectionPolicy comes from the trusted Driver's original Writer/TLS policy,
// never a customer DSN. The caller must verify its lease, saved credential UID,
// and external route ownership first. TLS and a role marker alone do not prove
// physical branch ownership or fence an obsolete Worker.
type ConnectionPolicy struct {
	Scope    Scope          `json:"-"`
	WriterID string         `json:"-"`
	Target   netip.AddrPort `json:"-"`
	Hostname string         `json:"-"`
	CA       string         `json:"-"`
	Timeout  time.Duration  `json:"-"`
}

var writerPattern = regexp.MustCompile(`^ep_[a-f0-9]{16}$`)
var hostnameLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
var runtimePasswordPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
var errRuntimeConnection = errors.New("Functions authenticated SQL connection rejected or unavailable")

func runtimeRoots(value string, now time.Time) (*x509.CertPool, error) {
	if len(value) == 0 || len(value) > 32<<10 {
		return nil, errRuntimeConnection
	}
	roots := x509.NewCertPool()
	seen := map[string]bool{}
	rest := []byte(value)
	count := 0
	for len(strings.TrimSpace(string(rest))) > 0 {
		if !strings.HasPrefix(strings.TrimSpace(string(rest)), "-----BEGIN CERTIFICATE-----") {
			return nil, errRuntimeConnection
		}
		block, trailing := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errRuntimeConnection
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageCertSign == 0 ||
			now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) || seen[string(certificate.Raw)] {
			return nil, errRuntimeConnection
		}
		seen[string(certificate.Raw)] = true
		roots.AddCert(certificate)
		count++
		rest = trailing
	}
	if count == 0 {
		return nil, errRuntimeConnection
	}
	return roots, nil
}

func validRuntimeHostname(value string) bool {
	if value == "" || len(value) > 253 || value != strings.ToLower(value) {
		return false
	}
	if ip, err := netip.ParseAddr(value); err == nil {
		return ip.Is4()
	}
	for _, label := range strings.Split(value, ".") {
		if !hostnameLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// ParseConfig initializes pgx's required internal state with dummy credentials.
// Every connection/security field is then replaced. Its parsing-only sslmode
// is never used for a connection: no plaintext, system roots, DNS rebinding,
// PGOPTIONS, ambient client certificate, password file or fallback is admitted.
func runtimeConnectionConfig(p ConnectionPolicy, password string) (*pgx.ConnConfig, error) {
	if p.Scope.Validate() != nil || !writerPattern.MatchString(p.WriterID) || !runtimePasswordPattern.MatchString(password) ||
		!p.Target.IsValid() || p.Target.Port() == 0 || !p.Target.Addr().Is4() || !(p.Target.Addr().IsPrivate() || p.Target.Addr().IsLoopback()) ||
		!validRuntimeHostname(p.Hostname) ||
		p.Timeout <= 0 || p.Timeout > 120*time.Second {
		return nil, errRuntimeConnection
	}
	roots, err := runtimeRoots(p.CA, time.Now())
	if err != nil {
		return nil, errRuntimeConnection
	}
	// A Unix-socket parsing template bypasses configTLS's pre-switch reads of
	// ambient sslrootcert/client key files. No socket is ever dialed: all related
	// fields are replaced together below. Other environment validation can only
	// fail closed; it cannot authorize a foreign connection or credential.
	cfg, err := pgx.ParseConfig("host=/neon-parse-only port=5432 user=neon_config password=unused dbname=postgres sslmode=disable connect_timeout=10 target_session_attrs=any sslnegotiation=postgres channel_binding=prefer min_protocol_version=3.0 max_protocol_version=3.0")
	if err != nil {
		return nil, errRuntimeConnection
	}
	cfg.Host, cfg.Port, cfg.Database = p.Hostname, p.Target.Port(), p.Scope.Database
	cfg.User, cfg.Password = p.Scope.Role(), password
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, ServerName: p.Hostname, RootCAs: roots}
	cfg.Fallbacks = nil
	cfg.SSLNegotiation, cfg.ChannelBinding = "postgres", "prefer"
	cfg.MinProtocolVersion, cfg.MaxProtocolVersion = "3.0", "3.0"
	cfg.ValidateConnect, cfg.AfterConnect, cfg.AfterNetConnect = nil, nil, nil
	cfg.OAuthTokenProvider = nil
	cfg.ConnectTimeout = p.Timeout
	cfg.RuntimeParams = map[string]string{"options": "endpoint=" + strings.Replace(p.WriterID, "ep_", "ep-", 1), "application_name": "neon-function-probe"}
	cfg.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		return pgproto3.NewFrontend(&runtimeAuthenticationReader{reader: r}, w)
	}
	cfg.LookupFunc = func(context.Context, string) ([]string, error) { return []string{p.Target.Addr().String()}, nil }
	cfg.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
		if network != "tcp" {
			return nil, errRuntimeConnection
		}
		return (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", p.Target.String())
	}
	return cfg, nil
}

// pgx 5.9.2 has no require_auth setting. Gate only startup authentication frames
// before giving them to pgx, which still performs the full SCRAM/server-proof
// validation. This neither implements SCRAM nor traces credential bytes, and
// leaves the actual tls.Conn available for pgx's SCRAM-PLUS channel binding.
// TRUST/cleartext/MD5 cannot make a role-password check appear successful.
type runtimeAuthenticationReader struct {
	reader                     io.Reader
	pending                    []byte
	sasl, final, authenticated bool
}

func (r *runtimeAuthenticationReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.authenticated {
		return r.reader.Read(p)
	}
	var header [5]byte
	if _, err := io.ReadFull(r.reader, header[:]); err != nil {
		return 0, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length < 4 || length > 32<<10 {
		return 0, errRuntimeConnection
	}
	frame := make([]byte, int(length)+1)
	copy(frame, header[:])
	if _, err := io.ReadFull(r.reader, frame[5:]); err != nil {
		return 0, err
	}
	switch header[0] {
	case 'R':
		if length < 8 {
			return 0, errRuntimeConnection
		}
		switch binary.BigEndian.Uint32(frame[5:9]) {
		case 10:
			if r.sasl {
				return 0, errRuntimeConnection
			}
			r.sasl = true
		case 11:
			if !r.sasl || r.final {
				return 0, errRuntimeConnection
			}
		case 12:
			if !r.sasl || r.final {
				return 0, errRuntimeConnection
			}
			r.final = true
		case 0:
			if !r.sasl || !r.final {
				return 0, errRuntimeConnection
			}
			r.authenticated = true
		default:
			return 0, errRuntimeConnection
		}
	case 'E', 'N', 'v': // pgx handles bounded server errors/notices/negotiation.
	default:
		return 0, errRuntimeConnection
	}
	r.pending = frame
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// ConnectRuntime performs one verify-full TLS/SCRAM authentication and the
// actual restricted-session catalogue check. It preserves the role's 30s/5s
// defaults; the ordinary console helper's SET statement_timeout=120s is not
// appropriate here. Returned connections belong to the caller and are never
// pooled or automatically reconnected after a failed/unknown SQL result.
func ConnectRuntime(ctx context.Context, p ConnectionPolicy, password string) (*pgx.Conn, error) {
	cfg, err := runtimeConnectionConfig(p, password)
	if err != nil {
		return nil, errRuntimeConnection
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, errRuntimeConnection
	}
	if VerifyRuntimeSession(ctx, conn, p.Scope) != nil {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelClose()
		_ = conn.Close(closeCtx)
		return nil, errRuntimeConnection
	}
	return conn, nil
}
