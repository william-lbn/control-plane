package control

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

type aiProviderWork struct {
	ID, ProviderID, RevisionID, ActorID, Lease                      string
	BaseURL, EndpointSHA256, Mode, SourceID, Previous, PolicySHA256 string
	Box                                                             *aigateway.SealedProviderCredential
}

func (s *server) runAIProviderController(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.aiProviderWorkOnce(ctx); err != nil {
				s.logger.Warn("Provider metadata operation could not be settled")
			}
		}
	}
}
func (s *server) aiProviderWorkOnce(ctx context.Context) (bool, error) {
	if s.aiProviders == nil || s.aiProviders.opener == nil {
		return false, nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var revision int
	if err = tx.QueryRow(ctx, `SELECT revision FROM ai_provider_catalog_state WHERE singleton FOR UPDATE`).Scan(&revision); err != nil {
		return false, err
	}
	var work aiProviderWork
	err = tx.QueryRow(ctx, `SELECT o.id,o.provider_id,o.revision_id,o.actor_id,r.base_url,r.endpoint_sha256,r.credential_mode,COALESCE(r.credential_source_id,''),COALESCE(r.previous_revision_id,''),r.policy_sha256,r.credential_box FROM ai_provider_operations o JOIN ai_provider_revisions r ON r.id=o.revision_id AND r.provider_id=o.provider_id WHERE o.state='queued' OR o.state='running' AND o.lease_expires_at<clock_timestamp() ORDER BY o.created_at FOR UPDATE OF o SKIP LOCKED LIMIT 1`).Scan(&work.ID, &work.ProviderID, &work.RevisionID, &work.ActorID, &work.BaseURL, &work.EndpointSHA256, &work.Mode, &work.SourceID, &work.Previous, &work.PolicySHA256, &work.Box)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	work.Lease = newID("apl_")
	_, err = tx.Exec(ctx, `UPDATE ai_provider_operations SET state='running',attempts=attempts+1,lease_owner=$2,lease_expires_at=clock_timestamp()+interval '60 seconds',error_code=NULL,retryable=false,finished_at=NULL WHERE id=$1`, work.ID, work.Lease)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return true, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	network, models, probeErr := s.aiProviderProbe(probeCtx, &work)
	settleCtx, close := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer close()
	return true, s.aiProviderSettle(settleCtx, work, network, models, probeErr)
}
func (s *server) aiProviderWorkCurrent(ctx context.Context, tx pgx.Tx, work aiProviderWork) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_provider_operations o JOIN ai_providers p ON p.id=o.provider_id JOIN users u ON u.id=o.actor_id AND NOT u.disabled WHERE o.id=$1 AND o.revision_id=$2 AND o.provider_id=$3 AND o.actor_id=$4 AND o.lease_owner=$5 AND o.state='running' AND o.lease_expires_at>clock_timestamp() AND p.state<>'deleted' AND p.target_revision_id=o.revision_id)`, work.ID, work.RevisionID, work.ProviderID, work.ActorID, work.Lease).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid || !s.aiProviders.operators[work.ActorID] || work.PolicySHA256 != s.aiProviders.policySHA256 {
		return aigateway.ProviderProbeError{Code: "provider_operation_superseded", Retryable: false}
	}
	return nil
}
func (s *server) aiProviderProbe(ctx context.Context, work *aiProviderWork) (aigateway.ResolvedProviderEndpoint, []string, error) {
	var empty aigateway.ResolvedProviderEndpoint
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, nil, err
	}
	defer tx.Rollback(ctx)
	if err = s.aiProviderWorkCurrent(ctx, tx, *work); err != nil {
		return empty, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, nil, err
	}
	network, err := s.aiProviders.network.Resolve(ctx, work.BaseURL)
	if err != nil || network.EndpointSHA256 != work.EndpointSHA256 {
		return empty, nil, aigateway.ProviderProbeError{Code: "provider_endpoint_rejected"}
	}
	scope := aigateway.ProviderCredentialScope{ProviderID: work.ProviderID, RevisionID: work.RevisionID, EndpointSHA256: work.EndpointSHA256}
	secret := ""
	sourceUID := ""
	if work.Mode != "none" {
		if work.Box != nil {
			secret, err = s.aiProviders.opener.Open(scope, *work.Box)
		} else {
			switch work.Mode {
			case "operator_reference":
				secret, sourceUID, err = s.aiProviderSourceCredential(ctx, work.SourceID, work.BaseURL)
			case "reuse":
				var box *aigateway.SealedProviderCredential
				var hash string
				err = s.db.QueryRow(ctx, `SELECT credential_box,endpoint_sha256 FROM ai_provider_revisions WHERE id=$1 AND provider_id=$2 AND base_url=$3 AND state='tested' AND credential_box IS NOT NULL AND policy_sha256=$4`, work.Previous, work.ProviderID, work.BaseURL, s.aiProviders.policySHA256).Scan(&box, &hash)
				if err == nil && box != nil && hash == work.EndpointSHA256 {
					secret, err = s.aiProviders.opener.Open(aigateway.ProviderCredentialScope{ProviderID: work.ProviderID, RevisionID: work.Previous, EndpointSHA256: hash}, *box)
				} else {
					err = errors.New("credential scope")
				}
			default:
				err = errors.New("credential missing")
			}
			if err == nil {
				var box aigateway.SealedProviderCredential
				box, err = s.aiProviders.sealer.Seal(scope, secret)
				if err == nil {
					work.Box = &box
				}
			}
			if err == nil {
				err = s.aiProviderFreezeCredential(ctx, *work, sourceUID)
			}
		}
		if err != nil {
			var probe aigateway.ProviderProbeError
			if errors.As(err, &probe) {
				return empty, nil, probe
			}
			return empty, nil, aigateway.ProviderProbeError{Code: "provider_credential_unavailable"}
		}
	}
	models, err := s.aiProviders.network.DiscoverModels(ctx, network, secret, 25*time.Second)
	secret = ""
	return network, models, err
}
func (s *server) aiProviderSourceCredential(ctx context.Context, id, baseURL string) (string, string, error) {
	source, ok := s.aiProviders.sourceFor(id, baseURL)
	if !ok || s.kube == nil || s.kube.namespace != source.Namespace {
		return "", "", errors.New("source rejected")
	}
	item, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", source.SecretName), nil)
	if err != nil {
		return "", "", aigateway.ProviderProbeError{Code: "provider_credential_source_unavailable", Retryable: true}
	}
	if nested(item, "metadata", "uid") != source.SecretUID || nested(item, "metadata", "name") != source.SecretName || nested(item, "metadata", "namespace") != source.Namespace || item["immutable"] != true || item["type"] != "Opaque" {
		return "", "", errors.New("source replaced")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(stringVal(nested(item, "data", source.DataKey)))
	if err != nil || len(raw) < 16 || len(raw) > 8192 {
		clear(raw)
		return "", "", errors.New("source invalid")
	}
	secret := string(raw)
	clear(raw)
	return secret, source.SecretUID, nil
}
func (s *server) aiProviderFreezeCredential(ctx context.Context, work aiProviderWork, sourceUID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var revision int
	if err = tx.QueryRow(ctx, `SELECT revision FROM ai_provider_catalog_state WHERE singleton FOR UPDATE`).Scan(&revision); err != nil {
		return err
	}
	if err = s.aiProviderWorkCurrent(ctx, tx, work); err != nil {
		return err
	}
	var uid any
	if sourceUID != "" {
		uid = sourceUID
	}
	tag, err := tx.Exec(ctx, `UPDATE ai_provider_revisions SET credential_box=$2,source_secret_uid=$3 WHERE id=$1 AND credential_box IS NULL AND state<>'tested'`, work.RevisionID, work.Box, uid)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return aigateway.ProviderProbeError{Code: "provider_operation_superseded"}
	}
	return tx.Commit(ctx)
}
func (s *server) aiProviderSettle(ctx context.Context, work aiProviderWork, network aigateway.ResolvedProviderEndpoint, models []string, probeErr error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var revision int
	if err = tx.QueryRow(ctx, `SELECT revision FROM ai_provider_catalog_state WHERE singleton FOR UPDATE`).Scan(&revision); err != nil {
		return err
	}
	if current := s.aiProviderWorkCurrent(ctx, tx, work); current != nil {
		probeErr = current
	}
	state, revisionState, code, retryable := "succeeded", "tested", "", false
	if probeErr != nil {
		state, revisionState, code = "failed", "failed", "provider_metadata_unavailable"
		var probe aigateway.ProviderProbeError
		if errors.As(probeErr, &probe) {
			code, retryable = probe.Code, probe.Retryable
		} else {
			retryable = true
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE ai_provider_operations SET state=$3,error_code=NULLIF($4,''),retryable=$5,finished_at=clock_timestamp(),lease_expires_at=NULL WHERE id=$1 AND state='running' AND lease_owner=$2`, work.ID, work.Lease, state, code, retryable)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if probeErr == nil {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_revisions SET state='tested',network=$2,models=$3,tested_at=clock_timestamp() WHERE id=$1 AND state<>'tested'`, work.RevisionID, network, models)
	} else {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_revisions SET state=$2 WHERE id=$1 AND state<>'tested'`, work.RevisionID, revisionState)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
