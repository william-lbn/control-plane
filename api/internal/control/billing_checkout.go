package control

import (
	"context"
	"crypto/hmac"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/billing"
)

type billingInvoiceState struct {
	id, org, actor, provider, account, offerID, offerRevision, offerName, currency, binding string
	live                                                                                    bool
	amount, credits, version                                                                int64
	checkout                                                                                string
	session, intent, charge                                                                 *string
	requested, dispatched, expires                                                          *time.Time
	keyVersion                                                                              *string
	ciphertext                                                                              []byte
	token                                                                                   *string
	until                                                                                   *time.Time
	paid                                                                                    bool
	refunded                                                                                int64
	disputeID, disputeState                                                                 *string
	disputeMinor, applied                                                                   int64
}
type billingQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadBillingInvoice(ctx context.Context, q billingQuerier, id, org string, lock bool) (billingInvoiceState, error) {
	var v billingInvoiceState
	sql := `SELECT id,org_id,actor_id,provider_identity,provider_account,livemode,offer_id,offer_revision,offer_name,currency,amount_minor,credit_micros,binding,version,checkout_state,session_id,intent_id,charge_id,checkout_requested_at,checkout_dispatched_at,checkout_expires_at,checkout_key_version,checkout_ciphertext,processing_token,processing_until,paid,refunded_minor,dispute_id,dispute_state,dispute_minor,applied_credit_micros FROM billing_invoices WHERE id=$1 AND ($2='' OR org_id=$2)`
	if lock {
		sql += " FOR UPDATE"
	}
	err := q.QueryRow(ctx, sql, id, org).Scan(&v.id, &v.org, &v.actor, &v.provider, &v.account, &v.live, &v.offerID, &v.offerRevision, &v.offerName, &v.currency, &v.amount, &v.credits, &v.binding, &v.version, &v.checkout, &v.session, &v.intent, &v.charge, &v.requested, &v.dispatched, &v.expires, &v.keyVersion, &v.ciphertext, &v.token, &v.until, &v.paid, &v.refunded, &v.disputeID, &v.disputeState, &v.disputeMinor, &v.applied)
	return v, err
}
func billingString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *server) billingQueueCheckout(w http.ResponseWriter, r *http.Request) {
	id, org := r.PathValue("invoice"), r.PathValue("org")
	var input struct {
		Version *int64 `json:"version"`
	}
	if !billingInvoiceID.MatchString(id) || readJSON(r, &input) != nil || input.Version == nil || *input.Version < 1 {
		fail(w, r, 422, "invalid_request", "An original invoice and its current version are required")
		return
	}
	s.billingMutation(w, r, org, input, false, func(tx pgx.Tx, mutation string) (record, int, error) {
		v, err := loadBillingInvoice(r.Context(), tx, id, org, true)
		if err != nil {
			return nil, 0, err
		}
		if v.provider != s.billing.identity || v.version != *input.Version {
			return nil, 0, providerProblem(409, "billing_invoice_changed", "Invoice or merchant configuration changed; inspect the original invoice")
		}
		offer, ok := s.billing.offers[v.offerID]
		if !ok || offer.Revision != v.offerRevision {
			return nil, 0, providerProblem(409, "billing_offer_changed", "Review the current offer before requesting checkout")
		}
		if v.paid || v.dispatched != nil || v.session != nil || v.checkout != "open" && v.checkout != "uncertain" {
			return nil, 0, providerProblem(409, "billing_original_checkout_exists", "Inspect or recover the original checkout; another checkout will not be created")
		}
		var enabled bool
		if err = tx.QueryRow(r.Context(), `SELECT enabled FROM ai_credit_accounts WHERE org_id=$1 FOR SHARE`, org).Scan(&enabled); err != nil || !enabled {
			if err != nil && !isNoRows(err) {
				return nil, 0, err
			}
			return nil, 0, providerProblem(409, "billing_credit_account_disabled", "The original organization credit account is disabled")
		}
		_, err = tx.Exec(r.Context(), `UPDATE billing_invoices SET checkout_state='dispatching',checkout_requested_at=COALESCE(checkout_requested_at,clock_timestamp()),version=version+1,updated_at=clock_timestamp() WHERE id=$1`, id)
		return record{"invoice_id": id, "org_id": org, "checkout_state": "dispatching", "version": v.version + 1}, 202, err
	})
}

func (p *billingSettings) validateSession(v billingInvoiceState, session billing.Session) error {
	if p == nil || v.provider != p.identity || v.account != p.config.Provider.Account || v.live != p.config.Provider.Live || session.Live != v.live || session.AmountTotal != v.amount || session.Currency != "usd" || session.Mode != "payment" || session.Metadata["neon_invoice"] != v.id || !hmac.Equal([]byte(session.Metadata["neon_binding"]), []byte(v.binding)) || session.Reference != v.id || v.session != nil && *v.session != session.ID {
		return providerProblem(409, "billing_provider_binding_mismatch", "Provider receipt does not match the original invoice")
	}
	return nil
}

func (p *billingSettings) validateCheckoutLink(id, link string, expires time.Time, now time.Time) error {
	u, err := url.Parse(link)
	if p == nil || err != nil || len(link) > 8192 || u.User != nil || u.Scheme+"://"+u.Host != p.config.Provider.CheckoutOrigin || u.Path != "/c/pay/"+id || u.RawQuery != "" || !expires.After(now) || expires.After(now.Add(25*time.Hour)) {
		return providerProblem(409, "billing_checkout_link_invalid", "Original checkout link is unavailable or expired")
	}
	return nil
}

func (s *server) billingCheckoutLink(w http.ResponseWriter, r *http.Request) {
	org, id := r.PathValue("org"), r.PathValue("invoice")
	if !s.billingPermission(w, r, org, false, false) {
		return
	}
	if !billingInvoiceID.MatchString(id) {
		fail(w, r, 404, "not_found", "Billing record not found")
		return
	}
	v, err := loadBillingInvoice(r.Context(), s.db, id, org, false)
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	if s.billing == nil || !s.billing.allowsOrg(org) || v.provider != s.billing.identity || v.paid || v.checkout != "ready" || v.session == nil || v.keyVersion == nil || v.expires == nil {
		billingFailure(w, r, providerProblem(409, "billing_checkout_not_ready", "Inspect the original invoice status before opening checkout"))
		return
	}
	link, err := s.billingOpenCheckout(billingCheckoutBox{Invoice: v.id, Org: v.org, Provider: v.provider, KeyVersion: *v.keyVersion, Expires: *v.expires, Ciphertext: v.ciphertext})
	if err == nil {
		err = s.billing.validateCheckoutLink(*v.session, link, *v.expires, time.Now())
	}
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 200, record{"invoice_id": id, "checkout_url": link, "expires_at": v.expires})
}
