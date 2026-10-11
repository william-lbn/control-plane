package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/dataapi"
)

// DataAPISpec contains public provider keys. Passwords, signing seeds and caller
// tokens never enter metadata, Operation payloads, audit records or UI responses.
type DataAPISpec struct {
	Database       string          `json:"database"`
	Schema         string          `json:"schema"`
	Issuer         string          `json:"issuer"`
	Audience       string          `json:"audience"`
	JWKS           json.RawMessage `json:"jwks"`
	AllowedOrigins []string        `json:"allowed_origins"`
}
type dataAPIPayload struct {
	ProjectID  string      `json:"project_id"`
	BranchID   string      `json:"branch_id"`
	EndpointID string      `json:"endpoint_id"`
	Generation int64       `json:"generation"`
	SecretRef  string      `json:"secret_ref"`
	Spec       DataAPISpec `json:"spec"`
}

var digestImage = regexp.MustCompile(`^[a-z0-9./-]+@sha256:[a-f0-9]{64}$`)
var dataBranchID = regexp.MustCompile(`^br_[a-f0-9]{16}$`)
var dataSchema = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// The first native Driver explicitly requires the lab transport profile.
// It does not advertise a production TLS gate while its Pod and relay use HTTP.
func dataAPIEnabled() bool {
	return os.Getenv("NEON_DATA_API_ENABLED") == "true" && os.Getenv("NEON_DATA_API_LAB_HTTP") == "true" &&
		digestImage.MatchString(os.Getenv("NEON_DATA_API_GATEWAY_IMAGE")) && digestImage.MatchString(os.Getenv("NEON_DATA_API_POSTGREST_IMAGE"))
}
func dataAPIName(branch string) string  { return "neon-da-" + strings.TrimPrefix(branch, "br_") }
func dataAPIRole(branch string) string  { return "app_da_" + strings.TrimPrefix(branch, "br_") }
func dataAPILogin(branch string) string { return "control_da_" + strings.TrimPrefix(branch, "br_") }
func dataAPILabels(p dataAPIPayload) map[string]string {
	labels := credentialLabels(p.ProjectID, p.EndpointID)
	labels["neon-control/branch-id"] = p.BranchID
	labels["neon-control/data-api"] = p.BranchID
	return labels
}
func ownedDataAPI(item map[string]any, p dataAPIPayload) bool {
	return owned(item, p.ProjectID, p.EndpointID) && nested(item, "metadata", "labels", "neon-control/branch-id") == p.BranchID
}
func dataAPIConfig(p dataAPIPayload) dataapi.Config {
	return dataapi.Config{Version: 1, DelegationIssuer: "neon-control-data-api", AllowPlaintextUpstream: true,
		Routes: []dataapi.RouteConfig{{BranchID: p.BranchID, Upstream: "http://127.0.0.1:3000", Issuer: p.Spec.Issuer,
			Audience: p.Spec.Audience, DefaultRole: dataAPIRole(p.BranchID), AllowedRoles: []string{dataAPIRole(p.BranchID)},
			JWKS: p.Spec.JWKS, AllowedOrigins: p.Spec.AllowedOrigins}}}
}
func validateDataAPI(p dataAPIPayload) error {
	if !dataBranchID.MatchString(p.BranchID) || !validPGIdentifier(p.Spec.Database) || !dataSchema.MatchString(p.Spec.Schema) ||
		p.Spec.Schema == "public" || p.Spec.Schema == "information_schema" || strings.HasPrefix(p.Spec.Schema, "pg_") || strings.HasPrefix(p.Spec.Schema, "neon_") ||
		strings.HasPrefix(p.Spec.Schema, "control_") {
		return errors.New("use an application-owned database schema distinct from public and reserved schemas")
	}
	b, err := json.Marshal(dataAPIConfig(p))
	if err != nil {
		return err
	}
	_, err = dataapi.ParseConfig(bytes.NewReader(b))
	return err
}

func (s *server) dataAPIRead(w http.ResponseWriter, r *http.Request) {
	branch, err := s.one(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, r.PathValue("branch"), r.PathValue("project"))
	if err != nil {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	item, err := s.one(r.Context(), `SELECT branch_id,endpoint_id,generation,state,spec,updated_at FROM data_api_instances WHERE branch_id=$1 AND project_id=$2`, branch["id"], r.PathValue("project"))
	if isNoRows(err) {
		item = record{"branch_id": branch["id"], "generation": int64(0), "state": "disabled", "spec": nil}
	} else if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read Data API")
		return
	}
	item["driver_enabled"] = dataAPIEnabled()
	item["request_role"] = dataAPIRole(stringVal(branch["id"]))
	item["public_endpoint"] = "/data/v1/" + stringVal(branch["id"]) + "/"
	// Kubernetes readiness is observed without SQL; visiting this page must not
	// cold-wake a Compute. Active metadata alone is not a runtime health claim.
	if item["state"] == "active" {
		item["runtime"] = s.kube.runtime(r.Context(), "deployment", dataAPIName(stringVal(branch["id"])))
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, item["generation"].(int64)))
	jsonResponse(w, 200, item)
}
func (s *server) acceptDataAPI(w http.ResponseWriter, r *http.Request, id string) {
	op, err := s.operationRecord(r.Context(), id, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load Operation")
		return
	}
	if op["resource_id"] != r.PathValue("branch") || op["resource_type"] != "data_api" {
		fail(w, r, 409, "idempotency_conflict", "Key belongs to another resource")
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+r.PathValue("project")+"/operations/"+id)
	jsonResponse(w, 202, map[string]any{"branch_id": r.PathValue("branch"), "operation": op})
}
func (s *server) replayDataAPI(w http.ResponseWriter, r *http.Request, key, hash string) bool {
	var stored, id string
	err := s.db.QueryRow(r.Context(), `SELECT request_hash,operation_id FROM idempotency_keys WHERE actor_id=$1 AND key_hash=$2`, userFrom(r).ID, keyHash(key)).Scan(&stored, &id)
	if isNoRows(err) {
		return false
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check idempotency")
		return true
	}
	if stored != hash {
		fail(w, r, 409, "idempotency_conflict", "Key used with different input")
		return true
	}
	s.acceptDataAPI(w, r, id)
	return true
}
func (s *server) dataAPIMutate(w http.ResponseWriter, r *http.Request) {
	if !dataAPIEnabled() || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "data_api_driver_disabled", "Data API native Driver is not configured")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	if err != nil || version < 0 {
		fail(w, r, 428, "version_required", "If-Match with the numeric generation is required")
		return
	}
	p := dataAPIPayload{ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch")}
	action := "enable_data_api"
	if r.Method == http.MethodDelete {
		action = "disable_data_api"
	} else {
		if err = readJSON(r, &p.Spec); err != nil {
			fail(w, r, 422, "invalid_json", "Invalid Data API configuration JSON")
			return
		}
		if err = validateDataAPI(p); err != nil {
			fail(w, r, 422, "invalid_data_api_config", err.Error())
			return
		}
	}
	hash := s.createRequestHash(action+":"+p.ProjectID+":"+p.BranchID, map[string]any{"spec": p.Spec, "generation": version})
	if s.replayDataAPI(w, r, key, hash) {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not admit Operation")
		return
	}
	defer tx.Rollback(r.Context())
	var branchState string
	err = tx.QueryRow(r.Context(), `SELECT state FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL FOR UPDATE`, p.BranchID, p.ProjectID).Scan(&branchState)
	if err != nil || branchState != "ready" {
		fail(w, r, 409, "branch_not_ready", "Ready branch required")
		return
	}
	var previous int64
	var state string
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT generation,state,spec,endpoint_id,secret_ref FROM data_api_instances WHERE branch_id=$1`, p.BranchID).Scan(&previous, &state, &raw, &p.EndpointID, &p.SecretRef)
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not read service intent")
		return
	}
	if previous != version {
		fail(w, r, 412, "version_conflict", "Data API generation changed; refresh before retrying")
		return
	}
	var pending bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, p.ProjectID, p.BranchID).Scan(&pending)
	if err != nil || pending {
		fail(w, r, 409, "branch_operation_in_progress", "Another branch Operation is active")
		return
	}
	p.Generation = previous + 1
	if action == "enable_data_api" {
		if state != "" && state != "disabled" {
			fail(w, r, 409, "disable_before_reconfigure", "Disable the existing instance before changing configuration")
			return
		}
		err = tx.QueryRow(r.Context(), `SELECT id FROM endpoints WHERE branch_id=$1 AND project_id=$2 AND endpoint_type='read_write' AND state='active' AND deleted_at IS NULL`, p.BranchID, p.ProjectID).Scan(&p.EndpointID)
		if err != nil {
			fail(w, r, 409, "writer_required", "A ready branch writer is required")
			return
		}
		p.SecretRef = fmt.Sprintf("%s-g%d", dataAPIName(p.BranchID), p.Generation)
		state = "provisioning"
	} else {
		if previous == 0 || state == "disabled" {
			fail(w, r, 409, "data_api_not_enabled", "Data API is already disabled")
			return
		}
		if err = json.Unmarshal(raw, &p.Spec); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Service intent is invalid")
			return
		}
		state = "disabling"
	}
	spec, _ := json.Marshal(p.Spec)
	_, err = tx.Exec(r.Context(), `INSERT INTO data_api_instances(branch_id,project_id,endpoint_id,generation,state,spec,secret_ref) VALUES($1,$2,$3,$4,$5,$6,$7)
        ON CONFLICT(branch_id) DO UPDATE SET generation=$4,state=$5,spec=$6,secret_ref=$7,endpoint_id=$3,updated_at=now()`, p.BranchID, p.ProjectID, p.EndpointID, p.Generation, state, spec, p.SecretRef)
	id := newID("op_")
	payload, _ := json.Marshal(p)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'data_api',$3,$4,'queued',$5,$6,$7)`, id, p.ProjectID, p.BranchID, action, userFrom(r).ID, requestID(r), payload)
	}
	for i, step := range []string{"reserve_service_credentials", "apply_restricted_database_identity", "publish_service_proxy_identity", "reconcile_service_workload", "verify_and_commit_service"} {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, id, i, step)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, userFrom(r).ID, keyHash(key), hash, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if s.replayDataAPI(w, r, key, hash) {
			return
		}
		fail(w, r, 409, "operation_conflict", "Could not admit service Operation")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, action, "data_api", p.BranchID, "accepted", requestID(r))
	s.acceptDataAPI(w, r, id)
}

func (s *server) reserveDataAPI(ctx context.Context, p dataAPIPayload) (map[string]any, error) {
	password := randomToken(32)
	verifier, err := scramVerifier(password)
	if err != nil {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err = rand.Read(seed); err != nil {
		return nil, err
	}
	cfg := dataAPIConfig(p)
	gateway, err := dataapi.New(cfg, seed, s.logger)
	if err != nil {
		return nil, err
	}
	config, _ := json.Marshal(cfg)
	// DSN is only placed in a restricted file, never argv, Pod env or logs.
	upstream := &url.URL{Scheme: "postgresql", User: url.UserPassword(dataAPILogin(p.BranchID), password), Host: s.proxyHost + ":" + s.proxyPort, Path: "/" + p.Spec.Database}
	q := upstream.Query()
	q.Set("sslmode", "require")
	q.Set("options", "endpoint="+selector(p.EndpointID))
	upstream.RawQuery = q.Encode()
	// A permanent LISTEN connection prevents Neon idle suspension. This static
	// schema profile reloads on an explicit disable/re-enable, rather than NOTIFY.
	postgrest := fmt.Sprintf("db-uri = %s\ndb-schemas = %s\ndb-pool = 1\ndb-pool-max-idletime = 1\ndb-channel-enabled = false\ndb-config = false\ndb-pre-request = %s\njwt-secret = \"@/run/data-api/delegation-jwks.json\"\njwt-aud = %s\njwt-role-claim-key = \"$$.role\"\njwt-cache-max-entries = 0\nserver-host = \"127.0.0.1\"\nserver-port = 3000\ndb-max-rows = 100\n", strconv.Quote(upstream.String()), strconv.Quote(p.Spec.Schema), strconv.Quote(dataAPIGuardSchema(p.BranchID)+".check_request"), strconv.Quote(p.Spec.Audience))
	return s.kube.createOwned(ctx, "secret", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
		"metadata":   map[string]any{"name": p.SecretRef, "labels": dataAPILabels(p)},
		"stringData": map[string]string{"password": password, "verifier": verifier, "delegation-seed": base64.StdEncoding.EncodeToString(seed), "delegation-jwks.json": string(gateway.DelegationJWKS()), "config.json": string(config), "postgrest.conf": postgrest}}, p.ProjectID, p.EndpointID)
}
func (s *server) dataAPISecret(ctx context.Context, p dataAPIPayload) (map[string]any, error) {
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", p.SecretRef), nil)
	if err != nil || !ownedDataAPI(secret, p) {
		return nil, errors.New("Data API Secret ownership mismatch")
	}
	return secret, nil
}

func (s *server) reconcileDataAPI(ctx context.Context, id, worker, action string, p dataAPIPayload) error {
	if !dataAPIEnabled() || validateDataAPI(p) != nil {
		return errors.New("Data API Driver configuration unavailable")
	}
	var branch string
	var generation int64
	if err := s.db.QueryRow(ctx, `SELECT branch_id,generation FROM data_api_instances WHERE branch_id=$1 AND project_id=$2 AND endpoint_id=$3 AND secret_ref=$4`, p.BranchID, p.ProjectID, p.EndpointID, p.SecretRef).Scan(&branch, &generation); err != nil || generation != p.Generation {
		return errors.New("Data API intent generation mismatch")
	}
	disabling := action == "disable_data_api"
	steps := []func(context.Context) error{
		func(ctx context.Context) error {
			if disabling {
				return nil
			}
			_, err := s.reserveDataAPI(ctx, p)
			return err
		},
		func(ctx context.Context) error {
			if disabling {
				return nil
			}
			return s.applyDataAPISQL(ctx, p)
		},
		func(ctx context.Context) error { return s.publishDataAPIIdentity(ctx, p, !disabling) },
		func(ctx context.Context) error { return s.reconcileDataAPIWorkload(ctx, p, !disabling) },
		func(ctx context.Context) error {
			if !disabling {
				// Deployment readiness uses only the gateway's configuration
				// listener. Verify the real database and restricted login once.
				if err := s.verifyDataAPILogin(ctx, p); err != nil {
					return err
				}
			}
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
				return err
			}
			state := "active"
			desired := "active"
			if disabling {
				state = "disabled"
				desired = "disabled"
			}
			tag, err := tx.Exec(ctx, `UPDATE data_api_instances SET state=$5,updated_at=now() WHERE branch_id=$1 AND project_id=$2 AND generation=$3 AND secret_ref=$4`, p.BranchID, p.ProjectID, p.Generation, p.SecretRef, state)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errLeaseLost
			}
			_, err = tx.Exec(ctx, `INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version,public_endpoint,version,last_observed_at)
                VALUES($1,'data_api',$2,$2,'native-data-api-v1',$3,$4,now()) ON CONFLICT(branch_id,service_kind) DO UPDATE SET desired_state=$2,observed_state=$2,driver_version='native-data-api-v1',public_endpoint=$3,version=$4,last_observed_at=now()`, p.BranchID, desired, "/data/v1/"+p.BranchID+"/", p.Generation)
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		},
	}
	for i, step := range steps {
		if err := s.createStep(ctx, id, worker, i, "data_api_reconcile", step); err != nil {
			return err
		}
	}
	return nil
}

// Public data traffic is independent of Console sessions. Only a fixed, owned
// branch service is reachable; the auth entry verifies the application JWT.
var dataAPIRelaySlots = make(chan struct{}, 64)

func (s *server) dataAPIRelay(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "HEAD", "POST", "PATCH", "DELETE", "OPTIONS":
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != "OPTIONS" && (len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || len(r.Header.Get("Authorization")) > 16384) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	select {
	case dataAPIRelaySlots <- struct{}{}:
		defer func() { <-dataAPIRelaySlots }()
	default:
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	branch := r.PathValue("dataBranch")
	if !dataAPIEnabled() || !dataBranchID.MatchString(branch) {
		http.NotFound(w, r)
		return
	}
	var state string
	var endpoint string
	if err := s.db.QueryRow(r.Context(), `SELECT state,endpoint_id FROM data_api_instances WHERE branch_id=$1`, branch).Scan(&state, &endpoint); err != nil || state != "active" {
		http.NotFound(w, r)
		return
	}
	target, _ := url.Parse("http://" + dataAPIName(branch) + "." + s.kube.namespace + ".svc.cluster.local:9080")
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		authorization := pr.In.Header.Get("Authorization")
		pr.SetURL(target)
		pr.Out.Host = target.Host
		pr.Out.Header.Set("Authorization", authorization)
		pr.Out.Header.Del("Cookie")
		pr.Out.Header.Del("Proxy-Authorization")
	}, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 130 * time.Second, MaxConnsPerHost: 16, DisableKeepAlives: true},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			fail(w, r, 503, "data_api_unavailable", "Data API runtime unavailable")
		}}
	// Internal request admission shares the existing suspend gate. This does
	// not claim to fence connections that bypass this relay through Proxy.
	release, err := s.acquireProbeGate(r.Context(), endpoint, true)
	if err != nil {
		fail(w, r, 503, "data_api_unavailable", "Could not admit data request")
		return
	}
	defer release()
	proxy.ServeHTTP(w, r)
}
