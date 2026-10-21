package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

var aiProviderID = regexp.MustCompile(`^aip_[a-f0-9]{24}$`)
var aiProviderRevisionID = regexp.MustCompile(`^apv_[a-f0-9]{24}$`)
var aiProviderOperationID = regexp.MustCompile(`^apo_[a-f0-9]{24}$`)
var aiProviderAliasID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,127}$`)

type aiProviderProblem struct {
	status        int
	code, message string
}

func (e aiProviderProblem) Error() string { return e.code }
func providerProblem(status int, code, message string) error {
	return aiProviderProblem{status, code, message}
}
func aiProviderFailure(w http.ResponseWriter, r *http.Request, err error) {
	var problem aiProviderProblem
	if errors.As(err, &problem) {
		fail(w, r, problem.status, problem.code, problem.message)
		return
	}
	fail(w, r, 503, "metadata_unavailable", "Provider catalog could not be committed")
}
func (s *server) requireAIProviderOperator(w http.ResponseWriter, r *http.Request) bool {
	if s.aiProviders == nil {
		fail(w, r, 503, "provider_management_disabled", "Provider management is not configured")
		return false
	}
	if !s.aiProviders.isOperator(userFrom(r)) {
		fail(w, r, 403, "platform_operator_required", "An explicitly approved platform operator session is required")
		return false
	}
	return true
}
func (s *server) aiProviderCapabilities(w http.ResponseWriter, r *http.Request) {
	p := s.aiProviders
	enabled := p != nil
	allowed := enabled && p.isOperator(userFrom(r))
	sources := []record{}
	if allowed {
		for _, source := range p.policy.CredentialSources {
			sources = append(sources, record{"id": source.ID, "name": source.Name, "allowed_base_urls": p.sources[source.ID].AllowedBaseURLs})
		}
	}
	jsonResponse(w, 200, record{"enabled": enabled, "can_manage": allowed, "credential_sources": sources, "raw_secret_input": enabled && s.secureCookies && r.TLS != nil && r.TLS.HandshakeComplete, "lab_secret_input": allowed && p.policy.AllowLabSecretInput, "protocols": []string{"openai_compatible"}, "discovery_performs_inference": false})
}

// All mutations lock the catalog singleton, then replay the actor-scoped keyed
// receipt before CAS. No raw provider key or unkeyed password hash is retained.
func (s *server) aiProviderMutation(w http.ResponseWriter, r *http.Request, input any, fn func(pgx.Tx) (record, int, error)) {
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	if len(s.idempotencyKey) < 32 {
		aiProviderFailure(w, r, errors.New("key unavailable"))
		return
	}
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, input)
	actor := userFrom(r).ID
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The independent Gateway has no UPDATE right on the catalog singleton.
	// Shared/exclusive advisory admission locks fence publication without
	// expanding that restricted metadata role's table privileges.
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(794210047)`); err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	var revision int
	if err = tx.QueryRow(r.Context(), `SELECT revision FROM ai_provider_catalog_state WHERE singleton FOR UPDATE`).Scan(&revision); err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	var stored string
	var response []byte
	var status int
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response,status FROM ai_provider_requests WHERE actor_id=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&stored, &response, &status)
	if err == nil {
		if stored != hash {
			fail(w, r, 409, "idempotency_conflict", "Key used with different provider input")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(response)
		return
	}
	if !isNoRows(err) {
		aiProviderFailure(w, r, err)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), `SELECT NOT disabled FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&active); err != nil || !active {
		fail(w, r, 403, "platform_operator_required", "Operator account is no longer active")
		return
	}
	value, status, err := fn(tx)
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	response, err = json.Marshal(value)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_provider_requests(actor_id,key_hash,request_hash,status,response) VALUES($1,$2,$3,$4,$5)`, actor, keyHash(key), hash, status, response)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	jsonResponse(w, status, value)
}

type aiProviderCredentialInput struct {
	Mode            string `json:"mode"`
	Secret          string `json:"secret,omitempty"`
	SourceID        string `json:"source_id,omitempty"`
	ReuseRevisionID string `json:"reuse_revision_id,omitempty"`
	LabAcknowledged bool   `json:"lab_secret_acknowledged,omitempty"`
}
type aiProviderDraftInput struct {
	Version    int64                     `json:"version"`
	Name       string                    `json:"name"`
	BaseURL    string                    `json:"base_url"`
	Credential aiProviderCredentialInput `json:"credential"`
}
type aiProviderModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	UpstreamModel   string `json:"upstream_model"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}
type aiProviderPublishInput struct {
	Version    int64             `json:"version"`
	RevisionID string            `json:"revision_id"`
	Models     []aiProviderModel `json:"models"`
}
type aiProviderVersionInput struct {
	Version int64 `json:"version"`
}

func providerNameValid(name string) bool {
	if len(name) < 1 || len(name) > 100 || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func providerLocked(ctx context.Context, tx pgx.Tx, id string, version int64) (string, error) {
	var current int64
	var state string
	err := tx.QueryRow(ctx, `SELECT version,state FROM ai_providers WHERE id=$1 FOR UPDATE`, id).Scan(&current, &state)
	if isNoRows(err) || err == nil && state == "deleted" {
		return "", providerProblem(404, "not_found", "Provider not found")
	}
	if err != nil {
		return "", err
	}
	if current != version {
		return "", providerProblem(409, "provider_version_conflict", "Provider changed; refresh before applying this action")
	}
	return state, nil
}
func (s *server) aiProviderDraft(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	var input aiProviderDraftInput
	if readJSON(r, &input) != nil {
		fail(w, r, 422, "invalid_request", "Invalid provider draft")
		return
	}
	id := r.PathValue("provider")
	creating := id == ""
	if !creating && !aiProviderID.MatchString(id) {
		fail(w, r, 404, "not_found", "Provider not found")
		return
	}
	s.aiProviderMutation(w, r, input, func(tx pgx.Tx) (record, int, error) {
		if !providerNameValid(input.Name) || creating && input.Version != 0 || !creating && input.Version < 1 {
			return nil, 0, providerProblem(422, "invalid_request", "Provider name and current version are required")
		}
		if creating {
			var count int
			if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM ai_providers WHERE state<>'deleted'`).Scan(&count); err != nil {
				return nil, 0, err
			}
			if count >= 16 {
				return nil, 0, providerProblem(429, "provider_quota_exceeded", "At most 16 provider definitions are allowed")
			}
			id = newID("aip_")
		} else if _, err := providerLocked(r.Context(), tx, id, input.Version); err != nil {
			return nil, 0, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		network, err := s.aiProviders.network.Resolve(ctx, input.BaseURL)
		if err != nil {
			return nil, 0, providerProblem(422, "provider_endpoint_rejected", "Use HTTPS /v1 with public addresses, or an exact operator-approved private endpoint")
		}
		revision, operation := newID("apv_"), newID("apo_")
		var sealed any
		var source, previous any
		credential := input.Credential
		switch credential.Mode {
		case "none":
			if credential.Secret != "" || credential.SourceID != "" || credential.ReuseRevisionID != "" {
				return nil, 0, providerProblem(422, "invalid_credential", "Credential mode fields conflict")
			}
		case "sealed":
			if credential.SourceID != "" || credential.ReuseRevisionID != "" {
				return nil, 0, providerProblem(422, "invalid_credential", "Credential mode fields conflict")
			}
			if !s.aiProviders.permitsSecretInput(r, network.BaseURL, credential.LabAcknowledged, s.secureCookies) {
				return nil, 0, providerProblem(422, "secure_secret_transport_required", "Use a TLS Console connection or an approved existing credential source")
			}
			box, err := s.aiProviders.sealer.Seal(aigateway.ProviderCredentialScope{ProviderID: id, RevisionID: revision, EndpointSHA256: network.EndpointSHA256}, credential.Secret)
			if err != nil {
				return nil, 0, providerProblem(422, "invalid_credential", "Provider key must contain 16-8192 printable characters")
			}
			sealed = box
		case "operator_reference":
			if credential.Secret != "" || credential.ReuseRevisionID != "" {
				return nil, 0, providerProblem(422, "invalid_credential", "Credential mode fields conflict")
			}
			if _, ok := s.aiProviders.sourceFor(credential.SourceID, network.BaseURL); !ok {
				return nil, 0, providerProblem(422, "credential_source_rejected", "Credential source is not approved for this endpoint")
			}
			source = credential.SourceID
		case "reuse":
			if creating || credential.Secret != "" || credential.SourceID != "" || !aiProviderRevisionID.MatchString(credential.ReuseRevisionID) {
				return nil, 0, providerProblem(422, "invalid_credential", "Select a tested revision of this provider")
			}
			var valid bool
			err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM ai_provider_revisions WHERE id=$1 AND provider_id=$2 AND state='tested' AND credential_box IS NOT NULL AND base_url=$3 AND endpoint_sha256=$4 AND policy_sha256=$5)`, credential.ReuseRevisionID, id, network.BaseURL, network.EndpointSHA256, s.aiProviders.policySHA256).Scan(&valid)
			if err != nil {
				return nil, 0, err
			}
			if !valid {
				return nil, 0, providerProblem(422, "credential_reuse_rejected", "A tested credential can only be reused at the same approved endpoint")
			}
			previous = credential.ReuseRevisionID
		default:
			return nil, 0, providerProblem(422, "invalid_credential", "Choose no authentication, a new key, an approved source or the tested credential")
		}
		if creating {
			_, err = tx.Exec(r.Context(), `INSERT INTO ai_providers(id,name,created_by) VALUES($1,$2,$3)`, id, input.Name, userFrom(r).ID)
			if err != nil {
				return nil, 0, err
			}
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_provider_revisions(id,provider_id,base_url,endpoint_sha256,credential_mode,credential_source_id,previous_revision_id,credential_box,policy_sha256,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, revision, id, network.BaseURL, network.EndpointSHA256, credential.Mode, source, previous, sealed, s.aiProviders.policySHA256, userFrom(r).ID)
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO ai_provider_operations(id,provider_id,revision_id,actor_id) VALUES($1,$2,$3,$4)`, operation, id, revision, userFrom(r).ID)
		}
		version := int64(1)
		if !creating {
			version = input.Version + 1
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE ai_providers SET name=$2,target_revision_id=$3,version=$4,updated_at=clock_timestamp() WHERE id=$1`, id, input.Name, revision, version)
		}
		return record{"provider_id": id, "revision_id": revision, "operation_id": operation, "version": version}, 202, err
	})
}

const aiProviderReadColumns = `id,name,version,state,active_publication_id,target_revision_id,created_by,created_at,updated_at`

func (s *server) aiProviderRead(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	id := r.PathValue("provider")
	if id == "" {
		items, err := s.many(r.Context(), `SELECT `+aiProviderReadColumns+` FROM ai_providers WHERE state<>'deleted' ORDER BY created_at,id`)
		if err != nil {
			aiProviderFailure(w, r, err)
			return
		}
		jsonResponse(w, 200, record{"items": items})
		return
	}
	if !aiProviderID.MatchString(id) {
		fail(w, r, 404, "not_found", "Provider not found")
		return
	}
	provider, err := s.one(r.Context(), `SELECT `+aiProviderReadColumns+` FROM ai_providers WHERE id=$1 AND state<>'deleted'`, id)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Provider not found")
		return
	}
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	revisions, err := s.many(r.Context(), `SELECT r.id,r.provider_id,r.base_url,r.credential_mode,r.credential_source_id,(r.credential_mode<>'none') AS has_credential,r.state,r.models,r.tested_at,r.created_at,o.id AS operation_id,o.state AS operation_state,o.attempts,o.retryable,o.error_code FROM ai_provider_revisions r JOIN ai_provider_operations o ON o.revision_id=r.id WHERE r.provider_id=$1 ORDER BY r.created_at DESC LIMIT 50`, id)
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	publications, err := s.many(r.Context(), `SELECT id,revision_id,models,created_by,created_at FROM ai_provider_publications WHERE provider_id=$1 ORDER BY created_at DESC LIMIT 50`, id)
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, record{"provider": provider, "revisions": revisions, "publications": publications})
}
func (s *server) aiProviderOperation(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	id := r.PathValue("providerOperation")
	if !aiProviderOperationID.MatchString(id) {
		fail(w, r, 404, "not_found", "Provider operation not found")
		return
	}
	item, err := s.one(r.Context(), `SELECT id,provider_id,revision_id,state,attempts,retryable,error_code,created_at,finished_at FROM ai_provider_operations WHERE id=$1`, id)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Provider operation not found")
		return
	}
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, item)
}
func (s *server) aiProviderRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	id := r.PathValue("providerOperation")
	var input struct{}
	if !aiProviderOperationID.MatchString(id) || readJSON(r, &input) != nil {
		fail(w, r, 422, "invalid_request", "Invalid provider retry")
		return
	}
	s.aiProviderMutation(w, r, input, func(tx pgx.Tx) (record, int, error) {
		tag, err := tx.Exec(r.Context(), `UPDATE ai_provider_operations o SET state='queued',error_code=NULL,retryable=false,finished_at=NULL,lease_owner=NULL,lease_expires_at=NULL FROM ai_providers p WHERE o.id=$1 AND o.actor_id=$2 AND o.state='failed' AND o.retryable AND o.attempts<5 AND p.id=o.provider_id AND p.state<>'deleted' AND p.target_revision_id=o.revision_id`, id, userFrom(r).ID)
		if err != nil {
			return nil, 0, err
		}
		if tag.RowsAffected() != 1 {
			return nil, 0, providerProblem(409, "provider_retry_rejected", "Only the original operator can retry a current transient failure, up to five attempts")
		}
		return record{"operation_id": id}, 202, nil
	})
}

func (s *server) aiProviderPublish(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	id := r.PathValue("provider")
	var input aiProviderPublishInput
	if !aiProviderID.MatchString(id) || readJSON(r, &input) != nil {
		fail(w, r, 422, "invalid_request", "Invalid publication")
		return
	}
	s.aiProviderMutation(w, r, input, func(tx pgx.Tx) (record, int, error) {
		if _, err := providerLocked(r.Context(), tx, id, input.Version); err != nil {
			return nil, 0, err
		}
		var models []string
		var network aigateway.ResolvedProviderEndpoint
		var policy string
		err := tx.QueryRow(r.Context(), `SELECT r.models,r.network,r.policy_sha256 FROM ai_provider_revisions r JOIN ai_providers p ON p.id=r.provider_id AND p.target_revision_id=r.id WHERE r.id=$1 AND r.provider_id=$2 AND r.state='tested'`, input.RevisionID, id).Scan(&models, &network, &policy)
		if isNoRows(err) {
			return nil, 0, providerProblem(409, "provider_revision_not_tested", "Publish only the current successfully tested revision")
		}
		if err != nil {
			return nil, 0, err
		}
		if policy != s.aiProviders.policySHA256 || s.aiProviders.network.ValidateResolved(network) != nil {
			return nil, 0, providerProblem(409, "provider_policy_changed", "Create and test a fresh revision under the current endpoint policy")
		}
		existing := map[string]bool{}
		if s.ai != nil {
			for _, route := range s.ai.Routes {
				existing[route.ID] = true
			}
		}
		rows, err := tx.Query(r.Context(), `SELECT pub.models FROM ai_providers p JOIN ai_provider_publications pub ON pub.id=p.active_publication_id AND pub.provider_id=p.id WHERE p.state='active' AND p.id<>$1`, id)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var entries []aiProviderModel
			if err = rows.Scan(&entries); err != nil {
				break
			}
			for _, entry := range entries {
				existing[entry.ID] = true
			}
		}
		rows.Close()
		if err != nil {
			return nil, 0, err
		}
		if rows.Err() != nil {
			return nil, 0, rows.Err()
		}
		if err = validateProviderPublication(input.Models, models, existing); err != nil {
			return nil, 0, err
		}
		publication := newID("app_")
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_provider_publications(id,provider_id,revision_id,models,created_by) VALUES($1,$2,$3,$4,$5)`, publication, id, input.RevisionID, input.Models, userFrom(r).ID)
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE ai_providers SET state='active',active_publication_id=$2,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, id, publication)
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE ai_provider_catalog_state SET revision=revision+1 WHERE singleton`)
		}
		return record{"provider_id": id, "publication_id": publication, "revision_id": input.RevisionID, "version": input.Version + 1, "state": "active"}, 200, err
	})
}
func validateProviderPublication(entries []aiProviderModel, discovered []string, existing map[string]bool) error {
	if len(entries)+len(existing) > 100 {
		return providerProblem(429, "model_catalog_quota_exceeded", "At most 100 active model aliases are allowed")
	}
	if len(entries) < 1 || len(entries) > 16 {
		return providerProblem(422, "invalid_models", "Publish 1-16 explicit model aliases")
	}
	available := map[string]bool{}
	for _, id := range discovered {
		available[id] = true
	}
	for _, entry := range entries {
		if !aiProviderAliasID.MatchString(entry.ID) || !providerNameValid(entry.Name) || !available[entry.UpstreamModel] || entry.MaxOutputTokens < 1 || entry.MaxOutputTokens > 4096 {
			return providerProblem(422, "invalid_models", "Aliases, names, discovered model IDs and output limits are required")
		}
		if existing[entry.ID] {
			return providerProblem(409, "model_alias_conflict", "Model alias already belongs to an approved route")
		}
		existing[entry.ID] = true
	}
	return nil
}
func (s *server) aiProviderDisable(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	id := r.PathValue("provider")
	var input aiProviderVersionInput
	if !aiProviderID.MatchString(id) || readJSON(r, &input) != nil {
		fail(w, r, 422, "invalid_request", "Current provider version is required")
		return
	}
	s.aiProviderMutation(w, r, input, func(tx pgx.Tx) (record, int, error) {
		state, err := providerLocked(r.Context(), tx, id, input.Version)
		if err != nil {
			return nil, 0, err
		}
		next := "disabled"
		if r.Method == http.MethodDelete {
			if state == "active" {
				return nil, 0, providerProblem(409, "provider_still_active", "Disable the provider before deleting its definition")
			}
			next = "deleted"
		}
		_, err = tx.Exec(r.Context(), `UPDATE ai_providers SET state=$2,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, id, next)
		if err == nil && state == "active" {
			_, err = tx.Exec(r.Context(), `UPDATE ai_provider_catalog_state SET revision=revision+1 WHERE singleton`)
		}
		return record{"provider_id": id, "state": next, "version": input.Version + 1}, 200, err
	})
}
