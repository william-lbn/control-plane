package control

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// 真正建立TLS连接，证明隧道仅替换地址；证书名称、CA和轮换凭据仍被检查。
func TestKubeExternalTunnelPreservesTLSAndRotatingToken(t *testing.T) {
	ca, cert, key, err := newComputeCertificate("ep_transport", "testing", "kube.internal.test")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	received := make(chan string, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		received <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"kind": "test"})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	tokenPath := filepath.Join(dir, "token")
	if err = os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(tokenPath, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEON_KUBE_API", server.URL)
	t.Setenv("NEON_KUBE_CA", caPath)
	t.Setenv("NEON_KUBE_CERT", "")
	t.Setenv("NEON_KUBE_KEY", "")
	t.Setenv("NEON_KUBE_TOKEN_FILE", tokenPath)
	t.Setenv("NEON_KUBE_TLS_SERVER_NAME", "kube.internal.test")
	k, err := newKubeClient()
	if err != nil {
		t.Fatal(err)
	}
	defer k.http.CloseIdleConnections()
	if _, err = k.request(context.Background(), http.MethodGet, "/", nil); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "Bearer first" {
		t.Fatal("wrong first credential")
	}
	if err = os.WriteFile(tokenPath, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = k.request(context.Background(), http.MethodGet, "/", nil); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "Bearer second" {
		t.Fatal("retained expired credential")
	}
	if err = os.WriteFile(tokenPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = k.request(context.Background(), http.MethodGet, "/", nil); err == nil {
		t.Fatal("empty credential accepted")
	}
	if err = os.WriteFile(tokenPath, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEON_KUBE_TLS_SERVER_NAME", "wrong.internal.test")
	bad, err := newKubeClient()
	if err != nil {
		t.Fatal(err)
	}
	defer bad.http.CloseIdleConnections()
	if _, err = bad.request(context.Background(), http.MethodGet, "/", nil); err == nil {
		t.Fatal("wrong TLS identity accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("failed identity or credential reached API")
	}
	t.Setenv("NEON_KUBE_API", "http://127.0.0.1:12345")
	if _, err = newKubeClient(); err == nil {
		t.Fatal("cleartext bearer transport accepted")
	}
}

// PG协议夹具只验证拨号/TLS/Startup/连接参数，不声称替代真实Neon SQL验收。
func TestSQLHostaddrPreservesServerIdentityAndSelector(t *testing.T) {
	ca, cert, key, err := newComputeCertificate("ep_transport", "testing", "proxy.internal.test")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEON_PG_TLS_MODE", "verify-full")
	t.Setenv("NEON_PG_CA_FILE", caPath)
	t.Setenv("NEON_PG_HOSTADDR", "127.0.0.1")
	t.Setenv("NEON_SQL_CONNECT_TIMEOUT_SECONDS", "5")
	for _, host := range []string{"proxy.internal.test", "wrong.internal.test"} {
		t.Run(host, func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			_, port, _ := net.SplitHostPort(l.Addr().String())
			result := make(chan error, 1)
			go func() {
				var outcome error
				defer func() { result <- outcome }()
				raw, e := l.Accept()
				if e != nil {
					outcome = e
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
				var sslRequest [8]byte
				if _, e = io.ReadFull(raw, sslRequest[:]); e != nil {
					outcome = e
					return
				}
				if binary.BigEndian.Uint32(sslRequest[:4]) != 8 || binary.BigEndian.Uint32(sslRequest[4:]) != 80877103 {
					outcome = errors.New("missing PostgreSQL SSLRequest")
					return
				}
				if _, e = raw.Write([]byte{'S'}); e != nil {
					outcome = e
					return
				}
				secure := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
				if e = secure.Handshake(); e != nil {
					if host == "proxy.internal.test" {
						outcome = e
					}
					return
				}
				if host != "proxy.internal.test" {
					outcome = errors.New("wrong hostname passed TLS")
					return
				}
				if secure.ConnectionState().ServerName != host {
					outcome = errors.New("dial address replaced TLS SNI")
					return
				}
				backend := pgproto3.NewBackend(secure, secure)
				m, e := backend.ReceiveStartupMessage()
				if e != nil {
					outcome = e
					return
				}
				startup, ok := m.(*pgproto3.StartupMessage)
				if !ok || startup.Parameters["user"] != "app_owner" || startup.Parameters["database"] != "app" || startup.Parameters["options"] != "endpoint=ep-test" {
					outcome = errors.New("startup identity changed")
					return
				}
				backend.Send(&pgproto3.AuthenticationOk{})
				backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "17.0"})
				backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if e = backend.Flush(); e != nil {
					outcome = e
					return
				}
				m, e = backend.Receive()
				if e != nil {
					outcome = e
					return
				}
				query, ok := m.(*pgproto3.Query)
				if !ok || query.String != "SET statement_timeout = '120s'" {
					outcome = errors.New("session timeout configuration changed")
					return
				}
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SET")})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				outcome = backend.Flush()
				if outcome == nil {
					_, _ = backend.Receive()
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			defer cancel()
			conn, err := connectSQL(ctx, host, port, "app_owner", "synthetic-password", "app", "ep-test")
			if host == "proxy.internal.test" && err != nil {
				t.Fatal(err)
			}
			if host != "proxy.internal.test" && err == nil {
				t.Fatal("SQL wrong TLS hostname accepted")
			}
			if conn != nil {
				_ = conn.Close(ctx)
			}
			if err = <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Setenv("NEON_PG_HOSTADDR", "not-an-ip")
	if _, err = connectSQL(context.Background(), "proxy.internal.test", "5432", "role", "password", "db", "ep"); err == nil || classifySQLFailure(err).stage != "configuration" {
		t.Fatal("invalid dial address was not rejected before network")
	}
}
