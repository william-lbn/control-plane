package functionsql

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// This is a disposable TLS routing fixture in front of the actual CI PG16,
// not Neon Proxy acceptance. Only this fixture removes the endpoint option:
// ordinary PostgreSQL cannot consume Neon's startup routing parameter. Actual
// authentication, role permissions, GUCs, transactions and DML go to PG16.
type runtimeTLSRouter struct {
	listener           net.Listener
	mu                 sync.Mutex
	accepted, startups int
	wg                 sync.WaitGroup
}

func startRuntimeTLSRouter(t *testing.T, certificate tls.Certificate, upstream, writer string) *runtimeTLSRouter {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	router := &runtimeTLSRouter{listener: listener}
	router.wg.Add(1)
	go func() {
		defer router.wg.Done()
		for {
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			router.mu.Lock()
			router.accepted++
			router.mu.Unlock()
			router.wg.Add(1)
			go func() {
				defer router.wg.Done()
				defer peer.Close()
				_ = peer.SetDeadline(time.Now().Add(20 * time.Second))
				var ssl [8]byte
				if _, err := io.ReadFull(peer, ssl[:]); err != nil || binary.BigEndian.Uint32(ssl[:4]) != 8 || binary.BigEndian.Uint32(ssl[4:]) != 80877103 {
					return
				}
				if _, err := peer.Write([]byte{'S'}); err != nil {
					return
				}
				secure := tls.Server(peer, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13})
				if secure.Handshake() != nil {
					return
				}
				protocol := pgproto3.NewBackend(secure, secure)
				message, err := protocol.ReceiveStartupMessage()
				startup, ok := message.(*pgproto3.StartupMessage)
				if err != nil || !ok || startup.Parameters["options"] != "endpoint="+strings.Replace(writer, "ep_", "ep-", 1) {
					return
				}
				router.mu.Lock()
				router.startups++
				router.mu.Unlock()
				delete(startup.Parameters, "options")
				data, err := startup.Encode(nil)
				if err != nil {
					return
				}
				backend, err := net.DialTimeout("tcp", upstream, 5*time.Second)
				if err != nil {
					return
				}
				defer backend.Close()
				_ = backend.SetDeadline(time.Now().Add(20 * time.Second))
				if _, err := backend.Write(data); err != nil {
					return
				}
				finished := make(chan struct{}, 1)
				go func() { _, _ = io.Copy(secure, backend); finished <- struct{}{} }()
				_, _ = io.Copy(backend, secure)
				_ = backend.Close()
				_ = secure.Close()
				<-finished
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); router.wg.Wait() })
	return router
}

func (r *runtimeTLSRouter) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepted, r.startups
}

func TestFunctionsSQLVerifiedTLSAndActualPostgres(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "control_ci" || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") || cfg.TLSConfig != nil {
		t.Fatal("disposable loopback control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("dedicated database unavailable")
	}
	defer admin.Close(context.Background())
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(random[:])
	s := Scope{FunctionID: "fnc_" + suffix, ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, Database: "fn_tls_" + suffix, Schema: "app"}
	exec := func(t *testing.T, c *pgx.Conn, q string, args ...any) {
		t.Helper()
		if _, err := c.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, admin, "CREATE DATABASE "+quote(s.Database))
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = admin.Exec(closeCtx, "DROP DATABASE IF EXISTS "+quote(s.Database)+" WITH (FORCE)")
		_, _ = admin.Exec(closeCtx, "DROP ROLE IF EXISTS "+quote(s.Role()))
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + s.Database
	owner, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal("dedicated owner unavailable")
	}
	defer owner.Close(context.Background())
	exec(t, owner, "REVOKE TEMPORARY ON DATABASE "+quote(s.Database)+" FROM PUBLIC")
	exec(t, owner, `CREATE SCHEMA app; CREATE TABLE app.items(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,value text NOT NULL)`, pgx.QueryExecModeSimpleProtocol)
	password := "private-function-disposable-" + suffix
	exec(t, owner, "SET password_encryption='scram-sha-256'")
	exec(t, owner, "CREATE ROLE "+quote(s.Role())+" NOLOGIN NOINHERIT PASSWORD "+literal(password))
	exec(t, owner, "COMMENT ON ROLE "+quote(s.Role())+" IS "+literal(s.marker()))
	var verifier string
	if owner.QueryRow(ctx, `SELECT rolpassword FROM pg_authid WHERE rolname=$1`, s.Role()).Scan(&verifier) != nil {
		t.Fatal("disposable verifier unavailable")
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ProvisionTx(ctx, tx, s, verifier); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal("provisioning failed", err)
	}
	if tx.Commit(ctx) != nil {
		t.Fatal("disposable role not committed")
	}
	ca, certificate, _ := runtimeTLSFixture(t, "sql.example.test")
	writer := "ep_" + suffix
	router := startRuntimeTLSRouter(t, certificate, net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))), writer)
	target := netip.MustParseAddrPort(router.listener.Addr().String())
	p := ConnectionPolicy{Scope: s, WriterID: writer, Target: target, Hostname: "sql.example.test", CA: ca, Timeout: 10 * time.Second}
	t.Run("verified_scram_session_real_dml_and_defaults", func(t *testing.T) {
		conn, err := ConnectRuntime(ctx, p, password)
		if err != nil {
			t.Fatal("explicit trusted SQL rejected", err)
		}
		defer conn.Close(context.Background())
		var statement, idle string
		if conn.QueryRow(ctx, `SELECT current_setting('statement_timeout'),current_setting('idle_session_timeout')`).Scan(&statement, &idle) != nil || statement != "30s" || idle != "5s" {
			t.Fatal("role timeout defaults changed")
		}
		exec(t, conn, `INSERT INTO items(value) VALUES('tls-restricted'); UPDATE items SET value='tls-verified' WHERE value='tls-restricted'`, pgx.QueryExecModeSimpleProtocol)
		var value string
		if owner.QueryRow(ctx, `SELECT value FROM app.items`).Scan(&value) != nil || value != "tls-verified" {
			t.Fatal("real DML not observed by owner")
		}
		if _, err = conn.Exec(ctx, `CREATE TEMP TABLE denied(id int)`); err == nil {
			t.Fatal("TLS login gained TEMP")
		}
	})
	for _, name := range []string{"wrong_ca", "wrong_hostname"} {
		t.Run(name, func(t *testing.T) {
			changed := p
			if name == "wrong_ca" {
				changed.CA, _, _ = runtimeTLSFixture(t, "sql.example.test")
			} else {
				changed.Hostname = "foreign.example.test"
			}
			beforeAccepted, beforeStartup := router.counts()
			if conn, err := ConnectRuntime(ctx, changed, password); err == nil || conn != nil || err.Error() != errRuntimeConnection.Error() {
				t.Fatal("untrusted TLS admitted or raw error returned")
			}
			afterAccepted, afterStartup := router.counts()
			if afterAccepted != beforeAccepted+1 || afterStartup != beforeStartup {
				t.Fatal("TLS rejection sent a SQL startup/credential or attempted a fallback")
			}
		})
	}
	t.Run("wrong_password_and_scope_rejected", func(t *testing.T) {
		if conn, err := ConnectRuntime(ctx, p, strings.Repeat("wrong", 8)); err == nil || conn != nil || err.Error() != errRuntimeConnection.Error() {
			t.Fatal("foreign password admitted or leaked")
		}
		changed := p
		changed.Scope.BranchID = "br_ffffffffffffffff"
		if conn, err := ConnectRuntime(ctx, changed, password); err == nil || conn != nil || err.Error() != errRuntimeConnection.Error() {
			t.Fatal("wrong branch marker admitted")
		}
	})
	t.Run("canceled_before_dial", func(t *testing.T) {
		before, _ := router.counts()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if conn, err := ConnectRuntime(canceled, p, password); err == nil || conn != nil {
			t.Fatal("canceled connection admitted")
		}
		after, _ := router.counts()
		if before != after {
			t.Fatal("canceled caller dialed")
		}
	})
}
