package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/william-lbn/control-plane/api/internal/billing"
)

type billingMoneyObservation struct {
	session billing.Session
	intent  billing.PaymentIntent
	charge  billing.Charge
	dispute *billing.Dispute
}

func (p *billingSettings) validateMoney(v billingInvoiceState, o billingMoneyObservation) error {
	bad := func() error {
		return providerProblem(409, "billing_payment_binding_mismatch", "Verified provider objects do not match the original paid invoice")
	}
	if v.requested == nil || v.dispatched == nil {
		return bad()
	}
	if p.validateSession(v, o.session) != nil || o.session.Status != "complete" || o.session.PaymentStatus != "paid" || o.session.PaymentIntent == "" || o.session.PaymentIntent != o.intent.ID || o.intent.Object != "payment_intent" || o.intent.Status != "succeeded" || o.intent.Amount != v.amount || o.intent.Received != v.amount || o.intent.Currency != "usd" || o.intent.Live != v.live || o.intent.Metadata["neon_invoice"] != v.id || !hmac.Equal([]byte(o.intent.Metadata["neon_binding"]), []byte(v.binding)) || v.intent != nil && *v.intent != o.intent.ID {
		return bad()
	}
	if o.intent.LatestCharge == "" || o.intent.LatestCharge != o.charge.ID || o.charge.Object != "charge" || o.charge.PaymentIntent != o.intent.ID || o.charge.Amount != v.amount || o.charge.CapturedAmount != v.amount || !o.charge.Paid || !o.charge.Captured || o.charge.Currency != "usd" || o.charge.Live != v.live || o.charge.Refunded < 0 || o.charge.Refunded > v.amount || v.charge != nil && *v.charge != o.charge.ID {
		return bad()
	}
	if !v.live && (!p.config.AllowNonLiveCredits || !p.allowsOrg(v.org)) {
		return providerProblem(409, "billing_non_live_scope_withdrawn", "Non-live credit issuance requires its original explicit organization approval")
	}
	if o.dispute == nil {
		if o.charge.Disputed || v.disputeID != nil {
			return providerProblem(409, "billing_dispute_receipt_required", "Retrieve the original dispute before applying this payment")
		}
		return nil
	}
	d := o.dispute
	if d.Object != "dispute" || d.Charge != o.charge.ID || d.PaymentIntent != "" && d.PaymentIntent != o.intent.ID || d.Amount < 1 || d.Amount > billing.MaxMinor || d.Currency != "usd" || d.Live != v.live || v.disputeID != nil && *v.disputeID != d.ID || v.disputeMinor != 0 && v.disputeMinor != min(v.amount, d.Amount) {
		return bad()
	}
	switch d.Status {
	case "needs_response", "under_review", "won", "lost", "prevented", "warning_needs_response", "warning_under_review", "warning_closed":
	default:
		return bad()
	}
	return nil
}

func billingLiability(v billingInvoiceState, o billingMoneyObservation) (refunded, disputed int64, disputeID, disputeState string, err error) {
	refunded = max(v.refunded, o.charge.Refunded)
	disputeID, disputeState = billingString(v.disputeID), billingString(v.disputeState)
	if o.dispute != nil {
		disputeID = o.dispute.ID
		state := o.dispute.Status
		if disputeState == "won" || disputeState == "lost" || disputeState == "prevented" {
			if state == "won" || state == "lost" || state == "prevented" {
				if state != disputeState {
					return 0, 0, "", "", providerProblem(409, "billing_dispute_terminal_conflict", "Retain and reconcile the original terminal dispute receipt")
				}
			} else {
				state = disputeState
			}
		}
		disputeState = state
		disputed = min(v.amount, o.dispute.Amount)
	}
	return
}

// Provider reads and signature verification happen before this short transaction.
// Account, immutable credit entry, payment effect and invoice move together.
func (s *server) billingApplyMoney(ctx context.Context, inboxID, inboxToken, invoiceToken string, o billingMoneyObservation) error {
	id := o.session.Metadata["neon_invoice"]
	if s.billing == nil || !billingInvoiceID.MatchString(id) {
		return errors.New("billing observation identity unavailable")
	}
	before, err := loadBillingInvoice(ctx, s.db, id, "", false)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = aiCreditOrgLock(ctx, tx, before.org); err != nil {
		return err
	}
	v, err := loadBillingInvoice(ctx, tx, id, before.org, true)
	if err != nil {
		return err
	}
	var provider, state string
	var receiptInvoice, token *string
	var leaseFresh bool
	err = tx.QueryRow(ctx, `SELECT provider_identity,state,invoice_id,lease_token,COALESCE(lease_until>clock_timestamp(),false) FROM billing_inbox WHERE id=$1 FOR UPDATE`, inboxID).Scan(&provider, &state, &receiptInvoice, &token, &leaseFresh)
	if err != nil {
		return err
	}
	if provider != v.provider || billingString(receiptInvoice) != id {
		return errLeaseLost
	}
	if state == "applied" {
		return nil
	}
	if billingString(v.token) != invoiceToken || v.until == nil || !v.until.After(time.Now()) {
		return errLeaseLost
	}
	if provider != v.provider || state != "processing" || billingString(receiptInvoice) != id || billingString(token) != inboxToken || !leaseFresh {
		return errLeaseLost
	}
	if err = s.billing.validateMoney(v, o); err != nil {
		return err
	}
	refunded, disputed, disputeID, disputeState, err := billingLiability(v, o)
	if err != nil {
		return err
	}
	liability := refunded
	if disputeState == "needs_response" || disputeState == "under_review" || disputeState == "lost" {
		liability = min(v.amount, liability+disputed)
	}
	reversal, err := billing.CreditReversal(v.credits, v.amount, liability)
	if err != nil {
		return err
	}
	desired := v.credits - reversal
	delta := desired - v.applied
	rawDispute := int64(0)
	if o.dispute != nil {
		rawDispute = o.dispute.Amount
	}
	proof, _ := json.Marshal([]any{"neon-billing-observation-v1", inboxID, v.id, v.provider, o.session.ID, o.intent.ID, o.charge.ID, v.amount, refunded, disputeID, disputeState, disputed, desired, rawDispute})
	sum := sha256.Sum256(proof)
	evidence := hex.EncodeToString(sum[:])
	if delta != 0 {
		var credited int64
		if err = tx.QueryRow(ctx, `SELECT credited_micros FROM ai_credit_accounts WHERE org_id=$1 FOR UPDATE`, v.org).Scan(&credited); err != nil {
			return err
		}
		if credited+delta < -1000000000000000 || credited+delta > 1000000000000000 {
			return providerProblem(409, "billing_credit_balance_limit", "Original payment receipt requires operator reconciliation at the account limit")
		}
		effect, entry := newID("blc_"), newID("acl_")
		kind := "grant"
		if delta < 0 {
			kind = "adjustment"
		}
		_, err = tx.Exec(ctx, `UPDATE ai_credit_accounts SET credited_micros=credited_micros+$2,version=version+1,updated_by=$3,updated_at=clock_timestamp() WHERE org_id=$1`, v.org, delta, v.actor)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO ai_credit_ledger(id,org_id,kind,credit_delta,source_event_id,reason,actor_id,evidence_sha256) VALUES($1,$2,$3,$4,$5,'Verified payment provider receipt',$6,$7)`, entry, v.org, kind, delta, "billing:"+effect, v.actor, evidence)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO billing_credit_effects(id,invoice_id,org_id,inbox_id,credit_entry_id,credit_delta,applied_before,applied_after) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, effect, v.id, v.org, inboxID, entry, delta, v.applied, desired)
		if err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE billing_invoices SET session_id=COALESCE(session_id,$3),intent_id=COALESCE(intent_id,$4),charge_id=COALESCE(charge_id,$5),paid=true,refunded_minor=$6,dispute_id=NULLIF($7,''),dispute_state=NULLIF($8,''),dispute_minor=$9,applied_credit_micros=$10,processing_token=NULL,processing_until=NULL,version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND processing_token=$2 AND processing_until>clock_timestamp()`, v.id, invoiceToken, o.session.ID, o.intent.ID, o.charge.ID, refunded, disputeID, disputeState, disputed, desired)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errLeaseLost
	}
	tag, err = tx.Exec(ctx, `UPDATE billing_inbox SET state='applied',observation=$3,lease_token=NULL,lease_until=NULL,error_code=NULL,updated_at=clock_timestamp() WHERE id=$1 AND lease_token=$2 AND lease_until>clock_timestamp()`, inboxID, inboxToken, json.RawMessage(proof))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errLeaseLost
	}
	return tx.Commit(ctx)
}
