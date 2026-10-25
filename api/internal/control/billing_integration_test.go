package control

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
	"github.com/william-lbn/control-plane/api/internal/billing"
)

func TestBillingPostgresSchemaBindsLedgerAndRetainsInvoice(t *testing.T) {
	f := newProviderFixture(t)
	f.exec(`INSERT INTO ai_credit_accounts(org_id,version,enabled,updated_by) VALUES('org_provider',1,true,'provider_operator')`)
	invoice := "bil_" + strings.Repeat("1", 24)
	inbox := "ble_" + strings.Repeat("2", 24)
	effect := "blc_" + strings.Repeat("3", 24)
	entry := "acl_" + strings.Repeat("4", 24)
	f.exec(`INSERT INTO billing_invoices(id,org_id,actor_id,provider_identity,provider_account,livemode,offer_id,offer_revision,offer_name,currency,amount_minor,credit_micros,binding,checkout_requested_at,checkout_dispatched_at) VALUES($1,'org_provider','provider_org_admin',$2,'acct_fixture',false,$3,$2,'Synthetic prepaid offer','USD',125,100,$2,clock_timestamp(),clock_timestamp())`, invoice, strings.Repeat("a", 64), "offer_"+strings.Repeat("a", 24))
	if _, err := f.s.db.Exec(f.ctx, `UPDATE billing_invoices SET offer_name='Retagged offer',version=version+1 WHERE id=$1`, invoice); err == nil {
		t.Fatal("paid price snapshot mutable")
	}
	if _, err := f.s.db.Exec(f.ctx, `DELETE FROM billing_invoices WHERE id=$1`, invoice); err == nil {
		t.Fatal("invoice history erased")
	}
	f.exec(`INSERT INTO billing_inbox(id,provider_identity,event_id,event_type,object_id,api_version,livemode,payload_sha256,source_kind,invoice_id,state) VALUES($1,$2,'evt_synthetic_once','checkout.session.completed','cs_synthetic','2024-06-20',false,$2,'signed_webhook',$3,'processing')`, inbox, strings.Repeat("a", 64), invoice)
	if _, err := f.s.db.Exec(f.ctx, `UPDATE billing_inbox SET payload_sha256=repeat('b',64) WHERE id=$1`, inbox); err == nil {
		t.Fatal("original signed event replaced")
	}
	tx, err := f.s.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `UPDATE billing_invoices SET paid=true,applied_credit_micros=100,version=version+1 WHERE id=$1`, invoice); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.ctx); err == nil {
		t.Fatal("invoice credited without retained effect")
	}
	tx, err = f.s.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `INSERT INTO ai_credit_ledger(id,org_id,kind,credit_delta,source_event_id,reason,actor_id,evidence_sha256) VALUES($1,'org_provider','grant',100,$2,'Verified synthetic payment receipt','provider_org_admin',repeat('a',64))`, entry, "billing:"+effect); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `UPDATE ai_credit_accounts SET credited_micros=100,version=version+1 WHERE org_id='org_provider'`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `INSERT INTO billing_credit_effects(id,invoice_id,org_id,inbox_id,credit_entry_id,credit_delta,applied_before,applied_after) VALUES($1,$2,'org_provider',$3,$4,100,0,100)`, effect, invoice, inbox, entry); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `UPDATE billing_invoices SET paid=true,applied_credit_micros=100,version=version+1 WHERE id=$1`, invoice); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(f.ctx, `UPDATE billing_credit_effects SET credit_delta=1 WHERE id=$1`, effect); err == nil {
		t.Fatal("payment effect mutable")
	}
	if _, err = f.s.db.Exec(f.ctx, `UPDATE billing_invoices SET refunded_minor=25,version=version+1 WHERE id=$1`, invoice); err == nil {
		t.Fatal("refund liability changed without its matching credit reversal")
	}
	refundInbox := "ble_" + strings.Repeat("5", 24)
	refundEffect := "blc_" + strings.Repeat("6", 24)
	refundEntry := "acl_" + strings.Repeat("7", 24)
	f.exec(`INSERT INTO billing_inbox(id,provider_identity,event_id,event_type,object_id,api_version,livemode,payload_sha256,source_kind,invoice_id,state) VALUES($1,$2,'evt_synthetic_refund','charge.refunded','ch_synthetic','2024-06-20',false,$2,'signed_webhook',$3,'processing')`, refundInbox, strings.Repeat("a", 64), invoice)
	refundTx, err := f.s.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer refundTx.Rollback(f.ctx)
	if _, err = refundTx.Exec(f.ctx, `INSERT INTO ai_credit_ledger(id,org_id,kind,credit_delta,source_event_id,reason,actor_id,evidence_sha256) VALUES($1,'org_provider','adjustment',-20,$2,'Verified synthetic refund receipt','provider_org_admin',repeat('b',64))`, refundEntry, "billing:"+refundEffect); err != nil {
		t.Fatal(err)
	}
	if _, err = refundTx.Exec(f.ctx, `INSERT INTO billing_credit_effects(id,invoice_id,org_id,inbox_id,credit_entry_id,credit_delta,applied_before,applied_after) VALUES($1,$2,'org_provider',$3,$4,-20,100,80)`, refundEffect, invoice, refundInbox, refundEntry); err != nil {
		t.Fatal(err)
	}
	if _, err = refundTx.Exec(f.ctx, `UPDATE billing_invoices SET refunded_minor=25,applied_credit_micros=80,version=version+1 WHERE id=$1`, invoice); err != nil {
		t.Fatal(err)
	}
	if _, err = refundTx.Exec(f.ctx, `UPDATE ai_credit_accounts SET credited_micros=80,version=version+1 WHERE org_id='org_provider'`); err != nil {
		t.Fatal(err)
	}
	if err = refundTx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(f.ctx, `UPDATE billing_invoices SET refunded_minor=0,version=version+1 WHERE id=$1`, invoice); err == nil {
		t.Fatal("out of order event reduced cumulative refund")
	}
	if _, err = f.s.db.Exec(f.ctx, `UPDATE billing_invoices SET paid=false,applied_credit_micros=0,version=version+1 WHERE id=$1`, invoice); err == nil {
		t.Fatal("paid receipt became unpaid")
	}
}

func TestBillingPostgresVerifiedFundingRefundDisputeAndAIAdmission(t *testing.T) {
	f := newProviderFixture(t)
	cfg := billingFixtureConfig()
	cfg.Operators = []string{"provider_operator"}
	cfg.Organizations = []string{"org_provider"}
	raw, _ := json.Marshal(cfg)
	p, err := newBillingSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.s.billing = p
	f.exec(`INSERT INTO ai_credit_accounts(org_id,version,enabled,updated_by) VALUES('org_provider',1,true,'provider_operator')`)
	root := "/api/v1/organizations/org_provider/billing/invoices"
	offer := p.offers[cfg.Offers[0].ID]
	created := f.call("provider_org_admin", "POST", root, "funded-original-invoice", record{"offer_id": offer.ID, "offer_revision": offer.Revision}, 201)
	id := stringVal(created["invoice_id"])
	f.call("provider_org_admin", "POST", root+"/"+id+"/checkout", "funded-original-checkout", record{"version": 1}, 202)
	f.exec(`UPDATE billing_invoices SET checkout_dispatched_at=clock_timestamp(),checkout_state='uncertain',version=version+1 WHERE id=$1`, id)
	v, err := loadBillingInvoice(f.ctx, f.s.db, id, "org_provider", false)
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"neon_invoice": id, "neon_binding": v.binding}
	obs := billingMoneyObservation{session: billing.Session{ID: "cs_paid_fixture", Object: "checkout.session", Mode: "payment", Status: "complete", PaymentStatus: "paid", AmountTotal: 125, Currency: "usd", Reference: id, PaymentIntent: "pi_paid_fixture", Metadata: metadata}, intent: billing.PaymentIntent{ID: "pi_paid_fixture", Object: "payment_intent", Status: "succeeded", Amount: 125, Received: 125, Currency: "usd", LatestCharge: "ch_paid_fixture", Metadata: metadata}, charge: billing.Charge{ID: "ch_paid_fixture", Object: "charge", PaymentIntent: "pi_paid_fixture", Amount: 125, CapturedAmount: 125, Currency: "usd", Paid: true, Captured: true}}
	apply := func(label string, o billingMoneyObservation) (string, string, string) {
		t.Helper()
		inbox, inboxToken, invoiceToken := newID("ble_"), newID("blt_"), newID("blt_")
		tx, err := f.s.db.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := billingClaimInvoice(f.ctx, tx, id, invoiceToken); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, err = tx.Exec(f.ctx, `INSERT INTO billing_inbox(id,provider_identity,event_id,event_type,object_id,api_version,livemode,payload_sha256,source_kind,invoice_id,state,lease_token,lease_until) VALUES($1,$2,$3,'checkout.session.completed','cs_paid_fixture','2024-06-20',false,repeat('a',64),'signed_webhook',$4,'processing',$5,clock_timestamp()+interval '210 seconds')`, inbox, p.identity, "evt_"+label, id, inboxToken); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err = f.s.billingApplyMoney(f.ctx, inbox, inboxToken, invoiceToken, o); err != nil {
			t.Fatal(label, err)
		}
		return inbox, inboxToken, invoiceToken
	}
	inbox, token, claim := apply("paid_original", obs)
	creditCounters(t, f, 1000000, 0, 0)
	if err = f.s.billingApplyMoney(f.ctx, inbox, token, claim, obs); err != nil {
		t.Fatal("original committed receipt replay", err)
	}
	apply("paid_duplicate_object_new_event", obs)
	creditCounters(t, f, 1000000, 0, 0)
	f.s.ai.Routes[0].Price = &aigateway.Price{Version: "funded-fixture-price", Currency: "USD", MaxInputTokens: 10, InputRate: 1000000, OutputRate: 2000000}
	first := newID("air_")
	if err = budgetReserve(t, f, first); err != nil {
		t.Fatal("verified funding did not admit AI", err)
	}
	if err = f.s.settleAIRequest(f.ctx, first, "br_provider", aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	pending := newID("air_")
	if err = budgetReserve(t, f, pending); err != nil {
		t.Fatal(err)
	}
	obs.charge.Refunded = 25
	apply("partial_refund", obs)
	creditCounters(t, f, 800000, 7, 26)
	obs.dispute = &billing.Dispute{ID: "dp_paid_fixture", Object: "dispute", Charge: "ch_paid_fixture", PaymentIntent: "pi_paid_fixture", Amount: 50, Currency: "usd", Status: "under_review"}
	obs.charge.Disputed = true
	apply("partial_dispute", obs)
	creditCounters(t, f, 400000, 7, 26)
	obs.dispute.Status = "won"
	obs.charge.Disputed = false
	apply("dispute_won", obs)
	creditCounters(t, f, 800000, 7, 26)
	obs.dispute.Status = "under_review"
	obs.charge.Refunded = 0
	apply("stale_dispute_and_refund", obs)
	creditCounters(t, f, 800000, 7, 26)
	obs.dispute.Status = "won"
	obs.charge.Refunded = 125
	apply("full_refund", obs)
	creditCounters(t, f, 0, 7, 26)
	if err = budgetReserve(t, f, newID("air_")); !errors.Is(err, errAICreditsExceeded) {
		t.Fatal("refunded balance admitted new AI work", err)
	}
	if err = f.s.settleAIRequest(f.ctx, pending, "br_provider", aigateway.Outcome{Status: "failed", Dispatched: false}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 0, 7, 0)
	if _, err = f.s.db.Exec(f.ctx, `UPDATE billing_inbox SET state='pending' WHERE id=$1`, inbox); err == nil {
		t.Fatal("final financial receipt rewritten")
	}
	if f.posts.Load() != 0 || f.gets.Load() != 0 {
		t.Fatal("funding accounting called an AI supplier")
	}
}

func TestBillingPostgresCheckoutWorkerSealsLinkAndNeverResendsUnknown(t *testing.T) {
	f := newProviderFixture(t)
	cfg := billingFixtureConfig()
	cfg.Operators = []string{"provider_operator"}
	cfg.Organizations = []string{"org_provider"}
	var posts atomic.Int64
	var mode atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cfg.Provider.Key {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path == "/v1/account" {
			id := "acct_fixture"
			if mode.Load() == 2 {
				id = "acct_othermerchant"
			}
			fmt.Fprintf(w, `{"id":%q,"object":"account"}`, id)
			return
		}
		if r.URL.Path != "/v1/checkout/sessions" || r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		r.ParseForm()
		id := r.Form.Get("metadata[neon_invoice]")
		binding := r.Form.Get("metadata[neon_binding]")
		if r.Header.Get("Idempotency-Key") != "neon-checkout:"+id {
			t.Error("original checkout key changed")
		}
		if mode.Load() == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		session := "cs_fixture_" + strings.TrimPrefix(id, "bil_")
		fmt.Fprintf(w, `{"id":%q,"object":"checkout.session","mode":"payment","status":"open","payment_status":"unpaid","amount_total":125,"currency":"usd","livemode":false,"client_reference_id":%q,"expires_at":%d,"url":%q,"metadata":{"neon_invoice":%q,"neon_binding":%q}}`, session, id, time.Now().Add(time.Hour).Unix(), cfg.Provider.CheckoutOrigin+"/c/pay/"+session+"#synthetic-bearer", id, binding)
	}))
	defer peer.Close()
	cfg.Provider.Origin = peer.URL
	cfg.Provider.CheckoutOrigin = peer.URL
	raw, _ := json.Marshal(cfg)
	p, err := newBillingSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.s.billing = p
	f.exec(`INSERT INTO ai_credit_accounts(org_id,version,enabled,updated_by) VALUES('org_provider',1,true,'provider_operator')`)
	root := "/api/v1/organizations/org_provider/billing/invoices"
	offer := p.offers[cfg.Offers[0].ID]
	create := func(key string) string {
		created := f.call("provider_org_admin", "POST", root, key, record{"offer_id": offer.ID, "offer_revision": offer.Revision}, 201)
		id := stringVal(created["invoice_id"])
		f.call("provider_org_admin", "POST", root+"/"+id+"/checkout", key+"-checkout", record{"version": 1}, 202)
		return id
	}
	id := create("worker-success-original")
	worked, err := f.s.billingCheckoutWorkOnce(f.ctx)
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	v, err := loadBillingInvoice(f.ctx, f.s.db, id, "org_provider", false)
	if err != nil || v.checkout != "ready" || v.dispatched == nil || bytes.Contains(v.ciphertext, []byte("/c/pay/")) || v.applied != 0 || v.paid {
		t.Fatal("checkout link or unpaid state", v.checkout, err)
	}
	link := f.call("provider_org_admin", "GET", root+"/"+id+"/checkout", "", nil, 200)
	if !strings.HasPrefix(stringVal(link["checkout_url"]), peer.URL+"/c/pay/") {
		t.Fatal("original owned checkout unavailable")
	}
	mode.Store(1)
	unknown := create("worker-unknown-original")
	worked, err = f.s.billingCheckoutWorkOnce(f.ctx)
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	v, err = loadBillingInvoice(f.ctx, f.s.db, unknown, "org_provider", false)
	if err != nil || v.checkout != "uncertain" || v.dispatched == nil || v.session != nil {
		t.Fatal("lost provider reply replaced original dispatch", v.checkout, err)
	}
	if worked, err = f.s.billingCheckoutWorkOnce(f.ctx); err != nil || worked || posts.Load() != 2 {
		t.Fatal("unknown provider POST resent", worked, posts.Load(), err)
	}
	f.call("provider_org_admin", "POST", root+"/"+unknown+"/checkout", "cannot-resend-unknown-original", record{"version": v.version}, 409)
	mode.Store(2)
	known := create("worker-known-no-dispatch")
	worked, err = f.s.billingCheckoutWorkOnce(f.ctx)
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	v, err = loadBillingInvoice(f.ctx, f.s.db, known, "org_provider", false)
	if err != nil || v.checkout != "uncertain" || v.dispatched != nil || posts.Load() != 2 {
		t.Fatal("wrong merchant dispatched a checkout", v.checkout, err)
	}
	mode.Store(0)
	f.call("provider_org_admin", "POST", root+"/"+known+"/checkout", "resume-same-known-not-dispatched", record{"version": v.version}, 202)
	if worked, err = f.s.billingCheckoutWorkOnce(f.ctx); err != nil || !worked || posts.Load() != 3 {
		t.Fatal("same proven unsent invoice did not resume", worked, posts.Load(), err)
	}
	var credits int64
	if err = f.s.db.QueryRow(f.ctx, `SELECT credited_micros FROM ai_credit_accounts WHERE org_id='org_provider'`).Scan(&credits); err != nil || credits != 0 {
		t.Fatal("checkout dispatch granted credits", credits, err)
	}
}

func TestBillingPostgresSignedInboxDeduplicatesWithoutTrustingPaidClaim(t *testing.T) {
	f := newProviderFixture(t)
	cfg := billingFixtureConfig()
	cfg.Operators = []string{"provider_operator"}
	cfg.Organizations = []string{"org_provider"}
	raw, _ := json.Marshal(cfg)
	p, err := newBillingSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.s.billing = p
	body := record{"id": "evt_synthetic_original", "object": "event", "type": "checkout.session.completed", "api_version": "2024-06-20", "created": time.Now().Unix(), "livemode": false, "data": record{"object": record{"id": "cs_synthetic_original", "object": "checkout.session", "payment_status": "paid", "amount_total": 99999999}}}
	send := func(data []byte, secret string, at time.Time, want int) {
		t.Helper()
		stamp := strconv.FormatInt(at.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(stamp + "."))
		mac.Write(data)
		r := httptest.NewRequest("POST", "/webhooks/billing/stripe", bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Stripe-Signature", "t="+stamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		f.s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal("provider inbox", w.Code, want, w.Body.String())
		}
	}
	raw, _ = json.Marshal(body)
	secret := cfg.Provider.WebhookSecrets[0]
	send(raw, "whsec_synthetic_wrong_signing_credential", time.Now(), 400)
	send(raw, secret, time.Now().Add(-301*time.Second), 400)
	send(raw, secret, time.Now(), 202)
	send(raw, secret, time.Now(), 200)
	pretty, _ := json.MarshalIndent(body, "", "  ")
	send(pretty, secret, time.Now(), 200)
	body["data"] = record{"object": record{"id": "cs_different_same_event", "object": "checkout.session"}}
	conflict, _ := json.Marshal(body)
	send(conflict, secret, time.Now(), 409)
	var count, credits int64
	if err = f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM billing_inbox),(SELECT count(*) FROM ai_credit_ledger)`).Scan(&count, &credits); err != nil || count != 1 || credits != 0 {
		t.Fatal("duplicate delivery or unverified paid claim issued credits", count, credits, err)
	}
}

func TestBillingPostgresInvoiceAdmissionReplayAndCurrentOwnership(t *testing.T) {
	f := newProviderFixture(t)
	cfg := billingFixtureConfig()
	cfg.Operators = []string{"provider_operator"}
	cfg.Organizations = []string{"org_provider"}
	raw, _ := json.Marshal(cfg)
	p, err := newBillingSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.s.billing = p
	root := "/api/v1/organizations/org_provider/billing"
	offer := p.offers[cfg.Offers[0].ID]
	input := record{"offer_id": offer.ID, "offer_revision": offer.Revision}
	f.call("provider_org_admin", "POST", root+"/invoices", "billing-account-must-be-enabled", input, 409)
	f.exec(`INSERT INTO ai_credit_accounts(org_id,version,enabled,updated_by) VALUES('org_provider',1,true,'provider_operator')`)
	catalog := f.call("provider_org_admin", "GET", root, "", nil, 200)
	if catalog["enabled"] != true || catalog["can_manage"] != false || catalog["provider_mode"] != "synthetic" {
		t.Fatal("tenant gained billing operator authority", catalog)
	}
	first := f.call("provider_org_admin", "POST", root+"/invoices", "billing-original-invoice-key", input, 201)
	id := stringVal(first["invoice_id"])
	if !billingInvoiceID.MatchString(id) || first["checkout_state"] != "open" {
		t.Fatal(first)
	}
	w := f.request("provider_org_admin", "POST", root+"/invoices", "billing-original-invoice-key", input)
	if w.Code != 201 || w.Header().Get("Idempotency-Replayed") != "true" || !strings.Contains(w.Body.String(), id) {
		t.Fatal("invoice was replaced after lost creation reply", w.Code, w.Body.String())
	}
	f.call("provider_org_admin", "POST", root+"/invoices", "billing-original-invoice-key", record{"offer_id": offer.ID, "offer_revision": strings.Repeat("c", 64)}, 409)
	f.call("provider_org_admin", "POST", root+"/invoices", "billing-price-cannot-be-forged", record{"offer_id": offer.ID, "offer_revision": offer.Revision, "amount_minor": 1}, 422)
	f.call("provider_org_admin", "GET", root+"/invoices/"+id, "", nil, 200)
	f.call("provider_org_admin", "GET", "/api/v1/platform/billing/organizations/org_provider/invoices", "", nil, 403)
	f.call("provider_operator", "GET", "/api/v1/platform/billing/organizations/org_provider/invoices", "", nil, 200)
	f.exec(`UPDATE organization_members SET role='editor' WHERE org_id='org_provider' AND user_id='provider_org_admin'`)
	f.call("provider_org_admin", "GET", root+"/invoices/"+id, "", nil, 403)
	f.call("provider_org_admin", "POST", root+"/invoices", "billing-original-invoice-key", input, 403)
	f.exec(`UPDATE organization_members SET role='admin' WHERE org_id='org_provider' AND user_id='provider_org_admin'`)
	f.s.billing = nil
	history := f.call("provider_org_admin", "GET", root+"/invoices", "", nil, 200)
	if len(history["items"].([]any)) != 1 {
		t.Fatal("withdrawing payment config hid original invoice")
	}
	if f.call("provider_org_admin", "GET", root, "", nil, 200)["enabled"] != false {
		t.Fatal("disabled billing still offered checkout")
	}
	var count, credited int64
	if err = f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM billing_invoices),(SELECT credited_micros FROM ai_credit_accounts WHERE org_id='org_provider')`).Scan(&count, &credited); err != nil || count != 1 || credited != 0 || f.posts.Load() != 0 || f.gets.Load() != 0 {
		t.Fatal("quote issued funds, duplicated an invoice, or called a supplier", count, credited, err)
	}
}

func TestBillingPostgresCheckoutQueuesOriginalWithoutIssuingCredits(t *testing.T) {
	f := newProviderFixture(t)
	cfg := billingFixtureConfig()
	cfg.Operators = []string{"provider_operator"}
	cfg.Organizations = []string{"org_provider"}
	raw, _ := json.Marshal(cfg)
	p, err := newBillingSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.s.billing = p
	f.exec(`INSERT INTO ai_credit_accounts(org_id,version,enabled,updated_by) VALUES('org_provider',1,true,'provider_operator')`)
	root := "/api/v1/organizations/org_provider/billing/invoices"
	offer := p.offers[cfg.Offers[0].ID]
	created := f.call("provider_org_admin", "POST", root, "billing-checkout-invoice-original", record{"offer_id": offer.ID, "offer_revision": offer.Revision}, 201)
	id := stringVal(created["invoice_id"])
	f.call("provider_org_admin", "POST", root+"/"+id+"/checkout", "billing-checkout-original-request", record{"version": 1}, 202)
	w := f.request("provider_org_admin", "POST", root+"/"+id+"/checkout", "billing-checkout-original-request", record{"version": 1})
	if w.Code != 202 || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("original checkout intent replaced", w.Code)
	}
	f.call("provider_org_admin", "POST", root+"/"+id+"/checkout", "billing-checkout-duplicate-denial", record{"version": 2}, 409)
	f.call("provider_org_admin", "GET", root+"/"+id+"/checkout", "", nil, 409)
	v, err := loadBillingInvoice(f.ctx, f.s.db, id, "org_provider", false)
	if err != nil || v.checkout != "dispatching" || v.requested == nil || v.dispatched != nil || v.session != nil || v.paid || v.applied != 0 {
		t.Fatal("queue created a session or issued money", v.checkout, err)
	}
	var entries int
	if err = f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_credit_ledger WHERE org_id='org_provider'`).Scan(&entries); err != nil || entries != 0 {
		t.Fatal("checkout admission granted credits", entries, err)
	}
}
