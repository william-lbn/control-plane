package control

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Reconciliation requests only provider reads. Operator assertions never grant
// credits or resend a checkout whose original outcome is unknown.
func (s *server) billingReconcile(w http.ResponseWriter, r *http.Request) {
	org, id := r.PathValue("billingOrg"), r.PathValue("invoice")
	var input struct {
		Version  *int64 `json:"version"`
		Kind     string `json:"object_type"`
		Object   string `json:"object_id"`
		Reason   string `json:"reason"`
		Evidence string `json:"evidence_sha256"`
	}
	if !billingInvoiceID.MatchString(id) || readJSON(r, &input) != nil || input.Version == nil || *input.Version < 1 || len(input.Reason) < 8 || len(input.Reason) > 500 || strings.TrimSpace(input.Reason) != input.Reason || !budgetEvidence.MatchString(input.Evidence) {
		fail(w, r, 422, "invalid_request", "Provide the original invoice version, provider object, reason and evidence digest")
		return
	}
	kind := map[string]string{"session": "checkout.session.completed", "charge": "charge.refunded", "dispute": "charge.dispute.updated"}[input.Kind]
	prefix, _ := billingEventObject(kind)
	if kind == "" || !billingProviderObjectID.MatchString(input.Object) || !strings.HasPrefix(input.Object, prefix) && !(prefix == "dp_" && strings.HasPrefix(input.Object, "du_")) {
		fail(w, r, 422, "invalid_request", "Use an original session, charge or dispute identifier")
		return
	}
	s.billingMutation(w, r, org, input, true, func(tx pgx.Tx, mutation string) (record, int, error) {
		v, err := loadBillingInvoice(r.Context(), tx, id, org, true)
		if err != nil {
			return nil, 0, err
		}
		if v.provider != s.billing.identity || v.version != *input.Version || v.requested == nil || v.dispatched == nil {
			return nil, 0, providerProblem(409, "billing_original_invoice_changed", "Inspect the original dispatched invoice and current version")
		}
		if input.Kind == "session" && v.session != nil && *v.session != input.Object || input.Kind == "charge" && v.charge != nil && *v.charge != input.Object || input.Kind == "dispute" && v.disputeID != nil && *v.disputeID != input.Object {
			return nil, 0, providerProblem(409, "billing_original_object_conflict", "Reconcile the original provider object")
		}
		var pending int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM billing_inbox WHERE invoice_id=$1 AND state IN ('pending','processing')`, id).Scan(&pending); err != nil {
			return nil, 0, err
		}
		if pending >= 32 {
			return nil, 0, providerProblem(429, "billing_pending_limit", "Inspect the existing original receipt requests")
		}
		inbox := newID("ble_")
		raw, _ := json.Marshal(input)
		_, err = tx.Exec(r.Context(), `INSERT INTO billing_inbox(id,provider_identity,event_id,event_type,object_id,api_version,livemode,payload_sha256,source_kind,source_actor,source_mutation,reason,evidence_sha256,invoice_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'operator_reconcile',$9,$10,$11,$12,$13)`, inbox, v.provider, "operator:"+mutation, kind, input.Object, s.billing.config.Provider.APIVersion, v.live, keyHash(string(raw)), userFrom(r).ID, mutation, input.Reason, input.Evidence, id)
		return record{"invoice_id": id, "org_id": org, "inbox_id": inbox, "state": "pending"}, 202, err
	})
}

func (s *server) billingReadReceipts(w http.ResponseWriter, r *http.Request) {
	org, id := r.PathValue("org"), r.PathValue("invoice")
	operator := false
	if org == "" {
		org = r.PathValue("billingOrg")
		operator = true
	}
	if !s.billingPermission(w, r, org, operator, false) {
		return
	}
	if !billingInvoiceID.MatchString(id) {
		fail(w, r, 404, "not_found", "Billing record not found")
		return
	}
	if _, err := loadBillingInvoice(r.Context(), s.db, id, org, false); err != nil {
		billingFailure(w, r, err)
		return
	}
	items, err := s.many(r.Context(), `SELECT id AS inbox_id,event_type,object_id,source_kind,state,attempts,error_code,observation,created_at,updated_at FROM billing_inbox WHERE invoice_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, id)
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 200, record{"invoice_id": id, "items": items, "limit": 100})
}
