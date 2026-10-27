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
	err := db.QueryRow(ctx, `SELECT NOT (r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls OR r.rolreplication)
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=r.oid)
 AND NOT has_schema_privilege(current_user,current_schema(),'CREATE')
 AND NOT has_any_column_privilege(current_user,'projects','UPDATE')
 AND NOT has_any_column_privilege(current_user,'branches','UPDATE')
 AND NOT has_any_column_privilege(current_user,'endpoints','UPDATE')
 AND NOT has_any_column_privilege(current_user,'object_storage_instances','UPDATE')
 AND NOT has_any_column_privilege(current_user,'operations','INSERT')
 AND NOT has_any_column_privilege(current_user,'sessions','SELECT')
 AND NOT has_column_privilege(current_user,'users','password_hash','SELECT')
 AND NOT has_column_privilege(current_user,'backend_credentials','revoked_at','UPDATE')
 AND NOT has_any_column_privilege(current_user,'storage_gateway_limits','UPDATE'),
 EXISTS(SELECT 1 FROM schema_migrations WHERE version=72)
 AND NOT EXISTS (
  SELECT 1 FROM (VALUES
   ('schema_migrations','SELECT'),('projects','SELECT'),('branches','SELECT'),('endpoints','SELECT'),
   ('object_storage_instances','SELECT'),('organizations','SELECT'),('organization_members','SELECT'),
   ('project_grants','SELECT'),('backend_credentials','SELECT'),('backend_storage_signing','SELECT'),
   ('storage_gateway_limits','SELECT'),('storage_gateway_days','SELECT'),('storage_gateway_requests','SELECT'),
   ('storage_gc_allocations','SELECT'),('function_triggers','SELECT'),('function_definitions','SELECT'),
   ('storage_event_branches','SELECT'),('storage_gateway_days','INSERT'),('storage_gateway_requests','INSERT'),
   ('storage_gc_allocations','INSERT'),('storage_event_branches','INSERT')
  ) required(relation,privilege) WHERE NOT has_table_privilege(current_user,relation,privilege)
 )
 AND NOT EXISTS (
  SELECT 1 FROM (VALUES
   ('users','id','SELECT'),('users','disabled','SELECT'),('backend_credentials','last_used_at','UPDATE'),
   ('storage_gateway_days','requests','UPDATE'),('storage_gateway_days','observed_bytes','UPDATE'),('storage_gateway_days','reserved_bytes','UPDATE'),
   ('storage_gateway_requests','state','UPDATE'),('storage_gateway_requests','finished_at','UPDATE'),
   ('storage_gateway_requests','http_status','UPDATE'),('storage_gateway_requests','ingress_bytes','UPDATE'),
   ('storage_gateway_requests','egress_bytes','UPDATE'),('storage_gateway_requests','duration_ms','UPDATE'),
   ('storage_gc_allocations','state','UPDATE'),('storage_gc_allocations','resolved_at','UPDATE'),
   ('storage_event_branches','endpoint_id','UPDATE'),('storage_event_branches','database_name','UPDATE'),
   ('storage_event_branches','timeline_id','UPDATE'),('storage_event_branches','generation','UPDATE'),
   ('storage_event_branches','dirty','UPDATE'),('storage_event_branches','error_code','UPDATE'),('storage_event_branches','updated_at','UPDATE'),
   ('audit_events','actor_id','INSERT'),('audit_events','action','INSERT'),('audit_events','resource_type','INSERT'),
   ('audit_events','resource_id','INSERT'),('audit_events','outcome','INSERT'),('audit_events','request_id','INSERT')
  ) required(relation,column_name,privilege) WHERE NOT has_column_privilege(current_user,relation,column_name,privilege)
 )
 AND NOT EXISTS (
  SELECT 1 FROM (VALUES ('storage_gateway_lock_project(text,text)'),('storage_gateway_lock_branch(text,text)'),('storage_gateway_lock_instance(text,text)')) required(signature)
  WHERE NOT has_function_privilege(current_user,signature,'EXECUTE')
 )
 FROM pg_roles r WHERE r.rolname=current_user`).Scan(&safe, &ready)
	if err != nil || !safe || !ready {
		return errors.New("restricted Storage role and schema 072 required")
	}
	return nil
}
