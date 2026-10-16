package control

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

var functionArtifactRoute = regexp.MustCompile(`^/internal/v1/function-artifacts/(fni_[a-f0-9]{16})/(fdp_[a-f0-9]{16})/([a-f0-9]{64})$`)

var errFunctionArtifactMetadataUnavailable = errors.New("Function artifact metadata temporarily unavailable")

type functionArtifactGateway struct {
	server     *server
	leadership *controllerState
	mu         sync.Mutex
	auth       map[string]*functions.ManagerAuthenticator
	budget     chan struct{}
}

func (g *functionArtifactGateway) authorizer(ctx context.Context, uid string, key []byte) (*functions.ManagerAuthenticator, error) {
	g.mu.Lock()
	existing := g.auth[uid]
	full := len(g.auth) >= 8
	g.mu.Unlock()
	if existing != nil {
		return existing, nil
	}
	if full {
		// Retired or superseded immutable instances cannot regain artifact
		// admission: generations cannot regress and retired rows cannot revive.
		// Evict only their process-local nonce caches, never backing resources.
		rows, err := g.server.db.Query(ctx, `SELECT b.secret_uid FROM function_instance_bootstraps b
 JOIN function_instances i ON i.id=b.instance_id JOIN function_definitions f ON f.id=i.function_id
 WHERE i.state IN ('provisioning','starting','ready') AND i.generation=f.generation
 AND f.state IN ('provisioning','active') AND b.mac_key_version=$1`, g.server.functions.input.KeyVersion)
		if err != nil {
			return nil, errFunctionArtifact
		}
		live := map[string]bool{}
		for rows.Next() {
			var current string
			if rows.Scan(&current) != nil {
				rows.Close()
				return nil, errFunctionArtifact
			}
			live[current] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, errFunctionArtifact
		}
		g.mu.Lock()
		for current := range g.auth {
			if !live[current] {
				delete(g.auth, current)
			}
		}
		g.mu.Unlock()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing = g.auth[uid]; existing != nil {
		return existing, nil
	}
	if len(g.auth) >= 8 {
		return nil, errFunctionArtifact
	}
	auth, err := functions.NewManagerAuthenticator(key)
	if err != nil {
		return nil, errFunctionArtifact
	}
	g.auth[uid] = auth
	return auth, nil
}

// Only the private Worker TLS listener receives this handler. It resolves the
// current authoritative instance/backings, verifies the original immutable
// bootstrap and SQL Secret, then serves the actually hash-verified ZIP. It is
// neither a generic blob URL nor a customer-controlled filesystem/network proxy.
func (s *server) serveFunctionArtifacts(ctx context.Context, state *controllerState) error {
	if s.processRole != roleWorker || s.functions == nil || s.functions.artifactTLS == nil {
		return errFunctionsPlatform
	}
	gateway := &functionArtifactGateway{server: s, leadership: state, auth: map[string]*functions.ManagerAuthenticator{}, budget: make(chan struct{}, 4)}
	listener := &http.Server{Addr: s.functions.input.ArtifactBind, Handler: gateway, TLSConfig: s.functions.artifactTLS,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
		case <-finished:
			return
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = listener.Shutdown(closeCtx)
	}()
	err := listener.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.New("Functions private artifact listener stopped")
}

func (g *functionArtifactGateway) identities(ctx context.Context, instance, deployment, digest string) (functionBootstrapReference, functionSQLReference, functionArtifactReference, error) {
	var boot functionBootstrapReference
	var sql functionSQLReference
	var artifact functionArtifactReference
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := g.server.db.QueryRow(ctx, `SELECT f.id,f.project_id,f.branch_id,f.slug,i.generation,
 b.namespace,b.secret_name,b.secret_uid,b.content_mac,b.mac_key_version,f.endpoint_id,
 c.credential_generation,c.namespace,c.secret_name,c.secret_uid,c.content_mac,c.mac_key_version,c.writer_id,c.database_name,c.sql_schema,c.provisioning_role,
 d.bundle_bytes,d.entry,d.artifact_key
 FROM function_instances i JOIN function_definitions f ON f.id=i.function_id
 JOIN function_deployments d ON d.id=i.deployment_id AND d.function_id=f.id
 JOIN function_instance_bootstraps b ON b.instance_id=i.id AND b.function_id=f.id
 JOIN function_deployment_backings a ON a.deployment_id=d.id
 JOIN function_sql_credentials c ON c.function_id=f.id AND c.credential_generation=1
 JOIN projects p ON p.id=f.project_id JOIN organizations o ON o.id=f.org_id
 JOIN branches r ON r.id=f.branch_id AND r.project_id=f.project_id
 JOIN endpoints e ON e.id=f.endpoint_id AND e.project_id=f.project_id AND e.branch_id=f.branch_id
 JOIN operations op ON op.id=d.operation_id
 WHERE i.id=$1 AND i.deployment_id=$2 AND d.bundle_digest=$3
 AND i.state IN ('provisioning','starting','ready') AND i.generation=f.generation
 AND f.state IN ('provisioning','active') AND (f.target_deployment_id=d.id OR f.active_deployment_id=d.id)
 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND o.state='active'
 AND r.state='ready' AND r.deleted_at IS NULL AND e.deleted_at IS NULL AND e.state IN ('active','suspended')
 AND e.endpoint_type='read_write' AND c.writer_id=e.id AND c.database_name=f.database_name AND c.sql_schema=f.sql_schema
 AND f.sql_role='fn_'||substring(f.id from 5)
 AND ((d.state='building' AND op.state='running' AND op.lease_expires_at>clock_timestamp()) OR d.state='completed')`,
		instance, deployment, digest).Scan(&boot.FunctionID, &boot.Scope.ProjectID, &boot.Scope.BranchID, &boot.Scope.Slug, &boot.Scope.Generation,
		&boot.Namespace, &boot.Name, &boot.UID, &boot.MAC, &boot.MACKeyVersion, &boot.WriterID,
		&sql.Generation, &sql.Namespace, &sql.Name, &sql.UID, &sql.MAC, &sql.MACKeyVersion, &sql.WriterID, &sql.Scope.Database, &sql.Scope.Schema, &sql.Scope.ProvisioningRole,
		&artifact.Size, &artifact.Entry, &artifact.Key)
	boot.Scope.InstanceID, boot.Scope.DeploymentID, boot.BundleDigest = instance, deployment, digest
	sql.Scope.FunctionID, sql.Scope.ProjectID, sql.Scope.BranchID = boot.FunctionID, boot.Scope.ProjectID, boot.Scope.BranchID
	artifact.ProjectID, artifact.BranchID, artifact.Digest = boot.Scope.ProjectID, boot.Scope.BranchID, digest
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return functionBootstrapReference{}, functionSQLReference{}, functionArtifactReference{}, errFunctionArtifactMetadataUnavailable
	}
	if err != nil || !boot.valid() || boot.UID == "" || !sql.valid() || sql.UID == "" || sql.WriterID != boot.WriterID {
		return functionBootstrapReference{}, functionSQLReference{}, functionArtifactReference{}, errFunctionArtifact
	}
	return boot, sql, artifact, nil
}

func (g *functionArtifactGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if g.server.functions == nil || !g.leadership.leading.Load() {
		jsonResponse(w, 503, record{"code": "functions_worker_unavailable"})
		return
	}
	match := functionArtifactRoute.FindStringSubmatch(r.URL.EscapedPath())
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if r.Method != http.MethodGet || len(match) != 4 || r.URL.RawPath != "" || r.URL.RawQuery != "" || err != nil || len(body) != 0 {
		jsonResponse(w, 404, record{"code": "artifact_route_rejected"})
		return
	}
	select {
	case g.budget <- struct{}{}:
		defer func() { <-g.budget }()
	default:
		jsonResponse(w, 429, record{"code": "artifact_capacity_exhausted"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	boot, sql, artifact, err := g.identities(ctx, match[1], match[2], match[3])
	if errors.Is(err, errFunctionArtifactMetadataUnavailable) {
		w.Header().Set("Retry-After", "1")
		jsonResponse(w, 503, record{"code": "artifact_metadata_unavailable"})
		return
	}
	if err != nil || boot.MACKeyVersion != g.server.functions.input.KeyVersion || sql.MACKeyVersion != g.server.functions.input.KeyVersion {
		jsonResponse(w, 404, record{"code": "artifact_identity_rejected"})
		return
	}
	config, err := g.server.kube.readFunctionBootstrap(ctx, g.server.functions.key, boot)
	if err != nil {
		jsonResponse(w, 503, record{"code": "artifact_backing_unavailable"})
		return
	}
	credential, err := g.server.kube.readFunctionSQLSecret(ctx, g.server.functions.key, sql)
	u, parseErr := url.Parse(config.Config.DatabaseURL)
	password, exists := "", false
	if u != nil && u.User != nil {
		password, exists = u.User.Password()
	}
	if err != nil || parseErr != nil || u == nil || u.User == nil || !exists || u.User.Username() != sql.Scope.Role() || u.Path != "/"+sql.Scope.Database ||
		!hmac.Equal([]byte(password), []byte(credential.Credential.Password)) || config.Config.ArtifactURL != g.server.functions.input.ArtifactOrigin+r.URL.Path ||
		config.Config.SQLCA != g.server.functions.proxyCA || config.Config.ManagerCA != g.server.functions.guestCA {
		jsonResponse(w, 503, record{"code": "artifact_sql_or_trust_rejected"})
		return
	}
	key, err := base64.StdEncoding.Strict().DecodeString(config.Config.ArtifactKey)
	if err != nil || len(key) != 32 {
		jsonResponse(w, 503, record{"code": "artifact_backing_unavailable"})
		return
	}
	auth, err := g.authorizer(ctx, boot.UID, key)
	if err != nil {
		jsonResponse(w, 429, record{"code": "artifact_capacity_exhausted"})
		return
	}
	if auth.Verify(r, nil) != nil {
		jsonResponse(w, 401, record{"code": "artifact_authentication_rejected"})
		return
	}
	archive, err := g.server.storage.readFunctionArtifact(ctx, artifact)
	if err != nil {
		jsonResponse(w, 503, record{"code": "artifact_backing_unavailable"})
		return
	}
	// Observe the authoritative scope once more after all external I/O. This is
	// a read admission check, not a distributed fence or cross-system transaction.
	current, _, _, err := g.identities(ctx, match[1], match[2], match[3])
	if err != nil || current != boot || !g.leadership.leading.Load() {
		jsonResponse(w, 503, record{"code": "artifact_intent_changed"})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(archive)
}
