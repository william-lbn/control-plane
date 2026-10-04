package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type backendKeyring struct {
	Active  string            `json:"active"`
	Keys    map[string]string `json:"keys"`
	decoded map[string][]byte
}

var backendTokenPattern = regexp.MustCompile(`^ncb_[0-9a-f]{24}\.[0-9a-f]{64}$`)
var backendVersionPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var backendModelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,127}$`)
var backendIDPattern = regexp.MustCompile(`^bcr_[a-f0-9]{24}$`)

func parseBackendKeyring(raw []byte) (*backendKeyring, error) {
	if len(raw) > 16384 {
		return nil, errors.New("Backend keyring exceeds size bound")
	}
	var ring backendKeyring
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ring); err != nil {
		return nil, errors.New("Invalid Backend keyring")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("Unexpected Backend keyring content")
	}
	if len(ring.Keys) < 1 || len(ring.Keys) > 8 {
		return nil, errors.New("Backend keyring requires one to eight versions")
	}
	ring.decoded = map[string][]byte{}
	for version, encoded := range ring.Keys {
		if !backendVersionPattern.MatchString(version) {
			return nil, errors.New("Invalid Backend key version")
		}
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(value) < 32 || len(value) > 64 {
			return nil, errors.New("Backend pepper requires 32 to 64 random bytes")
		}
		ring.decoded[version] = value
	}
	if ring.decoded[ring.Active] == nil {
		return nil, errors.New("Active Backend pepper is missing")
	}
	ring.Keys = nil // Never retain serializable secret strings on the server.
	return &ring, nil
}

func (s *server) backendHash(domain, value, version string) string {
	mac := hmac.New(sha256.New, s.backendKeys.decoded[version])
	mac.Write([]byte(domain))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

type backendCredentialSpec struct {
	Name          string    `json:"name"`
	Scopes        []string  `json:"scopes"`
	BranchScope   string    `json:"branch_scope"`
	AllowedModels []string  `json:"allowed_models"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func validateBackendCredentialSpec(spec *backendCredentialSpec, now time.Time) error {
	spec.Name = strings.TrimSpace(spec.Name)
	if len(spec.Name) < 1 || len(spec.Name) > 100 || strings.ContainsAny(spec.Name, "\r\n\x00") {
		return errors.New("Name must contain 1 to 100 bytes")
	}
	if len(spec.Scopes) != 1 || spec.Scopes[0] != "ai_gateway:invoke" {
		return errors.New("This version supports only ai_gateway:invoke")
	}
	if spec.BranchScope != "self" && spec.BranchScope != "self_and_descendants" {
		return errors.New("Invalid branch scope")
	}
	if spec.ExpiresAt.IsZero() {
		return errors.New("Expiry is required")
	}
	if !now.IsZero() && (!spec.ExpiresAt.After(now.Add(time.Minute)) || spec.ExpiresAt.After(now.Add(30*24*time.Hour))) {
		return errors.New("Expiry must be between one minute and 30 days")
	}
	if len(spec.AllowedModels) > 100 {
		return errors.New("At most 100 model IDs are permitted")
	}
	seen := map[string]bool{}
	for _, model := range spec.AllowedModels {
		if !backendModelPattern.MatchString(model) || seen[model] {
			return errors.New("Invalid or duplicate model ID")
		}
		seen[model] = true
	}
	if spec.AllowedModels == nil {
		spec.AllowedModels = []string{}
	}
	sort.Strings(spec.AllowedModels)
	spec.ExpiresAt = spec.ExpiresAt.UTC().Truncate(time.Second)
	return nil
}

const backendPublicColumns = `id,project_id,org_id,branch_id,issuer_id,name,scopes,branch_scope,allowed_models,generation,expires_at,revoked_at,created_at,rotated_at,last_used_at,
 CASE WHEN revoked_at IS NOT NULL THEN 'revoked' WHEN expires_at<=now() THEN 'expired' ELSE 'active' END AS state`

func (s *server) backendCredentials(w http.ResponseWriter, r *http.Request) {
	project, branch := r.PathValue("project"), r.PathValue("branch")
	if _, err := s.one(r.Context(), `SELECT id FROM branches WHERE project_id=$1 AND id=$2 AND deleted_at IS NULL`, project, branch); err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Branch not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read branch")
		}
		return
	}
	if r.Method == http.MethodGet {
		cursor := r.URL.Query().Get("cursor")
		if cursor != "" && !backendIDPattern.MatchString(cursor) {
			fail(w, r, 422, "invalid_cursor", "Invalid credential cursor")
			return
		}
		items, err := s.many(r.Context(), `SELECT `+backendPublicColumns+` FROM backend_credentials WHERE project_id=$1 AND branch_id=$2 AND
          ($3='' OR (created_at,id)<(SELECT created_at,id FROM backend_credentials WHERE id=$3 AND project_id=$1 AND branch_id=$2))
          ORDER BY created_at DESC,id DESC LIMIT 101`, project, branch, cursor)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not list Backend credentials")
			return
		}
		more := len(items) > 100
		if more {
			items = items[:100]
		}
		for _, item := range items {
			item["can_manage"] = accessFrom(r).Level >= 3 || item["issuer_id"] == userFrom(r).ID
		}
		next := ""
		if more {
			next = stringVal(items[len(items)-1]["id"])
		}
		jsonResponse(w, 200, map[string]any{"items": items, "has_more": more, "next_cursor": next, "enabled": s.backendKeys != nil, "inference_available": false})
		return
	}
	s.mutateBackendCredential(w, r)
}

// Creation/rotation return plaintext exactly once. Durable replay contains only
// public metadata, so a lost initial response must be recovered by rotation.
func (s *server) mutateBackendCredential(w http.ResponseWriter, r *http.Request) {
	if s.backendKeys == nil {
		fail(w, r, 503, "backend_credentials_disabled", "Backend credential keyring is not configured")
		return
	}
	actor := userFrom(r)
	if actor.APIKey {
		fail(w, r, 403, "forbidden", "Console sessions are required to manage Backend credentials")
		return
	}
	idem := r.Header.Get("Idempotency-Key")
	if len(idem) < 8 || len(idem) > 128 || strings.ContainsAny(idem, "\r\n") {
		fail(w, r, 422, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 bytes")
		return
	}
	action := "create"
	if r.Method == http.MethodDelete {
		action = "revoke"
	} else if r.PathValue("credential") != "" {
		action = "rotate"
	}
	var spec backendCredentialSpec
	if action != "revoke" {
		if err := readJSON(r, &spec); err != nil {
			fail(w, r, 422, "invalid_request", "Invalid Backend credential request")
			return
		}
		if err := validateBackendCredentialSpec(&spec, time.Time{}); err != nil {
			fail(w, r, 422, "invalid_request", err.Error())
			return
		}
	}
	var expected int64
	if action != "create" {
		value := r.Header.Get("If-Match")
		if value == "" {
			fail(w, r, 428, "precondition_required", "If-Match is required")
			return
		}
		var err error
		if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
			fail(w, r, 422, "invalid_version", "If-Match must be a quoted generation")
			return
		}
		expected, err = strconv.ParseInt(value[1:len(value)-1], 10, 64)
		if err != nil || expected < 1 {
			fail(w, r, 422, "invalid_version", "Invalid generation")
			return
		}
	}
	canonical, _ := json.Marshal(map[string]any{"action": action, "project": r.PathValue("project"), "branch": r.PathValue("branch"), "id": r.PathValue("credential"), "expected": expected, "spec": spec})
	// Replay survives pepper activation changes by using the fixed idempotency
	// key. A missing idempotency key must fail closed, never use a public hash.
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "idempotency_unavailable", "Persistent idempotency key is required")
		return
	}
	keyHash := s.createRequestHash("backend-idempotency", idem)
	requestHash := s.createRequestHash("backend-request", json.RawMessage(canonical))
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not begin credential change")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, actor.ID+":"+keyHash); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock credential request")
		return
	}
	var priorHash string
	var prior []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM backend_credential_requests WHERE actor_id=$1 AND key_hash=$2`, actor.ID, keyHash).Scan(&priorHash, &prior)
	if err == nil {
		if priorHash != requestHash {
			fail(w, r, 409, "idempotency_conflict", "Idempotency-Key was used with different input")
			return
		}
		var reply map[string]any
		if json.Unmarshal(prior, &reply) != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not restore credential reply")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		jsonResponse(w, 200, reply)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, 503, "metadata_unavailable", "Could not check credential replay")
		return
	}
	if action != "revoke" {
		if err := validateBackendCredentialSpec(&spec, time.Now().UTC()); err != nil {
			fail(w, r, 422, "invalid_request", err.Error())
			return
		}
	}
	var organization string
	err = tx.QueryRow(r.Context(), `SELECT p.org_id FROM branches b JOIN projects p ON p.id=b.project_id WHERE b.id=$1 AND b.project_id=$2 AND b.deleted_at IS NULL AND b.state='ready' AND p.deleted_at IS NULL AND p.state='ready' FOR UPDATE OF b`, r.PathValue("branch"), r.PathValue("project")).Scan(&organization)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, r, 409, "branch_not_ready", "Branch must be ready")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not check credential branch")
		}
		return
	}
	id := r.PathValue("credential")
	generation := int64(1)
	if action != "create" {
		var issuer string
		var revoked *time.Time
		err = tx.QueryRow(r.Context(), `SELECT issuer_id,generation,revoked_at FROM backend_credentials WHERE id=$1 AND project_id=$2 AND branch_id=$3 FOR UPDATE`, id, r.PathValue("project"), r.PathValue("branch")).Scan(&issuer, &generation, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, r, 404, "not_found", "Credential not found")
			return
		}
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read credential")
			return
		}
		if issuer != actor.ID && accessFrom(r).Level < 3 {
			fail(w, r, 403, "forbidden", "Only the issuer or project Admin may change this credential")
			return
		}
		if generation != expected {
			fail(w, r, 412, "version_conflict", "Credential generation changed")
			return
		}
		if revoked != nil && action == "rotate" {
			fail(w, r, 409, "credential_revoked", "A revoked credential cannot be rotated")
			return
		}
		if revoked == nil {
			generation++
		}
	} else {
		id = newID("bcr_")
	}
	if action == "create" {
		var count int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM backend_credentials WHERE branch_id=$1 AND revoked_at IS NULL AND expires_at>now()`, r.PathValue("branch")).Scan(&count); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not check credential quota")
			return
		}
		if count >= 100 {
			fail(w, r, 429, "credential_quota_exceeded", "At most 100 active credentials per branch")
			return
		}
	}
	token := ""
	if action == "revoke" {
		_, err = tx.Exec(r.Context(), `UPDATE backend_credentials SET revoked_at=COALESCE(revoked_at,now()),generation=$2 WHERE id=$1`, id, generation)
	} else {
		token = "ncb_" + strings.TrimPrefix(id, "bcr_") + "." + randomToken(32)
		hash := s.backendHash("backend-token", token, s.backendKeys.Active)
		if action == "create" {
			_, err = tx.Exec(r.Context(), `INSERT INTO backend_credentials(id,project_id,org_id,branch_id,issuer_id,name,scopes,branch_scope,allowed_models,token_hash,pepper_version,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, r.PathValue("project"), organization, r.PathValue("branch"), actor.ID, spec.Name, spec.Scopes, spec.BranchScope, spec.AllowedModels, hash, s.backendKeys.Active, spec.ExpiresAt)
		} else {
			_, err = tx.Exec(r.Context(), `UPDATE backend_credentials SET name=$2,scopes=$3,branch_scope=$4,allowed_models=$5,token_hash=$6,pepper_version=$7,expires_at=$8,generation=$9,rotated_at=now() WHERE id=$1`, id, spec.Name, spec.Scopes, spec.BranchScope, spec.AllowedModels, hash, s.backendKeys.Active, spec.ExpiresAt, generation)
		}
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not write credential")
		return
	}
	rows, err := tx.Query(r.Context(), `SELECT `+backendPublicColumns+` FROM backend_credentials WHERE id=$1`, id)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read credential result")
		return
	}
	items, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil || len(items) != 1 {
		fail(w, r, 503, "metadata_unavailable", "Could not collect credential result")
		return
	}
	reply := map[string]any{"credential": items[0], "secret_available": false, "inference_available": false}
	encoded, _ := json.Marshal(reply)
	if _, err = tx.Exec(r.Context(), `INSERT INTO backend_credential_requests(actor_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4)`, actor.ID, keyHash, requestHash, encoded); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not persist credential replay")
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,$2,'backend_credential',$3,'succeeded',$4)`, actor.ID, action+"_backend_credential", id, requestID(r)); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not record credential audit")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not commit credential change")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, generation))
	code := 200
	if action == "create" {
		code = 201
	}
	if token != "" {
		reply["api_token"] = token
		reply["secret_available"] = true
	}
	jsonResponse(w, code, reply)
}

var errBackendCredentialInvalid = errors.New("invalid Backend credential")
var errBackendCredentialForbidden = errors.New("Backend credential not authorized")

// One PostgreSQL snapshot checks the current principal, branch lineage and
// credential version. No per-process positive cache can outlive a revocation.
func (s *server) verifyBackendCredential(ctx context.Context, token, target, scope, model string) (map[string]any, error) {
	if s.backendKeys == nil {
		return nil, errors.New("Backend credential keyring unavailable")
	}
	if !backendTokenPattern.MatchString(token) {
		return nil, errBackendCredentialInvalid
	}
	id := "bcr_" + token[4:28]
	var hash, version, orgRole, grant string
	var permitted bool
	var scopes, models []string
	var expiry time.Time
	err := s.db.QueryRow(ctx, `WITH RECURSIVE lineage AS (
      SELECT id,project_id,parent_branch_id,ARRAY[id] AS seen FROM branches WHERE id=$2 AND deleted_at IS NULL AND state='ready'
      UNION ALL SELECT b.id,b.project_id,b.parent_branch_id,l.seen||b.id FROM branches b JOIN lineage l ON b.id=l.parent_branch_id
      WHERE b.project_id=l.project_id AND b.deleted_at IS NULL AND b.state='ready' AND NOT b.id=ANY(l.seen) AND cardinality(l.seen)<128
    ) SELECT c.token_hash,c.pepper_version,m.role,COALESCE(g.role,''),c.scopes,c.allowed_models,c.expires_at,
      EXISTS(SELECT 1 FROM lineage l WHERE l.id=c.branch_id AND l.project_id=c.project_id AND (c.branch_scope='self_and_descendants' OR c.branch_id=$2))
      FROM backend_credentials c JOIN users u ON u.id=c.issuer_id AND NOT u.disabled
      JOIN projects p ON p.id=c.project_id AND p.org_id=c.org_id AND p.deleted_at IS NULL AND p.state='ready'
      JOIN organizations o ON o.id=c.org_id AND o.state='active'
      JOIN branches root ON root.id=c.branch_id AND root.project_id=c.project_id AND root.deleted_at IS NULL AND root.state='ready'
      JOIN organization_members m ON m.org_id=c.org_id AND m.user_id=c.issuer_id
      LEFT JOIN project_grants g ON g.project_id=c.project_id AND g.user_id=c.issuer_id
      WHERE c.id=$1 AND c.revoked_at IS NULL AND c.expires_at>now()`, id, target).Scan(&hash, &version, &orgRole, &grant, &scopes, &models, &expiry, &permitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errBackendCredentialInvalid
	}
	if err != nil {
		return nil, err
	}
	if s.backendKeys.decoded[version] == nil {
		return nil, errors.New("Backend pepper version unavailable")
	}
	if !hmac.Equal([]byte(hash), []byte(s.backendHash("backend-token", token, version))) {
		return nil, errBackendCredentialInvalid
	}
	hasScope := false
	for _, value := range scopes {
		if value == scope {
			hasScope = true
		}
	}
	hasModel := model == "" || len(models) == 0
	for _, value := range models {
		if value == model {
			hasModel = true
		}
	}
	if !permitted || effectivePermission(orgRole, grant) < 2 || !hasScope || !hasModel {
		return nil, errBackendCredentialForbidden
	}
	return map[string]any{"valid": true, "credential_id": id, "branch_id": target, "scopes": scopes, "expires_at": expiry, "inference_available": false}, nil
}

// A Console-only access check provides a real authentication/lineage probe.
// It cannot invoke a model and does not turn ai_gateway capability on.
func (s *server) checkBackendCredential(w http.ResponseWriter, r *http.Request) {
	if userFrom(r).APIKey {
		fail(w, r, 403, "forbidden", "A Console session is required")
		return
	}
	if _, err := s.one(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, r.PathValue("branch"), r.PathValue("project")); err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Branch not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not check branch")
		}
		return
	}
	var input struct {
		Token string `json:"api_token"`
		Model string `json:"model"`
	}
	if err := readJSON(r, &input); err != nil || len(input.Token) > 256 || len(input.Model) > 128 {
		fail(w, r, 422, "invalid_request", "Invalid credential check")
		return
	}
	result, err := s.verifyBackendCredential(r.Context(), input.Token, r.PathValue("branch"), "ai_gateway:invoke", input.Model)
	if errors.Is(err, errBackendCredentialInvalid) {
		fail(w, r, 401, "invalid_backend_credential", "Credential is invalid, expired or revoked")
		return
	}
	if errors.Is(err, errBackendCredentialForbidden) {
		fail(w, r, 403, "credential_scope_denied", "Credential does not authorize this branch or model")
		return
	}
	if err != nil {
		fail(w, r, 503, "credential_store_unavailable", "Could not verify Backend credential")
		return
	}
	jsonResponse(w, 200, result)
}

func loadBackendKeyring() (*backendKeyring, error) {
	path := os.Getenv("NEON_BACKEND_CREDENTIAL_KEYS_FILE")
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("Backend credential keyring cannot be read")
	}
	return parseBackendKeyring(raw)
}
