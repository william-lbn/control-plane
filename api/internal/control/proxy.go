package control

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const sqlTotalTimeout = 180 * time.Second

// The connection budget includes Proxy cold wake and PostgreSQL authentication.
// Keep at least 30 seconds of the total budget for executing the user's SQL.
func sqlConnectTimeout() (time.Duration, error) {
	return parseSQLConnectTimeout(os.Getenv("NEON_SQL_CONNECT_TIMEOUT_SECONDS"))
}

func parseSQLConnectTimeout(value string) (time.Duration, error) {
	if value == "" {
		return 120 * time.Second, nil
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 5 || seconds > 150 {
		return 0, errors.New("NEON_SQL_CONNECT_TIMEOUT_SECONDS must be an integer from 5 to 150")
	}
	return time.Duration(seconds) * time.Second, nil
}

// Do not include the cause in Error: pgx errors can contain connection strings
// or user data. Unwrap preserves typed classification without logging them.
type sqlExecutionError struct {
	stage string
	cause error
}

func (e sqlExecutionError) Error() string { return "SQL " + safeSQLStage(e.stage) + " failed" }
func (e sqlExecutionError) Unwrap() error { return e.cause }

func safeSQLStage(stage string) string {
	switch stage {
	case "configuration", "connect", "session", "query", "rows":
		return stage
	default:
		return "unknown"
	}
}

type sqlFailure struct {
	status    int
	code      string
	message   string
	stage     string
	class     string
	sqlState  string
	retryable bool
}

var safeSQLState = regexp.MustCompile(`^[A-Z0-9]{5}$`)

// Classification describes the stage and outcome, never the SQL, DSN,
// PostgreSQL Detail/Hint/Where or the raw driver error. A timeout after sending
// SQL has an unknown outcome and must not advertise automatic retry safety.
func classifySQLFailure(err error) sqlFailure {
	f := sqlFailure{status: http.StatusUnprocessableEntity, code: "sql_failed",
		message: safeSQLError(err), stage: "unknown", class: "sql_error"}
	var execution sqlExecutionError
	if errors.As(err, &execution) {
		f.stage = safeSQLStage(execution.stage)
	}
	beforeQuery := f.stage == "connect" || f.stage == "session"
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && safeSQLState.MatchString(pgError.Code) {
		f.sqlState = pgError.Code
	}
	if f.stage == "configuration" {
		f.status, f.code, f.class = 500, "sql_configuration_failed", "configuration"
		f.message = "SQL gateway configuration is invalid; inspect the request ID in server logs"
		return f
	}
	if errors.Is(err, context.Canceled) {
		f.status, f.code, f.class = 408, "sql_cancelled", "cancelled"
		f.message = "Database request was cancelled"
		return f
	}
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) ||
		(errors.As(err, &networkError) && networkError.Timeout()) || f.sqlState == "57014" {
		f.status, f.code, f.class = 504, "sql_timeout", "timeout"
		f.retryable = beforeQuery
		if beforeQuery {
			f.message = "Database connection exceeded its cold-start budget; inspect the request ID in server logs"
		} else {
			f.message = "Database query timed out; its outcome may be unknown, verify before retrying"
		}
		return f
	}
	// The pinned Neon Proxy also serializes PasswordFailed as XX000. Inspect
	// only its known public prefix before generic cold-wake classification;
	// never return or log the remote message (which includes the role name).
	proxyBadPassword := f.stage == "connect" && f.sqlState == "XX000" && pgError != nil &&
		strings.HasPrefix(pgError.Message, "password authentication failed for user ")
	if strings.HasPrefix(f.sqlState, "28") || proxyBadPassword {
		f.class = "authentication"
		f.message = "Database authentication failed; check the database role credentials"
		return f
	}
	var certificateError *tls.CertificateVerificationError
	if errors.As(err, &certificateError) {
		f.status, f.code, f.class = 502, "sql_tls_verification_failed", "tls_verification"
		f.message = "Database TLS certificate verification failed; inspect the request ID in server logs"
		return f
	}
	// Proxy reports a failed cold wake as XX000 during startup. No user SQL
	// has been sent at this stage, so retry is safe and this is a service
	// availability error. XX000 returned by executed SQL remains a query error.
	if (f.stage == "connect" && (pgError == nil || f.sqlState == "XX000" || strings.HasPrefix(f.sqlState, "53"))) ||
		strings.HasPrefix(f.sqlState, "08") || f.sqlState == "57P01" ||
		f.sqlState == "57P02" || f.sqlState == "57P03" || f.sqlState == "53300" ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &networkError) {
		f.status, f.code, f.class = 503, "sql_connection_unavailable", "connection"
		f.retryable = beforeQuery
		f.message = "Database connection is unavailable; inspect the request ID in server logs"
		if !beforeQuery {
			f.message += "; the query outcome may be unknown, verify before retrying"
		}
	}
	return f
}

type sqlResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Command   string   `json:"command"`
	Truncated bool     `json:"truncated"`
}

func connectSQL(ctx context.Context, host, port, role, password, database, selector string) (*pgx.Conn, error) {
	connectTimeout, err := sqlConnectTimeout()
	if err != nil {
		return nil, sqlExecutionError{"configuration", err}
	}
	uri := url.URL{Scheme: "postgresql", User: url.UserPassword(role, password), Host: host + ":" + port, Path: "/" + database}
	mode := os.Getenv("NEON_PG_TLS_MODE")
	if mode == "" || mode == "lab-insecure" {
		mode = "require"
	}
	if mode != "require" && mode != "verify-full" {
		return nil, sqlExecutionError{"configuration", errors.New("invalid TLS mode")}
	}
	values := url.Values{"sslmode": []string{mode}, "options": []string{"endpoint=" + selector},
		"connect_timeout": []string{strconv.Itoa(int(connectTimeout / time.Second))}}
	if mode == "verify-full" && os.Getenv("NEON_PG_CA_FILE") != "" {
		values.Set("sslrootcert", os.Getenv("NEON_PG_CA_FILE"))
	}
	uri.RawQuery = values.Encode()
	config, err := pgx.ParseConfig(uri.String())
	if err != nil {
		return nil, sqlExecutionError{"configuration", err}
	}
	connectCtx, cancelConnect := context.WithTimeout(ctx, connectTimeout)
	conn, err := pgx.ConnectConfig(connectCtx, config)
	cancelConnect()
	if err != nil {
		return nil, sqlExecutionError{"connect", err}
	}
	if _, err = conn.Exec(ctx, "SET statement_timeout = '120s'"); err != nil {
		conn.Close(context.Background())
		return nil, sqlExecutionError{"session", err}
	}
	return conn, nil
}

func runSQL(parent context.Context, host, port, role, password, database, selector, sql string) (sqlResult, error) {
	ctx, cancel := context.WithTimeout(parent, sqlTotalTimeout)
	defer cancel()
	conn, err := connectSQL(ctx, host, port, role, password, database, selector)
	if err != nil {
		return sqlResult{}, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return sqlResult{}, sqlExecutionError{"query", err}
	}
	defer rows.Close()
	result := sqlResult{Columns: []string{}, Rows: [][]any{}, Command: ""}
	for _, field := range rows.FieldDescriptions() {
		result.Columns = append(result.Columns, string(field.Name))
	}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return sqlResult{}, sqlExecutionError{"rows", err}
		}
		if len(result.Rows) >= 200 {
			result.Truncated = true
			break
		}
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return sqlResult{}, sqlExecutionError{"rows", err}
	}
	result.Command = rows.CommandTag().String()
	return result, nil
}
