package control

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type server struct {
	db             *pgxpool.Pool
	kube           *kubeClient
	logger         *slog.Logger
	idempotencyKey []byte
	proxyHost      string
	proxyPort      string
	secureCookies  bool
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

func migrate(ctx context.Context, db *pgxpool.Pool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(794210026)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration filename: %w", err)
		}
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		b, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		// PostgreSQL parses the complete script, including dollar-quoted
		// functions and semicolons in literals. Do not split SQL ourselves.
		if _, err = tx.Exec(ctx, string(b), pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func Run(ctx context.Context) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if _, err := sqlConnectTimeout(); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[a-z0-9./-]+@sha256:[a-f0-9]{64}$`).MatchString(vmComputeImage()) {
		return errors.New("NEON_VM_COMPUTE_IMAGE must be an image pinned by sha256 digest")
	}
	databaseURL := os.Getenv("NEON_V2_DATABASE_URL")
	if databaseURL == "" {
		return errors.New("NEON_V2_DATABASE_URL is required")
	}
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("database config: %w", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		return fmt.Errorf("database unavailable: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	kube, err := newKubeClient()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	s := &server{db: db, kube: kube, logger: logger,
		proxyHost:     env("NEON_PROXY_HOST", "192.168.146.100"),
		proxyPort:     env("NEON_PROXY_PORT", "30432"),
		secureCookies: os.Getenv("NEON_COOKIE_SECURE") == "true"}
	if creationEnabled() {
		path := os.Getenv("NEON_V2_IDEMPOTENCY_KEY_FILE")
		if path == "" {
			return errors.New("NEON_V2_IDEMPOTENCY_KEY_FILE is required when creation is enabled")
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("idempotency key unavailable: %w", err)
		}
		s.idempotencyKey, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil || len(s.idempotencyKey) < 32 {
			return errors.New("idempotency key must be base64 of at least 32 random bytes")
		}
	}
	if err := s.bootstrapAdmin(ctx); err != nil {
		return fmt.Errorf("admin bootstrap: %w", err)
	}
	if err := s.seedLab(ctx); err != nil {
		return fmt.Errorf("lab seed failed: %w", err)
	}
	go s.runWorker(ctx)
	go s.runMonitor(ctx)
	if os.Getenv("NEON_V2_SCALE_ZERO_ENABLED") == "true" {
		go s.runIdleController(ctx)
	}
	go s.cleanSessions(ctx)
	mux := s.routes()
	addr := env("NEON_V2_BIND", "127.0.0.1:8788")
	httpServer := &http.Server{Addr: addr, Handler: s.middleware(mux), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 195 * time.Second, IdleTimeout: 60 * time.Second}
	errCh := make(chan error, 1)
	go func() { logger.Info("control api listening", "address", addr); errCh <- httpServer.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server stopped: %w", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

func fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	jsonResponse(w, status, apiError{code, message, requestID(r), status == 423 || status == 429 || status == 503})
}

type contextKey string

const requestIDKey contextKey = "request-id"
const userKey contextKey = "user"

func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey).(string)
	return id
}

func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if len(id) < 8 || len(id) > 128 {
			id = newID("req_")
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
