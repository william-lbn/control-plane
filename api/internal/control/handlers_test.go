package control

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSafeSQLErrorNeverReturnsConnectionSecret(t *testing.T) {
	secret := "password-do-not-log"
	message := safeSQLError(errors.New("connect failed: postgresql://user:" + secret + "@db.example"))
	if strings.Contains(message, secret) {
		t.Fatal("connection secret leaked to API error")
	}
	pgMessage := safeSQLError(&pgconn.PgError{Message: "syntax error at or near SELECT"})
	if pgMessage != "syntax error at or near SELECT" {
		t.Fatalf("expected sanitized PostgreSQL error, got %q", pgMessage)
	}
}

func TestRequestHashIsStableAndScoped(t *testing.T) {
	input := map[string]any{"max_cpu_milli": 2000}
	if requestHash("a", input) != requestHash("a", input) {
		t.Fatal("same request does not produce stable idempotency hash")
	}
	if requestHash("a", input) == requestHash("b", input) {
		t.Fatal("different scopes share idempotency hash")
	}
}
