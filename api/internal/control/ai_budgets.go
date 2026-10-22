package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

var errAIBudgetExceeded = errors.New("AI budget exceeded")
var errAIBudgetPrice = errors.New("AI budget requires versioned model pricing")

func aiBranchLock(ctx context.Context, tx pgx.Tx, branch string) error {
	var org string
	err := tx.QueryRow(ctx, `SELECT p.org_id FROM branches b JOIN projects p ON p.id=b.project_id WHERE b.id=$1`, branch).Scan(&org)
	if err != nil && !isNoRows(err) {
		return err
	}
	if org != "" {
		if err = aiCreditOrgLock(ctx, tx, org); err != nil {
			return err
		}
	}
	h := sha256.Sum256([]byte("ai-branch:" + branch))
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, hex.EncodeToString(h[:]))
	return err
}

func reserveAIBudget(ctx context.Context, tx pgx.Tx, id, branch string, route aigateway.Route, maxOutput int) error {
	var enabled bool
	var limit, spent, held int64
	err := tx.QueryRow(ctx, `SELECT enabled,limit_micros,spent_micros,reserved_micros FROM ai_budget_policies WHERE branch_id=$1 FOR UPDATE`, branch).Scan(&enabled, &limit, &spent, &held)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if route.Price == nil {
		return errAIBudgetPrice
	}
	amount, err := route.Price.Reserve(maxOutput)
	if err != nil {
		return errAIBudgetPrice
	}
	if spent > limit || held > limit-spent || amount > limit-spent-held {
		return errAIBudgetExceeded
	}
	if _, err = tx.Exec(ctx, `UPDATE ai_budget_policies SET reserved_micros=reserved_micros+$2 WHERE branch_id=$1`, branch, amount); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ai_budget_reservations(request_id,branch_id,price,reserved_micros,state) VALUES($1,$2,$3,$4,'reserved')`, id, branch, route.Price, amount); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ai_budget_ledger(id,branch_id,request_id,kind,reserved_delta,spent_delta) VALUES($1,$2,$3,'reserve',$4,0)`, newID("abl_"), branch, id, amount)
	return err
}

// Every mutation holds the same branch lock used by admission. Unknown or
// accepted-but-disconnected calls keep funds reserved without a TTL refund.
func settleAIBudget(ctx context.Context, tx pgx.Tx, id string, outcome aigateway.Outcome, actor, evidence string, reconcile bool) error {
	var branch, state string
	var amount int64
	var price aigateway.Price
	err := tx.QueryRow(ctx, `SELECT branch_id,state,reserved_micros,price FROM ai_budget_reservations WHERE request_id=$1 FOR UPDATE`, id).Scan(&branch, &state, &amount, &price)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "settled" || state == "released" {
		if reconcile {
			return providerProblem(409, "budget_already_settled", "A final settlement cannot be overwritten")
		}
		return nil
	}
	kind, next := "uncertain", "uncertain"
	heldDelta, spentDelta := int64(0), int64(0)
	var actual any
	var usage any
	if outcome.Usage != nil {
		cost, e := price.Cost(*outcome.Usage)
		if e != nil {
			return e
		}
		spentDelta = cost
		heldDelta = -amount
		actual = cost
		usage = outcome.Usage
		kind, next = "settle", "settled"
	} else if !outcome.Dispatched {
		heldDelta = -amount
		actual = int64(0)
		kind, next = "release", "released"
	}
	if next == state && !reconcile {
		return nil
	}
	if reconcile {
		kind = "reconcile"
	}
	if _, err = tx.Exec(ctx, `UPDATE ai_budget_reservations SET state=$2,actual_micros=$3,usage=$4,settled_at=CASE WHEN $2 IN ('settled','released') THEN clock_timestamp() ELSE NULL END WHERE request_id=$1`, id, next, actual, usage); err != nil {
		return err
	}
	if heldDelta != 0 || spentDelta != 0 {
		if _, err = tx.Exec(ctx, `UPDATE ai_budget_policies SET reserved_micros=reserved_micros+$2,spent_micros=spent_micros+$3 WHERE branch_id=$1`, branch, heldDelta, spentDelta); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO ai_budget_ledger(id,branch_id,request_id,kind,reserved_delta,spent_delta,actor_id,evidence_sha256) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''))`, newID("abl_"), branch, id, kind, heldDelta, spentDelta, actor, evidence)
	return err
}

func (s *server) settleAIRequest(ctx context.Context, id, branch string, outcome aigateway.Outcome, duration time.Duration) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = aiBranchLock(ctx, tx, branch); err != nil {
		return err
	}
	var prompt, completion, total, read, write any
	if outcome.Usage != nil {
		prompt = outcome.Usage.PromptTokens
		completion = outcome.Usage.CompletionTokens
		total = outcome.Usage.TotalTokens
		read = outcome.Usage.CacheReadTokens
		write = outcome.Usage.CacheWriteTokens
	}
	if _, err = tx.Exec(ctx, `UPDATE ai_gateway_requests SET state=$2,finished_at=clock_timestamp(),prompt_tokens=$3,completion_tokens=$4,total_tokens=$5,duration_ms=$6,first_token_ms=$7,upstream_status=$8,cache_read_tokens=$9,cache_write_tokens=$10 WHERE id=$1 AND state='running'`, id, outcome.Status, prompt, completion, total, duration.Milliseconds(), outcome.FirstTokenMillis, outcome.UpstreamStatus, read, write); err != nil {
		return err
	}
	if err = settleAIBudget(ctx, tx, id, outcome, "", "", false); err != nil {
		return err
	}
	if err = settleAICredits(ctx, tx, id, outcome, "", "", false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *server) aiBudgetRead(w http.ResponseWriter, r *http.Request) {
	branch, project := r.PathValue("branch"), r.PathValue("project")
	if _, err := s.one(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, branch, project); err != nil {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	policy, err := s.one(r.Context(), `SELECT branch_id,version,enabled,currency,limit_micros,spent_micros,reserved_micros,updated_at FROM ai_budget_policies WHERE branch_id=$1 AND project_id=$2`, branch, project)
	if isNoRows(err) {
		policy = record{"branch_id": branch, "version": int64(0), "enabled": false, "currency": "USD", "limit_micros": int64(0), "spent_micros": int64(0), "reserved_micros": int64(0)}
	} else if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read budget")
		return
	}
	items, err := s.many(r.Context(), `SELECT b.request_id,b.state,b.price,b.reserved_micros,b.actual_micros,b.usage,b.created_at,b.settled_at,(b.state='uncertain' OR b.state='reserved' AND a.expires_at<clock_timestamp()) AS needs_reconciliation FROM ai_budget_reservations b JOIN ai_gateway_requests a ON a.id=b.request_id WHERE b.branch_id=$1 ORDER BY b.created_at DESC LIMIT 50`, branch)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read reservations")
		return
	}
	jsonResponse(w, 200, record{"policy": policy, "reservations": items, "payment_billing_enabled": false, "can_configure": accessFrom(r).Level >= 3, "can_reconcile": s.aiProviders != nil && s.aiProviders.isOperator(userFrom(r)), "expiry_releases_funds": false})
}

func (s *server) aiBudgetMutation(w http.ResponseWriter, r *http.Request, input any, fn func(pgx.Tx) (record, error)) {
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "budget_unavailable", "Budget signing key unavailable")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err = aiBranchLock(r.Context(), tx, r.PathValue("branch")); err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	actor := userFrom(r).ID
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, input)
	var previous string
	var response []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM ai_budget_mutations WHERE actor_id=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&previous, &response)
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key already used with different budget input")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.Write(response)
		return
	}
	if !isNoRows(err) {
		aiProviderFailure(w, r, err)
		return
	}
	var branch string
	if err = tx.QueryRow(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND state='ready' AND deleted_at IS NULL FOR SHARE`, r.PathValue("branch"), r.PathValue("project")).Scan(&branch); err != nil {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	value, err := fn(tx)
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	response, err = json.Marshal(value)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_budget_mutations(actor_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4)`, actor, keyHash(key), hash, response)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, value)
}

func (s *server) aiBudgetPut(w http.ResponseWriter, r *http.Request) {
	if accessFrom(r).Level < 3 {
		fail(w, r, 403, "forbidden", "Budget policy requires a project administrator")
		return
	}
	var input struct {
		Version *int64 `json:"version"`
		Enabled *bool  `json:"enabled"`
		Limit   *int64 `json:"limit_micros"`
	}
	if readJSON(r, &input) != nil || input.Version == nil || input.Enabled == nil || input.Limit == nil || *input.Version < 0 || *input.Limit < 0 || *input.Limit > 1000000000000 {
		fail(w, r, 422, "invalid_request", "Use the current version and an integer micro-USD budget")
		return
	}
	s.aiBudgetMutation(w, r, input, func(tx pgx.Tx) (record, error) {
		var version int64
		err := tx.QueryRow(r.Context(), `SELECT version FROM ai_budget_policies WHERE branch_id=$1 FOR UPDATE`, r.PathValue("branch")).Scan(&version)
		if err != nil && !isNoRows(err) {
			return nil, err
		}
		if version != *input.Version {
			return nil, providerProblem(409, "budget_version_conflict", "Budget changed; refresh before editing")
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_budget_policies(branch_id,project_id,version,enabled,currency,limit_micros,updated_by) VALUES($1,$2,1,$3,'USD',$4,$5) ON CONFLICT(branch_id) DO UPDATE SET version=ai_budget_policies.version+1,enabled=EXCLUDED.enabled,limit_micros=EXCLUDED.limit_micros,updated_by=EXCLUDED.updated_by,updated_at=clock_timestamp()`, r.PathValue("branch"), r.PathValue("project"), *input.Enabled, *input.Limit, userFrom(r).ID)
		return record{"branch_id": r.PathValue("branch"), "version": version + 1, "enabled": *input.Enabled, "limit_micros": *input.Limit, "currency": "USD"}, err
	})
}

var budgetEvidence = regexp.MustCompile(`^[a-f0-9]{64}$`)

type aiConfirmedUsage struct {
	Prompt     *int64 `json:"prompt_tokens"`
	Completion *int64 `json:"completion_tokens"`
	Total      *int64 `json:"total_tokens"`
	CacheRead  int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite int64  `json:"cache_write_tokens,omitempty"`
}

func (s *server) aiBudgetReconcile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	var input struct {
		Usage       *aiConfirmedUsage `json:"usage,omitempty"`
		NotAccepted bool              `json:"confirmed_not_accepted"`
		Evidence    string            `json:"evidence_sha256"`
	}
	if readJSON(r, &input) != nil || !budgetEvidence.MatchString(input.Evidence) || (input.Usage == nil) == !input.NotAccepted {
		fail(w, r, 422, "invalid_request", "Supply confirmed usage or confirmed non-acceptance and an evidence SHA-256")
		return
	}
	var confirmed *aigateway.Usage
	if input.Usage != nil {
		u := input.Usage
		if u.Prompt == nil || u.Completion == nil || u.Total == nil {
			fail(w, r, 422, "invalid_usage", "Confirmed usage requires every token count")
			return
		}
		confirmed = &aigateway.Usage{PromptTokens: *u.Prompt, CompletionTokens: *u.Completion, TotalTokens: *u.Total, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite}
		if _, err := (aigateway.Price{Version: "validate", Currency: "USD", MaxInputTokens: 1}).Cost(*confirmed); err != nil {
			fail(w, r, 422, "invalid_usage", "Confirmed usage must contain complete consistent non-negative counts")
			return
		}
	}
	s.aiBudgetMutation(w, r, input, func(tx pgx.Tx) (record, error) {
		id := r.PathValue("aiRequest")
		var state string
		var expired bool
		err := tx.QueryRow(r.Context(), `SELECT b.state,a.expires_at<clock_timestamp() FROM ai_budget_reservations b JOIN ai_gateway_requests a ON a.id=b.request_id WHERE b.request_id=$1 AND b.branch_id=$2 AND a.project_id=$3 FOR UPDATE OF b`, id, r.PathValue("branch"), r.PathValue("project")).Scan(&state, &expired)
		if isNoRows(err) {
			return nil, providerProblem(404, "not_found", "Budget reservation not found")
		}
		if err != nil {
			return nil, err
		}
		if state != "uncertain" && !(state == "reserved" && expired) {
			return nil, providerProblem(409, "budget_not_uncertain", "Only uncertain or expired pending reservations can be reconciled")
		}
		outcome := aigateway.Outcome{Usage: confirmed, Dispatched: !input.NotAccepted}
		if err = settleAIBudget(r.Context(), tx, id, outcome, userFrom(r).ID, input.Evidence, true); err != nil {
			return nil, err
		}
		if err = settleAICredits(r.Context(), tx, id, outcome, userFrom(r).ID, input.Evidence, true); err != nil {
			return nil, err
		}
		var prompt, completion, total, read, write any
		if confirmed != nil {
			prompt = confirmed.PromptTokens
			completion = confirmed.CompletionTokens
			total = confirmed.TotalTokens
			read = confirmed.CacheReadTokens
			write = confirmed.CacheWriteTokens
		}
		if _, err = tx.Exec(r.Context(), `UPDATE ai_gateway_requests SET state=CASE WHEN state='running' THEN 'incomplete' ELSE state END,finished_at=COALESCE(finished_at,clock_timestamp()),prompt_tokens=$2,completion_tokens=$3,total_tokens=$4,cache_read_tokens=$5,cache_write_tokens=$6 WHERE id=$1`, id, prompt, completion, total, read, write); err != nil {
			return nil, err
		}
		return record{"request_id": id, "reconciled": true, "evidence_sha256": input.Evidence}, nil
	})
}
