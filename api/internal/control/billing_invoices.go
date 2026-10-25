package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

var billingInvoiceID = regexp.MustCompile(`^bil_[a-f0-9]{24}$`)

func billingFailure(w http.ResponseWriter, r *http.Request, err error) {
	var problem aiProviderProblem
	if errors.As(err, &problem) {
		fail(w, r, problem.status, problem.code, problem.message)
		return
	}
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Billing record not found")
		return
	}
	fail(w, r, 503, "billing_unavailable", "Billing state is temporarily unavailable; retain the original request reference")
}
func (s *server) billingPermission(w http.ResponseWriter, r *http.Request, org string, operator, write bool) bool {
	if operator {
		if s.billing == nil {
			fail(w, r, 503, "billing_disabled", "Billing requires approved operator configuration")
			return false
		}
		if !s.billing.permits(userFrom(r), org) {
			fail(w, r, 403, "billing_operator_required", "An explicitly approved billing operator session and organization scope are required")
			return false
		}
	} else if userFrom(r).APIKey || accessFrom(r).Level < 3 {
		fail(w, r, 403, "forbidden", "Organization billing requires a current administrator session")
		return false
	}
	if write && (s.billing == nil || !s.billing.allowsOrg(org)) {
		fail(w, r, 503, "billing_disabled", "Billing is not enabled for this organization")
		return false
	}
	return true
}

func (s *server) billingMutation(w http.ResponseWriter, r *http.Request, org string, input any, operator bool, fn func(pgx.Tx, string) (record, int, error)) {
	if !s.billingPermission(w, r, org, operator, true) {
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	if len(s.idempotencyKey) < 32 || s.backendKeys == nil {
		billingFailure(w, r, errors.New("billing receipt keys unavailable"))
		return
	}
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	if err = aiCreditOrgLock(ctx, tx, org); err != nil {
		billingFailure(w, r, err)
		return
	}
	actor := userFrom(r).ID
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('billing-mutation:'||$1||':'||$2,0))`, actor, keyHash(key)); err != nil {
		billingFailure(w, r, err)
		return
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT NOT disabled FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&active)
	if err != nil && !isNoRows(err) {
		billingFailure(w, r, err)
		return
	}
	if err != nil || !active {
		fail(w, r, 403, "forbidden", "Current billing actor is unavailable")
		return
	}
	if !operator {
		var role string
		err = tx.QueryRow(ctx, `SELECT m.role FROM organizations o JOIN organization_members m ON m.org_id=o.id WHERE o.id=$1 AND o.state='active' AND m.user_id=$2 FOR SHARE OF o,m`, org, actor).Scan(&role)
		if err != nil && !isNoRows(err) {
			billingFailure(w, r, err)
			return
		}
		if err != nil || roleLevel(role) < 3 {
			fail(w, r, 403, "forbidden", "Current organization administrator access is required")
			return
		}
	}
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, input)
	var stored string
	var raw []byte
	var status int
	err = tx.QueryRow(ctx, `SELECT request_hash,response,http_status FROM billing_mutations WHERE actor_id=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&stored, &raw, &status)
	if err == nil {
		if stored != hash {
			fail(w, r, 409, "idempotency_conflict", "Key was used for a different billing request")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(raw)
		return
	}
	if !isNoRows(err) {
		billingFailure(w, r, err)
		return
	}
	id := newID("blm_")
	result, status, err := fn(tx, id)
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	raw, err = json.Marshal(result)
	if err != nil || len(raw) > 65536 {
		billingFailure(w, r, errors.New("billing receipt exceeds its bound"))
		return
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_mutations(id,actor_id,org_id,invoice_id,key_hash,request_hash,response,http_status) VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8)`, id, actor, org, stringVal(result["invoice_id"]), keyHash(key), hash, raw, status)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	jsonResponse(w, status, result)
}

func (s *server) billingCatalog(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if !s.billingPermission(w, r, org, false, false) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	offers := []billingOffer{}
	enabled := s.billing != nil && s.billing.allowsOrg(org)
	mode := "disabled"
	if enabled {
		for _, offer := range s.billing.offers {
			offers = append(offers, offer)
		}
		sort.Slice(offers, func(i, j int) bool { return offers[i].ID < offers[j].ID })
		mode = "test"
		if s.billing.config.Provider.Fixture {
			mode = "synthetic"
		} else if s.billing.config.Provider.Live {
			mode = "live"
		}
	}
	var accountReady bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM ai_credit_accounts WHERE org_id=$1 AND enabled AND currency='USD')`, org).Scan(&accountReady); err != nil {
		billingFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, record{"enabled": enabled, "currency": "USD", "offers": offers, "credit_account_ready": accountReady, "provider_mode": mode, "can_manage": s.billing != nil && s.billing.permits(userFrom(r), org)})
}

func (s *server) billingCreateInvoice(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OfferID  string `json:"offer_id"`
		Revision string `json:"offer_revision"`
	}
	if readJSON(r, &input) != nil || !billingOfferID.MatchString(input.OfferID) || !budgetEvidence.MatchString(input.Revision) {
		fail(w, r, 422, "invalid_request", "Select an explicit current billing offer revision")
		return
	}
	org := r.PathValue("org")
	s.billingMutation(w, r, org, input, false, func(tx pgx.Tx, mutation string) (record, int, error) {
		offer, ok := s.billing.offers[input.OfferID]
		if !ok || offer.Revision != input.Revision {
			return nil, 0, providerProblem(409, "billing_offer_changed", "Billing offer changed; review the current price before purchasing")
		}
		var enabled bool
		if err := tx.QueryRow(r.Context(), `SELECT enabled FROM ai_credit_accounts WHERE org_id=$1 AND currency='USD' FOR SHARE`, org).Scan(&enabled); err != nil || !enabled {
			if err != nil && !isNoRows(err) {
				return nil, 0, err
			}
			return nil, 0, providerProblem(409, "billing_credit_account_disabled", "The organization credit account must be enabled before purchasing")
		}
		var pending int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM billing_invoices WHERE org_id=$1 AND NOT paid AND checkout_state<>'expired' AND created_at>clock_timestamp()-interval '24 hours'`, org).Scan(&pending); err != nil {
			return nil, 0, err
		}
		if pending >= 32 {
			return nil, 0, providerProblem(429, "billing_pending_limit", "Review the original pending invoices before creating another")
		}
		id := newID("bil_")
		binding := s.billingInvoiceBinding(id, org, userFrom(r).ID, s.billing.identity, offer)
		_, err := tx.Exec(r.Context(), `INSERT INTO billing_invoices(id,org_id,actor_id,provider_identity,provider_account,livemode,offer_id,offer_revision,offer_name,currency,amount_minor,credit_micros,binding) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'USD',$10,$11,$12)`, id, org, userFrom(r).ID, s.billing.identity, s.billing.config.Provider.Account, s.billing.config.Provider.Live, offer.ID, offer.Revision, offer.Name, offer.AmountMinor, offer.CreditMicros, binding)
		return record{"invoice_id": id, "org_id": org, "checkout_state": "open", "currency": "USD", "amount_minor": offer.AmountMinor, "credit_micros": offer.CreditMicros, "offer_id": offer.ID, "offer_revision": offer.Revision}, 201, err
	})
}

const billingInvoicePublicColumns = `id AS invoice_id,org_id,offer_id,offer_revision,offer_name,currency,amount_minor,credit_micros,version,checkout_state,paid,refunded_minor,dispute_state,dispute_minor,applied_credit_micros,checkout_expires_at,created_at,updated_at,(checkout_dispatched_at IS NOT NULL) AS checkout_dispatched,(checkout_state='uncertain' AND checkout_dispatched_at IS NULL AND session_id IS NULL) AS can_retry_checkout`

func (s *server) billingReadInvoices(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	operator := false
	if org == "" {
		org = r.PathValue("billingOrg")
		operator = true
	}
	if !s.billingPermission(w, r, org, operator, false) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if id := r.PathValue("invoice"); id != "" {
		if !billingInvoiceID.MatchString(id) {
			fail(w, r, 404, "not_found", "Billing record not found")
			return
		}
		invoice, err := s.one(r.Context(), `SELECT `+billingInvoicePublicColumns+` FROM billing_invoices WHERE id=$1 AND org_id=$2`, id, org)
		if err != nil {
			billingFailure(w, r, err)
			return
		}
		jsonResponse(w, 200, record{"invoice": invoice})
		return
	}
	var beforeAt any
	beforeID := ""
	if encoded := r.URL.Query().Get("before"); encoded != "" {
		var cursor struct {
			At time.Time `json:"at"`
			ID string    `json:"id"`
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
		if err != nil || len(raw) > 256 || json.Unmarshal(raw, &cursor) != nil || cursor.At.IsZero() || !billingInvoiceID.MatchString(cursor.ID) {
			fail(w, r, 422, "invalid_cursor", "Use the returned billing invoice cursor")
			return
		}
		beforeAt, beforeID = cursor.At, cursor.ID
	}
	items, err := s.many(r.Context(), `SELECT `+billingInvoicePublicColumns+` FROM billing_invoices WHERE org_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)<($2,$3)) ORDER BY created_at DESC,id DESC LIMIT 101`, org, beforeAt, beforeID)
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	next := ""
	if len(items) > 100 {
		items = items[:100]
		last := items[99]
		raw, _ := json.Marshal(record{"at": last["created_at"], "id": last["invoice_id"]})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	jsonResponse(w, 200, record{"org_id": org, "items": items, "next_cursor": next, "limit": 100})
}

// Leases protect the original invoice across provider I/O without holding a
// database transaction or account lock while waiting on an external service.
func billingClaimInvoice(ctx context.Context, tx pgx.Tx, id, token string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE billing_invoices SET processing_token=$2,processing_until=clock_timestamp()+interval '210 seconds',version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND (processing_until IS NULL OR processing_until<clock_timestamp())`, id, token)
	return err == nil && tag.RowsAffected() == 1, err
}
