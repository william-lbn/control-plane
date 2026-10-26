package control

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const roleStorageGateway processRole = "storage_gateway"

// RunStorageGateway owns the actual native S3/REST data path. It runs no
// migrations, controllers, Console routes or workload provisioning.
func RunStorageGateway(ctx context.Context) error {
	if s3Origin() == "" {
		return errors.New("Storage gateway requires canonical S3 origin")
	}
	if err := validateS3Config(); err != nil {
		return err
	}
	if err := configureStorageCapacity(); err != nil {
		return err
	}
	if _, err := sqlConnectTimeout(); err != nil {
		return err
	}
	if os.Getenv("NEON_OBJECT_STORAGE_GC_CONFIG_FILE") != "" {
		return errors.New("Storage gateway must not receive physical delete credentials")
	}
	dsn := os.Getenv("NEON_STORAGE_DATABASE_URL")
	if path := os.Getenv("NEON_STORAGE_DATABASE_URL_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return errors.New("Storage metadata credential unavailable")
		}
		dsn = strings.TrimSpace(string(raw))
	}
	if dsn == "" {
		return errors.New("restricted Storage metadata credential required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid Storage metadata configuration")
	}
	// Eight lifecycle sessions plus admission/finalization headroom, bounded.
	config.MaxConns = 12
	config.ConnConfig.RuntimeParams["application_name"] = "neon-storage-gateway"
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("Storage metadata unavailable")
	}
	defer db.Close()
	if err = validateStorageGatewayRole(ctx, db); err != nil {
		return err
	}
	kube, err := newKubeClient()
	if err != nil {
		return errors.New("Storage writer identity reader unavailable")
	}
	s := &server{db: db, kube: kube, processRole: roleStorageGateway,
		logger:    slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		proxyHost: env("NEON_PROXY_HOST", "192.168.146.100"), proxyPort: env("NEON_PROXY_PORT", "30432")}
	s.backendKeys, err = loadBackendKeyring()
	if err != nil || s.backendKeys == nil {
		return errors.New("Storage credential verification keyring required")
	}
	raw, err := os.ReadFile(os.Getenv("NEON_STORAGE_SIGNING_KEY_FILE"))
	if err != nil {
		return errors.New("Storage capability verification key unavailable")
	}
	s.idempotencyKey, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(s.idempotencyKey) < 32 {
		return errors.New("Storage capability verification key invalid")
	}
	s.storage, err = loadStorageBlobs()
	if err != nil || s.storage == nil {
		return errors.New("dedicated product blob identity required")
	}
	if err = s.configureStorageGC(); err != nil {
		return err
	}
	defer s.closeStorageGC()
	if err = s.storage.verify(ctx); err != nil {
		return errors.New("Storage product bucket unavailable")
	}
	s.functions, err = loadStorageGatewayEvents()
	if err != nil {
		return err
	}
	handler := s.storageGatewayHandler()
	public := &http.Server{Addr: env("NEON_STORAGE_GATEWAY_BIND", ":8793"), Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: currentStorageCapacity().TransferTimeout,
		WriteTimeout: currentStorageCapacity().TransferTimeout + 5*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	healthMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		check, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if validateStorageGatewayRole(check, db) != nil || s.storage.verify(check) != nil {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	health := &http.Server{Addr: env("NEON_STORAGE_GATEWAY_HEALTH_BIND", ":8794"), Handler: healthMux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 2)
	go func() { done <- public.ListenAndServe() }()
	go func() { done <- health.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-done:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Shutdown admission first, leaving the pool alive for receipt finalization.
	if e := public.Shutdown(shutdown); e != nil {
		_ = public.Close()
		if err == nil {
			err = e
		}
	}
	_ = health.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return errors.New("Storage gateway listener stopped")
	}
	return nil
}

func (s *server) storageGatewayHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /storage/v1/{storageBranch}/{bucket}", s.storagePublic)
	mux.HandleFunc("HEAD /storage/v1/{storageBranch}/{bucket}", s.storagePublic)
	mux.HandleFunc("PUT /storage/v1/{storageBranch}/{bucket}", s.storagePublicUpload)
	s3 := s.s3Handler()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never interpret Console, health or another service's route as an S3 bucket.
		for _, prefix := range []string{"/api/", "/healthz", "/readyz", "/ai/", "/functions/", "/storage/"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				http.NotFound(w, r)
				return
			}
		}
		s3.ServeHTTP(w, r)
	}))
	return s.storageUsageHandler(mux)
}

// Metadata identities may observe lifecycle/auth state and update only their
// own ledger plus last_used_at. Reject owner/superuser credentials at startup.
func validateStorageGatewayRole(ctx context.Context, db *pgxpool.Pool) error {
	var safe, ready bool
	err := db.QueryRow(ctx, `SELECT NOT (r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls)
 AND NOT has_table_privilege(current_user,'projects','UPDATE')
 AND NOT has_table_privilege(current_user,'branches','UPDATE')
 AND NOT has_table_privilege(current_user,'operations','INSERT')
 AND NOT has_table_privilege(current_user,'sessions','SELECT')
 AND NOT has_column_privilege(current_user,'users','password_hash','SELECT')
 AND NOT has_column_privilege(current_user,'backend_credentials','revoked_at','UPDATE')
 AND NOT has_table_privilege(current_user,'storage_gateway_limits','UPDATE'),
 to_regclass('storage_gateway_requests') IS NOT NULL
 AND EXISTS(SELECT 1 FROM schema_migrations WHERE version=72)
 AND has_table_privilege(current_user,'storage_gateway_requests','SELECT,INSERT')
 AND has_column_privilege(current_user,'storage_gateway_requests','state','UPDATE')
 AND has_function_privilege(current_user,'storage_gateway_lock_instance(text,text)','EXECUTE')
 AND has_table_privilege(current_user,'backend_storage_signing','SELECT')
 AND has_table_privilege(current_user,'function_triggers','SELECT')
 FROM pg_roles r WHERE r.rolname=current_user`).Scan(&safe, &ready)
	if err != nil || !safe || !ready {
		return errors.New("restricted Storage role and schema 072 required")
	}
	return nil
}
