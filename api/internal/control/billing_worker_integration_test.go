package control

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/billing"
)

func billingSignedFixture(t *testing.T, f *providerFixture, cfg billingConfig, label, kind, id, object string) {
	t.Helper()
	raw, _ := json.Marshal(record{"id": "evt_" + label, "object": "event", "type": kind, "api_version": cfg.Provider.APIVersion, "created": time.Now().Unix(), "livemode": false, "data": record{"object": record{"id": id, "object": object, "payment_status": "paid", "amount_total": 99999999}}})
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(cfg.Provider.WebhookSecrets[0]))
	mac.Write([]byte(stamp + "."))
	mac.Write(raw)
	r := httptest.NewRequest("POST", "/webhooks/billing/stripe", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Stripe-Signature", "t="+stamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestBillingPostgresProviderReadChainAndOriginalReconciliation(t *testing.T) {
	f := newProviderFixture(t)
	cfg := billingFixtureConfig()
	cfg.Operators = []string{"provider_operator"}
	cfg.Organizations = []string{"org_provider"}
	var obs billingMoneyObservation
	posts, lookups := 0, 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cfg.Provider.Key {
			w.WriteHeader(403)
			return
		}
		if r.Method != "GET" {
			posts++
			w.WriteHeader(405)
			return
		}
		var value any
		switch r.URL.Path {
		case "/v1/account":
			value = record{"id": "acct_fixture", "object": "account"}
		case "/v1/checkout/sessions":
			if r.URL.Query().Get("payment_intent") != obs.intent.ID || r.URL.Query().Get("limit") != "2" {
				w.WriteHeader(400)
				return
			}
			lookups++
			value = record{"object": "list", "has_more": false, "data": []billing.Session{obs.session}}
		case "/v1/checkout/sessions/cs_chain_fixture", "/v1/checkout/sessions/cs_recovery_fixture":
			value = obs.session
		case "/v1/payment_intents/pi_chain_fixture":
			value = obs.intent
		case "/v1/charges/ch_chain_fixture":
			value = obs.charge
		case "/v1/disputes/du_chain_fixture":
			value = obs.dispute
		default:
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(value)
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
	create := func(key string) billingInvoiceState {
		got := f.call("provider_org_admin", "POST", root, key, record{"offer_id": offer.ID, "offer_revision": offer.Revision}, 201)
		id := stringVal(got["invoice_id"])
		f.call("provider_org_admin", "POST", root+"/"+id+"/checkout", key+"-checkout", record{"version": 1}, 202)
		f.exec(`UPDATE billing_invoices SET checkout_dispatched_at=clock_timestamp(),checkout_state='uncertain',version=version+1 WHERE id=$1`, id)
		v, e := loadBillingInvoice(f.ctx, f.s.db, id, "org_provider", false)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	v := create("read-chain-original")
	metadata := map[string]string{"neon_invoice": v.id, "neon_binding": v.binding}
	obs = billingMoneyObservation{session: billing.Session{ID: "cs_chain_fixture", Object: "checkout.session", Mode: "payment", Status: "complete", PaymentStatus: "paid", AmountTotal: 125, Currency: "usd", Reference: v.id, PaymentIntent: "pi_chain_fixture", Metadata: metadata}, intent: billing.PaymentIntent{ID: "pi_chain_fixture", Object: "payment_intent", Status: "succeeded", Amount: 125, Received: 125, Currency: "usd", LatestCharge: "ch_chain_fixture", Metadata: metadata}, charge: billing.Charge{ID: "ch_chain_fixture", Object: "charge", PaymentIntent: "pi_chain_fixture", Amount: 125, CapturedAmount: 125, Currency: "usd", Paid: true, Captured: true, Refunded: 25}}
	work := func() {
		t.Helper()
		worked, e := f.s.billingInboxWorkOnce(f.ctx)
		if e != nil || !worked {
			t.Fatal(worked, e)
		}
	}
	// A refund can arrive before checkout completion and before its session ID
	// was recovered. Resolve exactly one session from the original intent.
	billingSignedFixture(t, f, cfg, "refund_arrives_first", "charge.refunded", obs.charge.ID, "charge")
	work()
	creditCounters(t, f, 800000, 0, 0)
	if lookups != 1 || posts != 0 {
		t.Fatal("original intent lookup or unexpected provider mutation", lookups, posts)
	}
	obs.dispute = &billing.Dispute{ID: "du_chain_fixture", Object: "dispute", Charge: obs.charge.ID, Amount: 200, Currency: "usd", Status: "under_review"}
	obs.charge.Disputed = true
	billingSignedFixture(t, f, cfg, "dispute_above_gross", "charge.dispute.created", obs.dispute.ID, "dispute")
	work()
	creditCounters(t, f, 0, 0, 0)
	var observation []byte
	if err = f.s.db.QueryRow(f.ctx, `SELECT observation FROM billing_inbox WHERE event_id='evt_dispute_above_gross'`).Scan(&observation); err != nil || !bytes.Contains(observation, []byte("200")) {
		t.Fatal("raw disputed amount not retained", err)
	}
	obs.dispute.Status = "prevented"
	obs.charge.Disputed = false
	billingSignedFixture(t, f, cfg, "dispute_prevented", "charge.dispute.closed", obs.dispute.ID, "dispute")
	work()
	creditCounters(t, f, 800000, 0, 0)
	obs.intent.Received = 124
	billingSignedFixture(t, f, cfg, "incorrect_provider_amount", "checkout.session.completed", obs.session.ID, "checkout.session")
	work()
	creditCounters(t, f, 800000, 0, 0)
	var state string
	if err = f.s.db.QueryRow(f.ctx, `SELECT state FROM billing_inbox WHERE event_id='evt_incorrect_provider_amount'`).Scan(&state); err != nil || state != "review" {
		t.Fatal("mismatched receipt not retained for review", state, err)
	}
	obs.intent.Received = 125
	// A different invoice must never acquire or mutate the first invoice's
	// proven provider objects, even through an approved operator request.
	other := create("reconcile-other-original")
	path := "/api/v1/platform/billing/organizations/org_provider/invoices/" + other.id + "/reconcile"
	input := record{"version": other.version, "object_type": "session", "object_id": obs.session.ID, "reason": "Inspect the original provider receipt", "evidence_sha256": strings.Repeat("b", 64)}
	f.call("provider_org_admin", "POST", path, "reconcile-unauthorized", input, 403)
	receipt := f.call("provider_operator", "POST", path, "reconcile-wrong-original", input, 202)
	work()
	creditCounters(t, f, 800000, 0, 0)
	if err = f.s.db.QueryRow(f.ctx, `SELECT state FROM billing_inbox WHERE id=$1`, receipt["inbox_id"]).Scan(&state); err != nil || state != "review" {
		t.Fatal("cross-invoice provider object rebound", state, err)
	}
	// Resolve an unpaid, originally dispatched checkout without another POST.
	metadata = map[string]string{"neon_invoice": other.id, "neon_binding": other.binding}
	obs.session = billing.Session{ID: "cs_recovery_fixture", Object: "checkout.session", Mode: "payment", Status: "open", PaymentStatus: "unpaid", AmountTotal: 125, Currency: "usd", Reference: other.id, Metadata: metadata, ExpiresAt: time.Now().Add(time.Hour).Unix(), URL: peer.URL + "/c/pay/cs_recovery_fixture#synthetic-only"}
	input["object_id"] = obs.session.ID
	input["version"] = other.version
	receipt = f.call("provider_operator", "POST", path, "recover-original-unpaid", input, 202)
	f.call("provider_operator", "POST", path, "recover-original-unpaid", input, 202)
	work()
	creditCounters(t, f, 800000, 0, 0)
	got, err := loadBillingInvoice(f.ctx, f.s.db, other.id, "org_provider", false)
	if err != nil || got.checkout != "ready" || billingString(got.session) != obs.session.ID || got.paid {
		t.Fatal("original unpaid checkout recovery", got.checkout, err)
	}
	link := f.call("provider_org_admin", "GET", root+"/"+other.id+"/checkout", "", nil, 200)
	if stringVal(link["checkout_url"]) != obs.session.URL {
		t.Fatal("recovered original link")
	}
	items := f.call("provider_org_admin", "GET", root+"/"+other.id+"/receipts", "", nil, 200)
	if len(items["items"].([]any)) != 2 {
		t.Fatal("original reconciliation history missing")
	}
	if posts != 0 {
		t.Fatal("reconciliation created another checkout")
	}
}
