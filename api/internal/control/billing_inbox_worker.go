package control

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/william-lbn/control-plane/api/internal/billing"
)

type billingInboxItem struct {
	id, kind, object, token string
	source                  string
	invoice                 *string
	attempts                int
}

var errBillingPending = errors.New("payment not yet confirmed")
var errBillingForeign = errors.New("provider object has no platform invoice")

func (s *server) billingReadObservation(ctx context.Context, item billingInboxItem) (billingMoneyObservation, error) {
	var o billingMoneyObservation
	account, err := s.billing.provider.Account(ctx)
	if err != nil {
		return o, err
	}
	if account != s.billing.config.Provider.Account {
		return o, providerProblem(409, "billing_merchant_changed", "Approved payment merchant does not match")
	}
	prefix, _ := billingEventObject(item.kind)
	switch prefix {
	case "cs_":
		o.session, err = s.billing.provider.Session(ctx, item.object)
	case "ch_":
		o.charge, err = s.billing.provider.Charge(ctx, item.object)
	case "dp_":
		var dispute billing.Dispute
		dispute, err = s.billing.provider.Dispute(ctx, item.object)
		if err == nil {
			o.dispute = &dispute
			o.charge, err = s.billing.provider.Charge(ctx, dispute.Charge)
		}
	default:
		return o, errBillingForeign
	}
	if err != nil {
		return o, err
	}
	if o.session.ID != "" {
		if o.session.Metadata["neon_invoice"] == "" {
			return o, errBillingForeign
		}
		if o.session.Status == "expired" && o.session.PaymentStatus != "paid" {
			return o, nil
		}
		if o.session.Status != "complete" || o.session.PaymentStatus != "paid" {
			if item.source == "operator_reconcile" && o.session.Status == "open" && o.session.PaymentStatus == "unpaid" {
				return o, nil
			}
			return o, errBillingPending
		}
		o.intent, err = s.billing.provider.Intent(ctx, o.session.PaymentIntent)
	} else {
		o.intent, err = s.billing.provider.Intent(ctx, o.charge.PaymentIntent)
	}
	if err != nil {
		return o, err
	}
	if o.session.ID == "" {
		if o.intent.Metadata["neon_invoice"] == "" {
			return o, errBillingForeign
		}
		id := o.intent.Metadata["neon_invoice"]
		if !billingInvoiceID.MatchString(id) {
			return o, billing.ErrProtocol
		}
		v, e := loadBillingInvoice(ctx, s.db, id, "", false)
		if e != nil {
			return o, e
		}
		if v.session != nil {
			o.session, err = s.billing.provider.Session(ctx, *v.session)
		} else {
			o.session, err = s.billing.provider.SessionForIntent(ctx, o.intent.ID)
		}
		if err != nil {
			return o, err
		}
	}
	if o.charge.ID == "" {
		o.charge, err = s.billing.provider.Charge(ctx, o.intent.LatestCharge)
		if err != nil {
			return o, err
		}
	}
	id := o.session.Metadata["neon_invoice"]
	if !billingInvoiceID.MatchString(id) {
		return o, billing.ErrProtocol
	}
	v, err := loadBillingInvoice(ctx, s.db, id, "", false)
	if err != nil {
		return o, err
	}
	if o.dispute == nil && v.disputeID != nil {
		d, e := s.billing.provider.Dispute(ctx, *v.disputeID)
		if e != nil {
			return o, e
		}
		o.dispute = &d
	}
	return o, nil
}

func (s *server) billingInboxFinish(item billingInboxItem, state, code, invoice, invoiceToken string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if invoice != "" && invoiceToken != "" {
		if _, err = tx.Exec(ctx, `UPDATE billing_invoices SET processing_token=NULL,processing_until=NULL,version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND processing_token=$2`, invoice, invoiceToken); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE billing_inbox SET state=$3,error_code=NULLIF($4,''),lease_token=NULL,lease_until=NULL,next_attempt_at=clock_timestamp()+interval '30 seconds',updated_at=clock_timestamp() WHERE id=$1 AND lease_token=$2 AND state='processing'`, item.id, item.token, state, code)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func billingInboxFailure(err error, attempts int) (string, string) {
	if errors.Is(err, errBillingForeign) {
		return "ignored", "not_platform_invoice"
	}
	if errors.Is(err, errBillingPending) {
		if attempts < 20 {
			return "pending", "awaiting_provider_confirmation"
		}
		return "review", "provider_confirmation_pending"
	}
	var problem aiProviderProblem
	if errors.As(err, &problem) {
		return "review", problem.code
	}
	if errors.Is(err, billing.ErrProtocol) || isNoRows(err) {
		return "review", "provider_receipt_unavailable"
	}
	if attempts >= 20 {
		return "review", "provider_read_needs_reconciliation"
	}
	return "pending", "provider_read_unavailable"
}

func (s *server) billingInboxWorkOnce(ctx context.Context) (bool, error) {
	if s.billing == nil {
		return false, nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	item := billingInboxItem{token: newID("blt_")}
	err = tx.QueryRow(ctx, `SELECT id,event_type,object_id,attempts,source_kind,invoice_id FROM billing_inbox WHERE provider_identity=$1 AND attempts<1000 AND ((state='pending' AND next_attempt_at<=clock_timestamp()) OR (state='processing' AND lease_until<clock_timestamp())) ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, s.billing.identity).Scan(&item.id, &item.kind, &item.object, &item.attempts, &item.source, &item.invoice)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE billing_inbox SET state='processing',attempts=attempts+1,lease_token=$2,lease_until=clock_timestamp()+interval '210 seconds',updated_at=clock_timestamp() WHERE id=$1`, item.id, item.token)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	item.attempts++
	workCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	o, readErr := s.billingReadObservation(workCtx, item)
	if readErr != nil {
		state, code := billingInboxFailure(readErr, item.attempts)
		return true, s.billingInboxFinish(item, state, code, "", "")
	}
	id := o.session.Metadata["neon_invoice"]
	if !billingInvoiceID.MatchString(id) {
		return true, s.billingInboxFinish(item, "review", "provider_invoice_reference_invalid", "", "")
	}
	if item.invoice != nil && *item.invoice != id {
		return true, s.billingInboxFinish(item, "review", "billing_original_invoice_mismatch", "", "")
	}
	claim, err := s.db.Begin(workCtx)
	if err != nil {
		return true, err
	}
	defer claim.Rollback(workCtx)
	v, err := loadBillingInvoice(workCtx, claim, id, "", true)
	if err != nil {
		claim.Rollback(workCtx)
		return true, s.billingInboxFinish(item, "review", "original_invoice_unavailable", "", "")
	}
	if err = s.billing.validateSession(v, o.session); err != nil {
		claim.Rollback(workCtx)
		return true, s.billingInboxFinish(item, "review", "billing_provider_binding_mismatch", "", "")
	}
	if v.requested == nil || v.dispatched == nil {
		claim.Rollback(workCtx)
		return true, s.billingInboxFinish(item, "review", "billing_original_dispatch_required", "", "")
	}
	invoiceToken := newID("blt_")
	ok, err := billingClaimInvoice(workCtx, claim, id, invoiceToken)
	if err != nil {
		return true, err
	}
	if !ok {
		claim.Rollback(workCtx)
		return true, s.billingInboxFinish(item, "pending", "original_invoice_busy", "", "")
	}
	tag, err := claim.Exec(workCtx, `UPDATE billing_inbox SET invoice_id=$3,updated_at=clock_timestamp() WHERE id=$1 AND lease_token=$2 AND lease_until>clock_timestamp()`, item.id, item.token, id)
	if err != nil {
		return true, err
	}
	if tag.RowsAffected() != 1 {
		return true, errLeaseLost
	}
	if err = claim.Commit(workCtx); err != nil {
		return true, err
	}
	if o.session.Status == "expired" && o.session.PaymentStatus != "paid" || item.source == "operator_reconcile" && o.session.Status == "open" && o.session.PaymentStatus == "unpaid" {
		state := "expired"
		var keyVersion, linkCipher, expires any
		if o.session.Status == "open" {
			deadline := time.Unix(o.session.ExpiresAt, 0).UTC()
			if err = s.billing.validateCheckoutLink(o.session.ID, o.session.URL, deadline, time.Now()); err != nil {
				return true, s.billingInboxFinish(item, "review", "billing_checkout_link_invalid", id, invoiceToken)
			}
			box := billingCheckoutBox{Invoice: v.id, Org: v.org, Provider: v.provider, Expires: deadline}
			if err = s.billingSealCheckout(&box, o.session.URL); err != nil {
				return true, s.billingInboxFinish(item, "review", "billing_checkout_sealing_failed", id, invoiceToken)
			}
			state = "ready"
			keyVersion, linkCipher, expires = box.KeyVersion, box.Ciphertext, deadline
		}
		final, err := s.db.Begin(workCtx)
		if err != nil {
			return true, err
		}
		defer final.Rollback(workCtx)
		tag, err = final.Exec(workCtx, `UPDATE billing_invoices SET checkout_state=CASE WHEN paid THEN checkout_state ELSE $4 END,session_id=COALESCE(session_id,$3),checkout_key_version=COALESCE($5,checkout_key_version),checkout_ciphertext=COALESCE($6,checkout_ciphertext),checkout_expires_at=COALESCE($7,checkout_expires_at),processing_token=NULL,processing_until=NULL,version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND processing_token=$2 AND processing_until>clock_timestamp()`, id, invoiceToken, o.session.ID, state, keyVersion, linkCipher, expires)
		if err != nil {
			return true, err
		}
		if tag.RowsAffected() != 1 {
			return true, errLeaseLost
		}
		proof, _ := json.Marshal([]any{"neon-billing-checkout-observation-v1", id, o.session.ID, o.session.Status, o.session.PaymentStatus})
		tag, err = final.Exec(workCtx, `UPDATE billing_inbox SET state='applied',observation=$3,lease_token=NULL,lease_until=NULL,error_code=NULL,updated_at=clock_timestamp() WHERE id=$1 AND lease_token=$2 AND state='processing' AND lease_until>clock_timestamp()`, item.id, item.token, json.RawMessage(proof))
		if err != nil {
			return true, err
		}
		if tag.RowsAffected() != 1 {
			return true, errLeaseLost
		}
		return true, final.Commit(workCtx)
	}
	if err = s.billingApplyMoney(workCtx, item.id, item.token, invoiceToken, o); err != nil {
		state, code := billingInboxFailure(err, item.attempts)
		return true, s.billingInboxFinish(item, state, code, id, invoiceToken)
	}
	return true, nil
}

func (s *server) runBilling(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := s.billingCheckoutWorkOnce(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("billing checkout requires original invoice observation")
		}
		inbox, e := s.billingInboxWorkOnce(ctx)
		if e != nil && ctx.Err() == nil {
			s.logger.Warn("billing receipt requires original inbox observation")
		}
		if worked || inbox {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
