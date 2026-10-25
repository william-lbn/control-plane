package control

import (
	"context"
	"errors"
	"time"

	"github.com/william-lbn/control-plane/api/internal/billing"
)

// One durable invoice can dispatch at most one provider POST. A crash after the
// dispatch mark retains an uncertain original, even when the response was lost.
func (s *server) billingCheckoutWorkOnce(ctx context.Context) (bool, error) {
	if s.billing == nil {
		return false, nil
	}
	_, err := s.db.Exec(ctx, `UPDATE billing_invoices SET checkout_state='uncertain',processing_token=NULL,processing_until=NULL,version=version+1,updated_at=clock_timestamp() WHERE provider_identity=$1 AND checkout_state='dispatching' AND checkout_dispatched_at IS NOT NULL AND (processing_until IS NULL OR processing_until<clock_timestamp())`, s.billing.identity)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM billing_invoices WHERE provider_identity=$1 AND checkout_state='dispatching' AND checkout_dispatched_at IS NULL AND NOT paid AND (processing_until IS NULL OR processing_until<clock_timestamp()) ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, s.billing.identity).Scan(&id)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	token := newID("blt_")
	if ok, err := billingClaimInvoice(ctx, tx, id, token); err != nil || !ok {
		return false, err
	}
	v, err := loadBillingInvoice(ctx, tx, id, "", false)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	workCtx, cancel := context.WithTimeout(ctx, 80*time.Second)
	defer cancel()
	account, err := s.billing.provider.Account(workCtx)
	if err == nil && (account != v.account || !s.billing.allowsOrg(v.org)) {
		err = providerProblem(409, "billing_merchant_changed", "Original merchant approval is unavailable")
	}
	if err != nil {
		return true, s.billingFinishCheckout(v, token, nil, err)
	}
	// The mark is committed before the only provider mutation. Reusing a Stripe
	// key after its retention window is deliberately not a recovery strategy.
	tag, err := s.db.Exec(workCtx, `UPDATE billing_invoices SET checkout_dispatched_at=clock_timestamp(),version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND processing_token=$2 AND processing_until>clock_timestamp() AND checkout_dispatched_at IS NULL AND checkout_state='dispatching'`, v.id, token)
	if err != nil {
		return true, err
	}
	if tag.RowsAffected() != 1 {
		return true, errLeaseLost
	}
	session, err := s.billing.provider.CreateSession(workCtx, billing.CheckoutInput{InvoiceID: v.id, Binding: v.binding, Name: v.offerName, ReturnOrigin: s.billing.config.ReturnOrigin, IdempotencyKey: "neon-checkout:" + v.id, AmountMinor: v.amount})
	if err == nil {
		err = s.billing.validateSession(v, session)
	}
	if err != nil {
		return true, s.billingFinishCheckout(v, token, nil, err)
	}
	return true, s.billingFinishCheckout(v, token, &session, nil)
}

func (s *server) billingFinishCheckout(original billingInvoiceState, token string, session *billing.Session, dispatchError error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	v, err := loadBillingInvoice(ctx, tx, original.id, original.org, true)
	if err != nil {
		return err
	}
	if billingString(v.token) != token {
		return errLeaseLost
	}
	state := "uncertain"
	var keyVersion, linkCipher, expires, sessionID any
	if session != nil {
		if err = s.billing.validateSession(v, *session); err != nil {
			return err
		}
		sessionID = session.ID
		deadline := time.Unix(session.ExpiresAt, 0).UTC()
		if dispatchError = s.billing.validateCheckoutLink(session.ID, session.URL, deadline, time.Now()); dispatchError == nil {
			box := billingCheckoutBox{Invoice: v.id, Org: v.org, Provider: v.provider, Expires: deadline}
			if err = s.billingSealCheckout(&box, session.URL); err != nil {
				return err
			}
			state = "ready"
			keyVersion, linkCipher, expires = box.KeyVersion, box.Ciphertext, deadline
		}
	}
	if state == "uncertain" && dispatchError == nil {
		return errors.New("uncertain checkout requires a retained cause")
	}
	tag, err := tx.Exec(ctx, `UPDATE billing_invoices SET checkout_state=$3,session_id=COALESCE(session_id,$4),checkout_key_version=COALESCE($5,checkout_key_version),checkout_ciphertext=COALESCE($6,checkout_ciphertext),checkout_expires_at=COALESCE($7,checkout_expires_at),processing_token=NULL,processing_until=NULL,version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND processing_token=$2 AND processing_until>clock_timestamp()`, v.id, token, state, sessionID, keyVersion, linkCipher, expires)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errLeaseLost
	}
	return tx.Commit(ctx)
}
