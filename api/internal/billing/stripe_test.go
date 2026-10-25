package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStripeSignedOriginalBytesFreshnessAndRotation(t *testing.T) {
	secret := "whsec_synthetic_fixture_not_an_actual_credential"
	now := time.Unix(1800000000, 0)
	body := []byte(`{ "id":"evt_fixture", "data":{"object":{"id":"cs_fixture"}} }`)
	sign := func(stamp string, data []byte, key string) string {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(stamp + "."))
		mac.Write(data)
		return hex.EncodeToString(mac.Sum(nil))
	}
	header := "t=1800000000,v1=" + sign("1800000000", body, secret)
	if err := VerifyStripeSignature(body, header, []string{secret}, now); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStripeSignature(body, header, []string{"whsec_synthetic_retired_rotation_credential", secret}, now); err != nil {
		t.Fatal("rotation", err)
	}
	for _, input := range []struct {
		body   []byte
		header string
		now    time.Time
	}{
		{append([]byte(" "), body...), header, now},
		{body, header, now.Add(301 * time.Second)},
		{body, header, now.Add(-31 * time.Second)},
		{body, header + ",t=1800000000", now},
		{body, strings.Replace(header, "1800000000", "01800000000", 1), now},
		{body, "t=1800000000,v1=" + strings.Repeat("0", 64), now},
	} {
		if VerifyStripeSignature(input.body, input.header, []string{secret}, input.now) == nil {
			t.Fatal("invalid signed event accepted")
		}
	}
	rotated := header + ",v1=" + sign("1800000000", body, "whsec_synthetic_second_valid_signing_secret")
	if err := VerifyStripeSignature(body, rotated, []string{"whsec_synthetic_second_valid_signing_secret"}, now); err != nil {
		t.Fatal("multiple v1 signatures", err)
	}
}

func TestStripeStrictJSONAndExactCumulativeRefundArithmetic(t *testing.T) {
	var out struct {
		Amount int64 `json:"amount"`
	}
	if DecodeJSON([]byte(`{"amount":125,"provider_extension":true}`), &out) != nil || out.Amount != 125 {
		t.Fatal("provider extension blocked")
	}
	for _, body := range []string{`{"amount":1,"amount":2}`, `{"extra":{"id":"a","id":"b"},"amount":1}`, `{"amount":1} {}`, `{"amount":1.5}`, strings.Repeat("[", 50) + "0" + strings.Repeat("]", 50)} {
		if DecodeJSON([]byte(body), &out) == nil {
			t.Fatal("ambiguous/invalid money JSON accepted", body)
		}
	}
	for _, row := range []struct{ credits, paid, refunded, want int64 }{{100, 100, 0, 0}, {100, 100, 25, 25}, {101, 100, 25, 25}, {101, 100, 99, 99}, {101, 100, 100, 101}, {MaxCredits, MaxMinor, MaxMinor - 1, 999999989999}, {MaxCredits, MaxMinor, MaxMinor, MaxCredits}} {
		got, err := CreditReversal(row.credits, row.paid, row.refunded)
		if err != nil || got != row.want {
			t.Fatal(row, got, err)
		}
	}
	for _, row := range [][3]int64{{0, 1, 0}, {1, 0, 0}, {1, 1, 2}, {MaxCredits + 1, 1, 1}, {1, MaxMinor + 1, 1}, {1, 1, -1}} {
		if _, err := CreditReversal(row[0], row[1], row[2]); err == nil {
			t.Fatal("refund bounds", row)
		}
	}
}

func TestStripeBoundedHTTPCheckoutBindingAndNoRedirect(t *testing.T) {
	var posts, leaks atomic.Int64
	invoice := "bil_" + strings.Repeat("a", 24)
	binding := strings.Repeat("b", 64)
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks.Add(1); w.WriteHeader(200) }))
	defer external.Close()
	var responseMode atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-provider-secret-at-least-32-characters" || r.Header.Get("Stripe-Version") != "2024-06-20" {
			t.Error("provider request identity/version")
			w.WriteHeader(403)
			return
		}
		if responseMode.Load() == 1 {
			w.Header().Set("Location", external.URL)
			w.WriteHeader(302)
			return
		}
		if responseMode.Load() == 2 {
			w.WriteHeader(500)
			fmt.Fprint(w, "synthetic-provider-secret-at-least-32-characters")
			return
		}
		switch r.URL.Path {
		case "/v1/account":
			fmt.Fprint(w, `{"id":"acct_fixture","object":"account"}`)
		case "/v1/checkout/sessions":
			posts.Add(1)
			if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "original-invoice-attempt-key" {
				t.Error("checkout dispatch key")
			}
			if r.ParseForm() != nil || r.Form.Get("line_items[0][price_data][unit_amount]") != "125" || r.Form.Get("metadata[neon_binding]") != binding || r.Form.Get("payment_intent_data[metadata][neon_invoice]") != invoice || r.Form.Get("mode") != "payment" {
				t.Error("fixed amount/binding not sent")
			}
			amount := 125
			if responseMode.Load() == 3 {
				amount = 126
			}
			fmt.Fprintf(w, `{"id":"cs_fixture","object":"checkout.session","mode":"payment","currency":"usd","amount_total":%d,"metadata":{"neon_invoice":%q,"neon_binding":%q}}`, amount, invoice, binding)
		case "/v1/checkout/sessions/cs_fixture":
			fmt.Fprintf(w, `{"id":"cs_fixture","object":"checkout.session","mode":"payment","currency":"usd","amount_total":125,"metadata":{"neon_invoice":%q,"neon_binding":%q}}`, invoice, binding)
		case "/v1/payment_intents/pi_fixture":
			fmt.Fprint(w, `{"id":"pi_fixture","object":"payment_intent","amount":125,"currency":"usd"}`)
		case "/v1/charges/ch_fixture":
			fmt.Fprint(w, `{"id":"ch_fixture","object":"charge","amount":125,"amount_refunded":25,"currency":"usd"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	client, err := NewStripe(provider.Client(), provider.URL, "synthetic-provider-secret-at-least-32-characters", "2024-06-20")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account, err := client.Account(ctx)
	if err != nil || account != "acct_fixture" {
		t.Fatal(account, err)
	}
	input := CheckoutInput{InvoiceID: invoice, Binding: binding, Name: "Explicit synthetic credit offer", ReturnOrigin: "https://console.example.test", IdempotencyKey: "original-invoice-attempt-key", AmountMinor: 125}
	if _, err := client.CreateSession(ctx, input); err != nil || posts.Load() != 1 {
		t.Fatal("checkout", err, posts.Load())
	}
	if _, err := client.Session(ctx, "cs_fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Intent(ctx, "pi_fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Charge(ctx, "ch_fixture"); err != nil {
		t.Fatal(err)
	}
	responseMode.Store(1)
	if _, err := client.Account(ctx); err == nil || leaks.Load() != 0 {
		t.Fatal("redirect leaked credential", err, leaks.Load())
	}
	responseMode.Store(2)
	if _, err := client.Account(ctx); err == nil || strings.Contains(err.Error(), "synthetic-provider-secret") {
		t.Fatal("provider error leaked credential", err)
	}
	responseMode.Store(3)
	if _, err := client.CreateSession(ctx, input); !errors.Is(err, ErrProtocol) || posts.Load() != 2 {
		t.Fatal("checkout price mismatch accepted/retried", err, posts.Load())
	}
	if _, err := client.Session(ctx, url.PathEscape("cs_fixture/../../account")); err == nil {
		t.Fatal("provider path injection")
	}
	for _, amount := range []int64{0, 49, 100000000} {
		bad := input
		bad.AmountMinor = amount
		if _, err := client.CreateSession(ctx, bad); !errors.Is(err, ErrProtocol) || posts.Load() != 2 {
			t.Fatal("unsupported USD amount dispatched", amount, err)
		}
	}
}

func TestStripeOriginalIntentResolutionAndCurrentDisputeShape(t *testing.T) {
	var multiple atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/disputes/du_fixture":
			fmt.Fprint(w, `{"id":"du_fixture","object":"dispute","charge":"ch_fixture","payment_intent":null,"amount":150,"currency":"usd","livemode":false,"status":"prevented"}`)
		case "/v1/checkout/sessions":
			if r.URL.Query().Get("payment_intent") != "pi_fixture" || r.URL.Query().Get("limit") != "2" {
				t.Error("unbounded or unrelated checkout lookup")
			}
			item := `{"id":"cs_fixture","object":"checkout.session","mode":"payment","amount_total":125,"currency":"usd","payment_intent":"pi_fixture"}`
			items := item
			if multiple.Load() {
				items += "," + item
			}
			fmt.Fprint(w, `{"object":"list","has_more":false,"data":[`+items+`]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	client, err := NewStripe(peer.Client(), peer.URL, "synthetic-provider-secret-at-least-32-characters", "2024-06-20")
	if err != nil {
		t.Fatal(err)
	}
	d, err := client.Dispute(context.Background(), "du_fixture")
	if err != nil || d.Status != "prevented" || d.PaymentIntent != "" || d.Amount != 150 {
		t.Fatal("current documented dispute shape", d, err)
	}
	s, err := client.SessionForIntent(context.Background(), "pi_fixture")
	if err != nil || s.ID != "cs_fixture" {
		t.Fatal("original checkout lookup", s, err)
	}
	multiple.Store(true)
	if _, err = client.SessionForIntent(context.Background(), "pi_fixture"); !errors.Is(err, ErrProtocol) {
		t.Fatal("ambiguous original checkout accepted", err)
	}
}

func TestStripeCheckoutPreservesApprovedUnicodeOfferName(t *testing.T) {
	invoice, binding := "bil_"+strings.Repeat("a", 24), strings.Repeat("b", 64)
	var sent string
	var posts int
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		r.ParseForm()
		sent = r.Form.Get("line_items[0][price_data][product_data][name]")
		fmt.Fprintf(w, `{"id":"cs_unicode_fixture","object":"checkout.session","mode":"payment","amount_total":125,"currency":"usd","metadata":{"neon_invoice":%q,"neon_binding":%q}}`, invoice, binding)
	}))
	defer peer.Close()
	client, err := NewStripe(peer.Client(), peer.URL, "synthetic-provider-secret-at-least-32-characters", "2024-06-20")
	if err != nil {
		t.Fatal(err)
	}
	input := CheckoutInput{InvoiceID: invoice, Binding: binding, ReturnOrigin: "https://console.example.test", IdempotencyKey: "unicode-original-invoice-key", AmountMinor: 125}
	for _, name := range []string{strings.Repeat("额", 80), strings.Repeat("𠮷", 80)} {
		input.Name = name
		if _, err = client.CreateSession(context.Background(), input); err != nil || sent != name {
			t.Fatal("approved Unicode offer rejected or changed", err)
		}
	}
	for _, name := range []string{strings.Repeat("𠮷", 81), strings.Repeat("a", 81), " invalid trim", "invalid\ncontrol", string([]byte{0xff})} {
		input.Name = name
		if _, err = client.CreateSession(context.Background(), input); !errors.Is(err, ErrProtocol) || posts != 2 {
			t.Fatal("invalid offer dispatched", err, posts)
		}
	}
}
