package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSQLConnectionBudget(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"", 120 * time.Second, true}, {"5", 5 * time.Second, true},
		{"150", 150 * time.Second, true}, {" 120 ", 120 * time.Second, true},
		{"4", 0, false}, {"151", 0, false}, {"-1", 0, false},
		{"0", 0, false}, {"1.5", 0, false}, {"abc", 0, false},
		{"999999999999999999999999", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := parseSQLConnectTimeout(tc.value)
			if (err == nil) != tc.valid || got != tc.want {
				t.Fatalf("budget=%v err=%v; want %v, valid=%v", got, err, tc.want, tc.valid)
			}
			if err == nil && sqlTotalTimeout-got < 30*time.Second {
				t.Fatal("connection budget leaves insufficient SQL execution time")
			}
		})
	}
	t.Setenv("NEON_SQL_CONNECT_TIMEOUT_SECONDS", "15")
	got, err := sqlConnectTimeout()
	if err != nil || got != 15*time.Second {
		t.Fatal("configured connection budget was not read")
	}
	t.Setenv("NEON_SQL_CONNECT_TIMEOUT_SECONDS", "151")
	if err := Run(context.Background()); err == nil || !strings.Contains(err.Error(), "NEON_SQL_CONNECT_TIMEOUT_SECONDS") {
		t.Fatal("invalid connection budget must reject startup before database or Kubernetes access")
	}
}

func TestSQLFailureClassificationAndRetrySafety(t *testing.T) {
	for _, tc := range []struct {
		name, stage, class string
		cause              error
		status             int
		retryable          bool
	}{
		{"connect timeout", "connect", "timeout", context.DeadlineExceeded, 504, true},
		{"session timeout", "session", "timeout", context.DeadlineExceeded, 504, true},
		{"query timeout", "query", "timeout", context.DeadlineExceeded, 504, false},
		{"row timeout", "rows", "timeout", &net.DNSError{IsTimeout: true}, 504, false},
		{"statement timeout", "rows", "timeout", &pgconn.PgError{Code: "57014"}, 504, false},
		{"cancelled", "query", "cancelled", context.Canceled, 408, false},
		{"bad password", "connect", "authentication", &pgconn.PgError{Code: "28P01"}, 422, false},
		{"pinned proxy bad password", "connect", "authentication", &pgconn.PgError{Code: "XX000", Message: "password authentication failed for user 'cloud_admin'"}, 422, false},
		{"user SQL resembling proxy authentication", "rows", "sql_error", &pgconn.PgError{Code: "XX000", Message: "password authentication failed for user 'cloud_admin'"}, 422, false},
		{"missing database", "connect", "sql_error", &pgconn.PgError{Code: "3D000"}, 422, false},
		{"syntax", "query", "sql_error", &pgconn.PgError{Code: "42601"}, 422, false},
		{"read only", "rows", "sql_error", &pgconn.PgError{Code: "25006"}, 422, false},
		{"not ready", "connect", "connection", &pgconn.PgError{Code: "57P03"}, 503, true},
		{"proxy cold wake internal failure", "connect", "connection", &pgconn.PgError{Code: "XX000"}, 503, true},
		{"internal error after SQL sent", "rows", "sql_error", &pgconn.PgError{Code: "XX000"}, 422, false},
		{"resource exhaustion on connect", "connect", "connection", &pgconn.PgError{Code: "53200"}, 503, true},
		{"resource exhaustion after SQL sent", "query", "sql_error", &pgconn.PgError{Code: "53200"}, 422, false},
		{"connection refused", "connect", "connection", errors.New("dial refused"), 503, true},
		{"server shutdown mid query", "rows", "connection", &pgconn.PgError{Code: "57P01"}, 503, false},
		{"connection lost mid query", "rows", "connection", io.EOF, 503, false},
		{"configuration", "configuration", "configuration", errors.New("invalid config"), 500, false},
		{"certificate", "connect", "tls_verification", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, 502, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := classifySQLFailure(sqlExecutionError{tc.stage, tc.cause})
			if failure.status != tc.status || failure.retryable != tc.retryable || failure.class != tc.class || failure.stage != tc.stage {
				t.Fatalf("unexpected classification: %+v", failure)
			}
		})
	}
}

func TestSQLFailureLogsRedactDriverSecretsAndCorrelateResponse(t *testing.T) {
	secret := "never-log-this-password"
	sql := "INSERT INTO private_tokens VALUES ('sensitive-user-data')"
	dsn := "postgresql://user:" + secret + "@db.example/private-db"
	for _, cause := range []error{
		errors.New("connect: " + dsn + " " + sql),
		&pgconn.PgError{Code: "42601", Message: sql, Detail: secret, Hint: dsn, InternalQuery: sql},
		&pgconn.PgError{Code: "XX000", Message: "password authentication failed for user '" + secret + "'", Detail: dsn},
		&pgconn.PgError{Code: secret, Message: "bad code"},
	} {
		var logs bytes.Buffer
		s := &server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
		r := httptest.NewRequest("POST", "/query", strings.NewReader(sql))
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, "req_correlation_test"))
		r.SetPathValue("project", "prj_test")
		r.SetPathValue("endpoint", "ep_test")
		w := httptest.NewRecorder()
		s.sqlFailureResponse(w, r, sqlExecutionError{"connect", cause})
		for _, forbidden := range []string{secret, sql, dsn, "private-db", "sensitive-user-data"} {
			if strings.Contains(logs.String(), forbidden) {
				t.Fatalf("sensitive data leaked in structured logs: %q", forbidden)
			}
		}
		var entry map[string]any
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		var response apiError
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if entry["request_id"] != response.RequestID || response.RequestID != "req_correlation_test" ||
			entry["project_id"] != "prj_test" || entry["endpoint_id"] != "ep_test" || entry["stage"] != "connect" {
			t.Fatal("failure log cannot be correlated to request and endpoint")
		}
		if entry["status"] != float64(w.Code) || entry["retryable"] != response.Retryable {
			t.Fatal("API response and log disagree about status or retry safety")
		}
		if len(entry) != 11 {
			t.Fatalf("log fields changed; review redaction allowlist: %+v", entry)
		}
	}
	wrapped := sqlExecutionError{dsn, errors.New(secret)}
	if wrapped.Error() != "SQL unknown failed" || classifySQLFailure(wrapped).stage != "unknown" {
		t.Fatal("untrusted stage entered error or log classification")
	}
}
