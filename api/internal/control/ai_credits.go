package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

var errAICreditsExceeded = errors.New("AI organization credits exhausted")
var errAICreditsPrice = errors.New("AI organization credits require explicit model pricing")
var errAICreditsPending = errors.New("AI organization credits have too many unsettled requests")

// All admission, budget policy, settlement and evidence reconciliation use the
// same org -> branch lock order. No external network request holds these locks.
func aiCreditOrgLock(ctx context.Context, tx pgx.Tx, org string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ai-credit-org:'||$1,0))`, org)
	return err
}

func reserveAICredits(ctx context.Context, tx pgx.Tx, id, branch string, route aigateway.Route, maxOutput int) error {
	var org, project string
	err := tx.QueryRow(ctx, `SELECT p.org_id,p.id FROM branches b JOIN projects p ON p.id=b.project_id WHERE b.id=$1`, branch).Scan(&org, &project)
	if err != nil {
		return err
	}
	var enabled bool
	var credited, spent, held int64
	err = tx.QueryRow(ctx, `SELECT enabled,credited_micros,spent_micros,reserved_micros FROM ai_credit_accounts WHERE org_id=$1 FOR UPDATE`, org).Scan(&enabled, &credited, &spent, &held)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	var pending int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM ai_credit_reservations WHERE org_id=$1 AND state IN ('reserved','uncertain')`, org).Scan(&pending); err != nil {
		return err
	}
	if pending >= 1024 {
		return errAICreditsPending
	}
	if route.Price == nil {
		return errAICreditsPrice
	}
	amount, err := route.Price.Reserve(maxOutput)
	if err != nil {
		return errAICreditsPrice
	}
	if spent > credited || held > credited-spent || amount > credited-spent-held {
		return errAICreditsExceeded
	}
	_, err = tx.Exec(ctx, `UPDATE ai_credit_accounts SET reserved_micros=reserved_micros+$2 WHERE org_id=$1`, org, amount)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ai_credit_reservations(request_id,org_id,project_id,branch_id,price,reserved_micros,state) VALUES($1,$2,$3,$4,$5,$6,'reserved')`, id, org, project, branch, route.Price, amount)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ai_credit_ledger(id,org_id,request_id,kind,reserved_delta) VALUES($1,$2,$3,'reserve',$4)`, newID("acl_"), org, id, amount)
	return err
}

func settleAICredits(ctx context.Context, tx pgx.Tx, id string, outcome aigateway.Outcome, actor, evidence string, reconcile bool) error {
	var org, state string
	var amount int64
	var price aigateway.Price
	err := tx.QueryRow(ctx, `SELECT org_id,state,reserved_micros,price FROM ai_credit_reservations WHERE request_id=$1 FOR UPDATE`, id).Scan(&org, &state, &amount, &price)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "settled" || state == "released" {
		if reconcile {
			return providerProblem(409, "credits_already_settled", "A final credit settlement cannot be overwritten")
		}
		return nil
	}
	kind, next := "uncertain", "uncertain"
	heldDelta, spentDelta := int64(0), int64(0)
	var actual, usage any
	if outcome.Usage != nil {
		cost, e := price.Cost(*outcome.Usage)
		if e != nil {
			return e
		}
		actual, usage = cost, outcome.Usage
		heldDelta, spentDelta = -amount, cost
		kind, next = "settle", "settled"
	} else if !outcome.Dispatched {
		actual = int64(0)
		heldDelta = -amount
		kind, next = "release", "released"
	}
	if next == state && !reconcile {
		return nil
	}
	if reconcile {
		kind = "reconcile"
	}
	_, err = tx.Exec(ctx, `UPDATE ai_credit_reservations SET state=$2,actual_micros=$3,usage=$4,settled_at=CASE WHEN $2 IN ('settled','released') THEN clock_timestamp() ELSE NULL END WHERE request_id=$1`, id, next, actual, usage)
	if err != nil {
		return err
	}
	if heldDelta != 0 || spentDelta != 0 {
		_, err = tx.Exec(ctx, `UPDATE ai_credit_accounts SET reserved_micros=reserved_micros+$2,spent_micros=spent_micros+$3 WHERE org_id=$1`, org, heldDelta, spentDelta)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO ai_credit_ledger(id,org_id,request_id,kind,spent_delta,reserved_delta,actor_id,evidence_sha256) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''))`, newID("acl_"), org, id, kind, spentDelta, heldDelta, actor, evidence)
	return err
}

func (s *server) aiCreditOperator(r *http.Request) bool {
	return s.aiProviders != nil && s.aiProviders.isOperator(userFrom(r))
}

func (s *server) aiCreditRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	org := r.PathValue("org")
	if org == "" {
		if !s.requireAIProviderOperator(w, r) {
			return
		}
		org = r.PathValue("creditOrg")
	} else if accessFrom(r).Level < 3 || userFrom(r).APIKey && userFrom(r).KeyProject != "" {
		fail(w, r, 403, "forbidden", "Organization credit history requires organization administrator access")
		return
	}
	var exists bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1)`, org).Scan(&exists); err != nil || !exists {
		if err != nil {
			aiProviderFailure(w, r, err)
		} else {
			fail(w, r, 404, "not_found", "Organization not found")
		}
		return
	}
	if strings.HasSuffix(r.URL.Path, "/ledger") || strings.HasSuffix(r.URL.Path, "/requests") {
		var beforeAt any
		beforeID := ""
		if encoded := r.URL.Query().Get("before"); encoded != "" {
			var cursor struct {
				At time.Time `json:"at"`
				ID string    `json:"id"`
			}
			raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
			if err != nil || len(raw) > 256 || json.Unmarshal(raw, &cursor) != nil || cursor.At.IsZero() || !creditHistoryID.MatchString(cursor.ID) {
				fail(w, r, 422, "invalid_cursor", "Use the returned credit history cursor")
				return
			}
			beforeAt, beforeID = cursor.At, cursor.ID
		}
		query := `SELECT id,request_id,kind,credit_delta,spent_delta,reserved_delta,source_event_id,reason,actor_id,evidence_sha256,created_at FROM ai_credit_ledger WHERE org_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)<($2,$3)) ORDER BY created_at DESC,id DESC LIMIT 101`
		key := "id"
		if strings.HasSuffix(r.URL.Path, "/requests") {
			query = `SELECT c.request_id,c.project_id,c.branch_id,c.state,c.reserved_micros,c.created_at,a.model_id,a.expires_at,(c.state='uncertain' OR a.expires_at<clock_timestamp()) AS needs_reconciliation FROM ai_credit_reservations c JOIN ai_gateway_requests a ON a.id=c.request_id WHERE c.org_id=$1 AND c.state IN ('reserved','uncertain') AND ($2::timestamptz IS NULL OR (c.created_at,c.request_id)<($2,$3)) ORDER BY c.created_at DESC,c.request_id DESC LIMIT 101`
			key = "request_id"
		}
		items, err := s.many(r.Context(), query, org, beforeAt, beforeID)
		if err != nil {
			aiProviderFailure(w, r, err)
			return
		}
		next := ""
		if len(items) > 100 {
			items = items[:100]
			last := items[99]
			raw, err := json.Marshal(record{"at": last["created_at"], "id": last[key]})
			if err != nil {
				aiProviderFailure(w, r, err)
				return
			}
			next = base64.RawURLEncoding.EncodeToString(raw)
		}
		jsonResponse(w, 200, record{"org_id": org, "items": items, "limit": 100, "next_cursor": next})
		return
	}
	account, err := s.one(r.Context(), `SELECT org_id,version,enabled,currency,credited_micros,spent_micros,reserved_micros,credited_micros-spent_micros AS balance_micros,credited_micros-spent_micros-reserved_micros AS available_micros,updated_at FROM ai_credit_accounts WHERE org_id=$1`, org)
	if isNoRows(err) {
		account = record{"org_id": org, "version": int64(0), "enabled": false, "currency": "USD", "credited_micros": int64(0), "spent_micros": int64(0), "reserved_micros": int64(0), "balance_micros": int64(0), "available_micros": int64(0), "updated_at": nil}
	} else if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, record{"account": account, "can_manage": s.aiCreditOperator(r), "credit_issuance": "self_hosted_operator_grants", "payment_processor": false})
}

func (s *server) aiCreditMutation(w http.ResponseWriter, r *http.Request, org string, input any, activeOrg bool, fn func(pgx.Tx) (record, error)) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	if len(s.idempotencyKey) < 32 {
		aiProviderFailure(w, r, errors.New("key unavailable"))
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err = aiCreditOrgLock(r.Context(), tx, org); err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	actor, hash := userFrom(r).ID, s.createRequestHash(r.Method+":"+r.URL.Path, input)
	var stored string
	var raw []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM ai_credit_mutations WHERE actor_id=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&stored, &raw)
	if err == nil {
		if stored != hash {
			fail(w, r, 409, "idempotency_conflict", "Key already used for different credit input")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
		return
	}
	if !isNoRows(err) {
		aiProviderFailure(w, r, err)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), `SELECT NOT disabled FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&active); err != nil || !active {
		fail(w, r, 403, "platform_operator_required", "Operator is no longer active")
		return
	}
	var state string
	if err = tx.QueryRow(r.Context(), `SELECT state FROM organizations WHERE id=$1 FOR SHARE`, org).Scan(&state); err != nil || activeOrg && state != "active" {
		if err != nil && !isNoRows(err) {
			aiProviderFailure(w, r, err)
		} else {
			fail(w, r, 404, "not_found", "Organization not found")
		}
		return
	}
	value, err := fn(tx)
	if err != nil {
		aiProviderFailure(w, r, err)
		return
	}
	raw, err = json.Marshal(value)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_credit_mutations(actor_id,org_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4,$5)`, actor, org, keyHash(key), hash, raw)
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

func (s *server) aiCreditPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	var input struct {
		Version *int64 `json:"version"`
		Enabled *bool  `json:"enabled"`
	}
	if readJSON(r, &input) != nil || input.Version == nil || input.Enabled == nil || *input.Version < 0 || *input.Version >= 1000000000000 {
		fail(w, r, 422, "invalid_request", "Explicit current version and enabled flag required")
		return
	}
	org := r.PathValue("creditOrg")
	s.aiCreditMutation(w, r, org, input, true, func(tx pgx.Tx) (record, error) {
		var version int64
		err := tx.QueryRow(r.Context(), `SELECT version FROM ai_credit_accounts WHERE org_id=$1 FOR UPDATE`, org).Scan(&version)
		if err != nil && !isNoRows(err) {
			return nil, err
		}
		if version != *input.Version {
			return nil, providerProblem(409, "credit_version_conflict", "Credit account changed; refresh before editing")
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_credit_accounts(org_id,version,enabled,updated_by) VALUES($1,1,$2,$3) ON CONFLICT(org_id) DO UPDATE SET version=ai_credit_accounts.version+1,enabled=EXCLUDED.enabled,updated_by=EXCLUDED.updated_by,updated_at=clock_timestamp()`, org, *input.Enabled, userFrom(r).ID)
		return record{"org_id": org, "version": version + 1, "enabled": *input.Enabled}, err
	})
}

var creditSourceEvent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{7,127}$`)
var creditHistoryID = regexp.MustCompile(`^(acl|air)_[a-f0-9]{24}$`)

func creditReasonValid(value string) bool {
	if strings.TrimSpace(value) != value || utf8.RuneCountInString(value) < 8 || utf8.RuneCountInString(value) > 500 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *server) aiCreditEntry(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderOperator(w, r) {
		return
	}
	var input struct {
		Version  *int64 `json:"version"`
		Kind     string `json:"kind"`
		Amount   *int64 `json:"amount_micros"`
		Source   string `json:"source_event_id"`
		Reason   string `json:"reason"`
		Evidence string `json:"evidence_sha256"`
	}
	if readJSON(r, &input) != nil || input.Version == nil || *input.Version < 0 || *input.Version >= 1000000000000 || input.Amount == nil || *input.Amount == 0 || *input.Amount < -1000000000000 || *input.Amount > 1000000000000 || input.Kind != "grant" && input.Kind != "adjustment" || input.Kind == "grant" && *input.Amount < 0 || !creditSourceEvent.MatchString(input.Source) || !creditReasonValid(input.Reason) || !budgetEvidence.MatchString(input.Evidence) {
		fail(w, r, 422, "invalid_request", "Use a versioned nonzero integer credit entry with a unique source event, reason and evidence SHA-256")
		return
	}
	org := r.PathValue("creditOrg")
	s.aiCreditMutation(w, r, org, input, true, func(tx pgx.Tx) (record, error) {
		var version, credited int64
		err := tx.QueryRow(r.Context(), `SELECT version,credited_micros FROM ai_credit_accounts WHERE org_id=$1 FOR UPDATE`, org).Scan(&version, &credited)
		if err != nil && !isNoRows(err) {
			return nil, err
		}
		var duplicate bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM ai_credit_ledger WHERE org_id=$1 AND source_event_id=$2)`, org, input.Source).Scan(&duplicate); err != nil {
			return nil, err
		}
		if duplicate {
			return nil, providerProblem(409, "credit_source_event_exists", "Source event already has an immutable credit entry")
		}
		if version != *input.Version {
			return nil, providerProblem(409, "credit_version_conflict", "Credit account changed; refresh before editing")
		}
		if total := credited + *input.Amount; total < -1000000000000000 || total > 1000000000000000 {
			return nil, providerProblem(422, "credit_balance_limit", "Cumulative operator credits exceed the supported range")
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_credit_accounts(org_id,version,credited_micros,updated_by) VALUES($1,1,$2,$3) ON CONFLICT(org_id) DO UPDATE SET version=ai_credit_accounts.version+1,credited_micros=ai_credit_accounts.credited_micros+EXCLUDED.credited_micros,updated_by=EXCLUDED.updated_by,updated_at=clock_timestamp()`, org, *input.Amount, userFrom(r).ID)
		id := newID("acl_")
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO ai_credit_ledger(id,org_id,kind,credit_delta,source_event_id,reason,actor_id,evidence_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, org, input.Kind, *input.Amount, input.Source, input.Reason, userFrom(r).ID, input.Evidence)
		}
		return record{"entry_id": id, "org_id": org, "version": version + 1, "kind": input.Kind, "amount_micros": *input.Amount, "source_event_id": input.Source, "currency": "USD"}, err
	})
}

// Operator evidence recovery is independent of a live or ready branch. Soft
// deletion cannot make an accepted, uncertain charge disappear from accounting.
func (s *server) aiCreditReconcile(w http.ResponseWriter, r *http.Request) {
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
			fail(w, r, 422, "invalid_usage", "Every confirmed token count is required")
			return
		}
		confirmed = &aigateway.Usage{PromptTokens: *u.Prompt, CompletionTokens: *u.Completion, TotalTokens: *u.Total, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite}
		if _, err := (aigateway.Price{Version: "validate", Currency: "USD", MaxInputTokens: 1}).Cost(*confirmed); err != nil {
			fail(w, r, 422, "invalid_usage", "Confirmed usage must contain complete consistent nonnegative counts")
			return
		}
	}
	id := r.PathValue("creditRequest")
	var org, branch string
	if err := s.db.QueryRow(r.Context(), `SELECT org_id,branch_id FROM ai_credit_reservations WHERE request_id=$1`, id).Scan(&org, &branch); err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Credit reservation not found")
		} else {
			aiProviderFailure(w, r, err)
		}
		return
	}
	s.aiCreditMutation(w, r, org, input, false, func(tx pgx.Tx) (record, error) {
		if err := aiBranchLock(r.Context(), tx, branch); err != nil {
			return nil, err
		}
		var state string
		var expired bool
		err := tx.QueryRow(r.Context(), `SELECT c.state,a.expires_at<clock_timestamp() FROM ai_credit_reservations c JOIN ai_gateway_requests a ON a.id=c.request_id WHERE c.request_id=$1 AND c.org_id=$2 FOR UPDATE OF c`, id, org).Scan(&state, &expired)
		if err != nil {
			return nil, err
		}
		if state != "uncertain" && !(state == "reserved" && expired) {
			return nil, providerProblem(409, "credits_not_uncertain", "Only uncertain or expired pending credits can be reconciled")
		}
		outcome := aigateway.Outcome{Usage: confirmed, Dispatched: !input.NotAccepted}
		if err = settleAICredits(r.Context(), tx, id, outcome, userFrom(r).ID, input.Evidence, true); err != nil {
			return nil, err
		}
		if err = settleAIBudget(r.Context(), tx, id, outcome, userFrom(r).ID, input.Evidence, true); err != nil {
			return nil, err
		}
		var prompt, completion, total, read, write any
		if confirmed != nil {
			prompt, completion, total, read, write = confirmed.PromptTokens, confirmed.CompletionTokens, confirmed.TotalTokens, confirmed.CacheReadTokens, confirmed.CacheWriteTokens
		}
		_, err = tx.Exec(r.Context(), `UPDATE ai_gateway_requests SET state=CASE WHEN state='running' THEN 'incomplete' ELSE state END,finished_at=COALESCE(finished_at,clock_timestamp()),prompt_tokens=$2,completion_tokens=$3,total_tokens=$4,cache_read_tokens=$5,cache_write_tokens=$6 WHERE id=$1`, id, prompt, completion, total, read, write)
		return record{"request_id": id, "org_id": org, "reconciled": true, "evidence_sha256": input.Evidence, "reconciled_at": time.Now().UTC()}, err
	})
}
