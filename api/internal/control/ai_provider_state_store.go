package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

const aiProviderHandleColumns = `id,kind,project_id,org_id,branch_id,actor_id,model_id,provider_id,provider_revision_id,provider_identity,key_version,request_id,COALESCE(parent_handle_id,''),COALESCE(previous_handle_id,''),expires_at,ciphertext,state,provider_status`

func scanAIProviderHandle(row pgx.Row) (h aiProviderHandle, state, status string, err error) {
	err = row.Scan(&h.ID, &h.Kind, &h.Project, &h.Org, &h.Branch, &h.Actor, &h.Model, &h.Provider, &h.Revision, &h.ProviderIdentity, &h.KeyVersion, &h.RequestID, &h.Parent, &h.Previous, &h.ExpiresAt, &h.Ciphertext, &state, &status)
	return
}

// The identity index survives deletion and key rotation. Its digest is private
// and is never returned as a public handle or a supplier credential.
func aiProviderRemoteIdentity(h aiProviderHandle, remote string) string {
	sum := sha256.Sum256([]byte(h.ProviderIdentity + "\x00" + remote))
	return hex.EncodeToString(sum[:])
}

func (s *server) insertAIProviderHandle(ctx context.Context, tx pgx.Tx, h *aiProviderHandle) error {
	if err := tx.QueryRow(ctx, `SELECT org_id FROM ai_input_file_scopes WHERE branch_id=$1 AND project_id=$2`, h.Branch, h.Project).Scan(&h.Org); err != nil {
		if isNoRows(err) {
			return providerProblem(404, "not_found", "Branch is not ready")
		}
		return err
	}
	var active, recent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state NOT IN ('deleted','revoked') AND expires_at>clock_timestamp()),count(*) FILTER(WHERE created_at>clock_timestamp()-interval '1 day') FROM ai_provider_handles WHERE branch_id=$1`, h.Branch).Scan(&active, &recent); err != nil {
		return err
	}
	if active >= 256 || recent >= 1024 {
		return providerProblem(429, "provider_state_quota_exceeded", "Delete unused references or wait for the daily quota")
	}
	_, err := tx.Exec(ctx, `INSERT INTO ai_provider_handles(id,kind,project_id,org_id,branch_id,actor_id,model_id,provider_id,provider_revision_id,provider_identity,key_version,request_id,parent_handle_id,previous_handle_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),NULLIF($14,''),$15)`, h.ID, h.Kind, h.Project, h.Org, h.Branch, h.Actor, h.Model, h.Provider, h.Revision, h.ProviderIdentity, h.KeyVersion, h.RequestID, h.Parent, h.Previous, h.ExpiresAt)
	return err
}

func (s *server) loadAIProviderHandle(ctx context.Context, tx pgx.Tx, id, project, branch string, route aigateway.Route, cleanup bool) (aiProviderHandle, aiProviderHandleData, error) {
	h, state, _, err := scanAIProviderHandle(tx.QueryRow(ctx, `SELECT `+aiProviderHandleColumns+` FROM ai_provider_handles WHERE id=$1 AND project_id=$2 AND branch_id=$3 FOR UPDATE`, id, project, branch))
	if isNoRows(err) {
		return h, aiProviderHandleData{}, providerProblem(404, "not_found", "Provider reference not found")
	}
	if err != nil {
		return h, aiProviderHandleData{}, err
	}
	if state == "deleted" {
		return h, aiProviderHandleData{}, providerProblem(410, "provider_state_deleted", "Provider reference was deleted")
	}
	if !cleanup && (!time.Now().Before(h.ExpiresAt) || state == "revoked") {
		return h, aiProviderHandleData{}, providerProblem(410, "provider_state_expired", "Reference expired or was revoked; explicit supplier cleanup remains available")
	}
	if state != "ready" && !(cleanup && (state == "uncertain" || state == "revoked")) {
		return h, aiProviderHandleData{}, providerProblem(409, "provider_state_uncertain", "The original supplier action must be reconciled; do not replay it")
	}
	if !route.AllowProviderState || route.StateIdentity() != h.ProviderIdentity {
		return h, aiProviderHandleData{}, providerProblem(409, "provider_state_route_changed", "Original supplier account or revision is no longer published")
	}
	data, err := s.aiProviderHandleOpen(h)
	return h, data, err
}

func (s *server) newAIProviderHandle(id, kind, request, project, branch, actor string, route aigateway.Route) aiProviderHandle {
	return aiProviderHandle{ID: id, Kind: kind, RequestID: request, Project: project, Branch: branch, Actor: actor, Model: route.ID, Provider: route.ProviderID, Revision: route.RevisionID, ProviderIdentity: route.StateIdentity(), KeyVersion: s.backendKeys.Active, ExpiresAt: time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)}
}

func (s *server) prepareAIProviderResponse(ctx context.Context, request, project, branch, actor string, route aigateway.Route, intent aigateway.ProviderStateIntent) (*aigateway.ProviderStateBinding, func(pgx.Tx) error) {
	h := s.newAIProviderHandle(newID("nrp_"), "response", request, project, branch, actor, route)
	h.Parent, h.Previous = intent.Conversation, intent.Previous
	b := &aigateway.ProviderStateBinding{Intent: intent, ResponseID: h.ID}
	return b, func(tx pgx.Tx) error {
		if !route.AllowProviderState {
			return providerProblem(403, "provider_state_disabled", "Supplier persistence is not approved for this model")
		}
		if err := tx.QueryRow(ctx, `SELECT org_id FROM ai_input_file_scopes WHERE branch_id=$1 AND project_id=$2`, branch, project).Scan(&h.Org); err != nil {
			return err
		}
		for _, id := range []string{intent.Previous, intent.Conversation} {
			if id == "" {
				continue
			}
			_, data, err := s.loadAIProviderHandle(ctx, tx, id, project, branch, route, false)
			if err != nil {
				return err
			}
			if id == intent.Previous {
				var status string
				if err = tx.QueryRow(ctx, `SELECT provider_status FROM ai_provider_handles WHERE id=$1`, id).Scan(&status); err != nil {
					return err
				}
				if status != "completed" && status != "incomplete" {
					return providerProblem(409, "provider_response_not_terminal", "Wait for the original response before continuing")
				}
				b.PreviousRemote = data.RemoteID
			} else {
				var busy bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_provider_state_mutations WHERE handle_id=$1 AND state IN ('pending','uncertain')) OR EXISTS(SELECT 1 FROM ai_provider_state_uses u JOIN ai_gateway_requests g ON g.id=u.request_id WHERE u.handle_id=$1 AND g.state='running')`, id).Scan(&busy); err != nil {
					return err
				}
				if busy {
					return providerProblem(409, "provider_state_in_use", "A conversation mutation or inference is still unresolved")
				}
				b.ConversationRemote = data.RemoteID
			}
			if _, err = tx.Exec(ctx, `INSERT INTO ai_provider_state_uses(request_id,handle_id,project_id,branch_id) VALUES($1,$2,$3,$4)`, request, id, project, branch); err != nil {
				return err
			}
		}
		var err error
		if intent.Store {
			if err = s.insertAIProviderHandle(ctx, tx, &h); err != nil {
				return err
			}
		}
		b.Proof, err = s.aiProviderHandleProof(h)
		if err != nil {
			return err
		}
		b.RewriteItems = s.aiProviderItemsRewriter(h)
		if intent.Store {
			b.CommitResponse = func(remote, status string) error {
				// Persist even after the browser disconnects. The same identity may
				// advance its status; it can never be replaced by another response.
				receipt, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return s.commitAIProviderHandle(receipt, h, remote, status, nil)
			}
		}
		return nil
	}
}

func (s *server) commitAIProviderHandle(ctx context.Context, h aiProviderHandle, remote, status string, metadata map[string]string) error {
	if err := s.aiProviderHandleSeal(&h, aiProviderHandleData{RemoteID: remote, Metadata: metadata}); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	stored, state, oldStatus, err := scanAIProviderHandle(tx.QueryRow(ctx, `SELECT `+aiProviderHandleColumns+` FROM ai_provider_handles WHERE id=$1 FOR UPDATE`, h.ID))
	if err != nil {
		return err
	}
	if state != "pending" && state != "ready" && state != "uncertain" {
		return errors.New("provider receipt is not writable")
	}
	if len(stored.Ciphertext) > 0 {
		data, e := s.aiProviderHandleOpen(stored)
		if e != nil || data.RemoteID != remote {
			return errAIProviderHandleEnvelope
		}
		if oldStatus == status && state == "ready" {
			return tx.Commit(ctx)
		}
		if oldStatus != status && (oldStatus == "completed" || oldStatus == "incomplete" || oldStatus == "failed" || oldStatus == "cancelled") {
			return errors.New("provider response status regressed")
		}
		_, err = tx.Exec(ctx, `UPDATE ai_provider_handles SET state='ready',provider_status=$2,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, h.ID, status)
	} else {
		_, err = tx.Exec(ctx, `UPDATE ai_provider_handles SET state='ready',provider_status=$2,ciphertext=$3,remote_identity_sha256=$4,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, h.ID, status, h.Ciphertext, aiProviderRemoteIdentity(h, remote))
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
