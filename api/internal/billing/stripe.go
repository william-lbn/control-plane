// Package billing contains the bounded payment-provider protocol. Permission,
// durable dispatch/inbox records, and atomic entitlement issuance live in the
// control layer. A browser redirect or an unsigned event never proves payment.
package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const BodyLimit = 1 << 20
const MaxMinor = int64(99999999)
const MinMinor = int64(50)
const MaxCredits = int64(1000000000000)

var ErrSignature = errors.New("payment event signature invalid or expired")
var ErrProtocol = errors.New("payment provider response invalid")
var ErrTransport = errors.New("payment provider outcome unavailable")
var stripeID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{2,254}$`)

// CreditReversal uses a cumulative refund, so duplicate and out-of-order events
// cannot introduce rounding drift. A final refund always reverses all credits.
func CreditReversal(credits, paidMinor, refundedMinor int64) (int64, error) {
	if credits < 1 || credits > MaxCredits || paidMinor < 1 || paidMinor > MaxMinor || refundedMinor < 0 || refundedMinor > paidMinor {
		return 0, ErrProtocol
	}
	n := new(big.Int).Mul(big.NewInt(credits), big.NewInt(refundedMinor))
	n.Quo(n, big.NewInt(paidMinor))
	return n.Int64(), nil
}

// VerifyStripeSignature implements the documented v1 HMAC over timestamp+'.'+
// the exact original bytes. Multiple v1 signatures support secret rotation.
func VerifyStripeSignature(body []byte, header string, secrets []string, now time.Time) error {
	if len(body) == 0 || len(body) > BodyLimit || len(header) > 4096 || len(secrets) < 1 || len(secrets) > 2 {
		return ErrSignature
	}
	stamp := ""
	signatures := [][]byte{}
	for _, item := range strings.Split(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			return ErrSignature
		}
		switch name {
		case "t":
			if stamp != "" {
				return ErrSignature
			}
			stamp = value
		case "v1":
			v, err := hex.DecodeString(value)
			if err != nil || len(v) != sha256.Size || len(signatures) >= 8 {
				return ErrSignature
			}
			signatures = append(signatures, v)
		}
	}
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != stamp || seconds <= 0 || len(signatures) == 0 {
		return ErrSignature
	}
	age := now.Sub(time.Unix(seconds, 0))
	if age > 5*time.Minute || age < -30*time.Second {
		return ErrSignature
	}
	matched := false
	for _, secret := range secrets {
		if !strings.HasPrefix(secret, "whsec_") || len(secret) < 24 || len(secret) > 256 {
			return ErrSignature
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = io.WriteString(mac, stamp+".")
		_, _ = mac.Write(body)
		for _, signature := range signatures {
			matched = hmac.Equal(signature, mac.Sum(nil)) || matched
		}
	}
	if !matched {
		return ErrSignature
	}
	return nil
}

// DecodeJSON permits provider extensions, but rejects ambiguous duplicate keys,
// trailing values, and excessive nesting before decoding the typed fields.
func DecodeJSON(body []byte, value any) error {
	if len(body) == 0 || len(body) > BodyLimit {
		return ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 48 {
			return ErrProtocol
		}
		token, err := d.Token()
		if err != nil {
			return ErrProtocol
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return ErrProtocol
				}
				seen[name] = true
				if err = scan(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return ErrProtocol
			}
		case '[':
			for d.More() {
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return ErrProtocol
			}
		default:
			return ErrProtocol
		}
		return nil
	}
	if scan(0) != nil {
		return ErrProtocol
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrProtocol
	}
	if json.Unmarshal(body, value) != nil {
		return ErrProtocol
	}
	return nil
}

type Event struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Type    string `json:"type"`
	Live    *bool  `json:"livemode"`
	Account string `json:"account"`
	Version string `json:"api_version"`
	Created int64  `json:"created"`
	Data    struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type Session struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Mode          string            `json:"mode"`
	Status        string            `json:"status"`
	PaymentStatus string            `json:"payment_status"`
	AmountTotal   int64             `json:"amount_total"`
	Currency      string            `json:"currency"`
	Live          bool              `json:"livemode"`
	URL           string            `json:"url"`
	PaymentIntent string            `json:"payment_intent"`
	ExpiresAt     int64             `json:"expires_at"`
	Reference     string            `json:"client_reference_id"`
	Metadata      map[string]string `json:"metadata"`
}

type Charge struct {
	ID             string `json:"id"`
	Object         string `json:"object"`
	PaymentIntent  string `json:"payment_intent"`
	Amount         int64  `json:"amount"`
	CapturedAmount int64  `json:"amount_captured"`
	Refunded       int64  `json:"amount_refunded"`
	Currency       string `json:"currency"`
	Paid           bool   `json:"paid"`
	Captured       bool   `json:"captured"`
	Disputed       bool   `json:"disputed"`
	Live           bool   `json:"livemode"`
}

type PaymentIntent struct {
	ID           string            `json:"id"`
	Object       string            `json:"object"`
	Status       string            `json:"status"`
	Amount       int64             `json:"amount"`
	Received     int64             `json:"amount_received"`
	Currency     string            `json:"currency"`
	LatestCharge string            `json:"latest_charge"`
	Live         bool              `json:"livemode"`
	Metadata     map[string]string `json:"metadata"`
}

type Dispute struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Charge        string `json:"charge"`
	PaymentIntent string `json:"payment_intent"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	Live          bool   `json:"livemode"`
}

type Stripe struct {
	client                  *http.Client
	origin, secret, version string
}

// Origin is validated against the approved production/fixture network policy by
// the caller. This client never follows a redirect or exposes a provider body
// in an error, and makes no application-level retry of a checkout POST.
func NewStripe(client *http.Client, origin, secret, version string) (*Stripe, error) {
	u, err := url.Parse(origin)
	if client == nil || err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Scheme != "https" && u.Scheme != "http" || len(secret) < 16 || len(secret) > 512 || strings.ContainsAny(secret, "\r\n") || !regexp.MustCompile(`^20[0-9]{2}-[0-9]{2}-[0-9]{2}(\.[a-z]+)?$`).MatchString(version) {
		return nil, ErrProtocol
	}
	c := *client
	c.Timeout = 30 * time.Second
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Stripe{client: &c, origin: origin, secret: secret, version: version}, nil
}

type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("payment provider returned status %d", e.Status)
}

func (s *Stripe) request(ctx context.Context, method, path, key string, form url.Values, value any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r, err := http.NewRequestWithContext(ctx, method, s.origin+path, body)
	if err != nil {
		return ErrProtocol
	}
	r.Header.Set("Authorization", "Bearer "+s.secret)
	r.Header.Set("Stripe-Version", s.version)
	if method == "POST" {
		if len(key) < 16 || len(key) > 200 || strings.ContainsAny(key, "\r\n") {
			return ErrProtocol
		}
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.GetBody = nil
		r.Close = true
	}
	response, err := s.client.Do(r)
	if err != nil {
		return ErrTransport
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, BodyLimit+1))
	if err != nil || len(data) > BodyLimit {
		return ErrProtocol
	}
	if response.StatusCode != 200 {
		return &StatusError{Status: response.StatusCode}
	}
	return DecodeJSON(data, value)
}

func (s *Stripe) Account(ctx context.Context) (string, error) {
	var account struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	if err := s.request(ctx, "GET", "/v1/account", "", nil, &account); err != nil {
		return "", err
	}
	if account.Object != "account" || !strings.HasPrefix(account.ID, "acct_") || !stripeID.MatchString(account.ID) {
		return "", ErrProtocol
	}
	return account.ID, nil
}

type CheckoutInput struct {
	InvoiceID, Binding, Name, ReturnOrigin, IdempotencyKey string
	AmountMinor                                            int64
}

func validOfferName(name string) bool {
	return len(name) >= 1 && len(name) <= 320 && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 80 && strings.TrimSpace(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0
}

func (s *Stripe) CreateSession(ctx context.Context, input CheckoutInput) (Session, error) {
	var out Session
	u, err := url.Parse(input.ReturnOrigin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Scheme != "http" && u.Scheme != "https" || !regexp.MustCompile(`^bil_[a-f0-9]{24}$`).MatchString(input.InvoiceID) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(input.Binding) || !validOfferName(input.Name) || input.AmountMinor < MinMinor || input.AmountMinor > MaxMinor {
		return out, ErrProtocol
	}
	form := url.Values{"mode": {"payment"}, "payment_method_types[0]": {"card"}, "line_items[0][quantity]": {"1"}, "line_items[0][price_data][currency]": {"usd"}, "line_items[0][price_data][unit_amount]": {strconv.FormatInt(input.AmountMinor, 10)}, "line_items[0][price_data][product_data][name]": {input.Name}, "client_reference_id": {input.InvoiceID}, "success_url": {input.ReturnOrigin + "/?billing_invoice=" + input.InvoiceID}, "cancel_url": {input.ReturnOrigin + "/?billing_invoice=" + input.InvoiceID}}
	for _, prefix := range []string{"metadata", "payment_intent_data[metadata]"} {
		form.Set(prefix+"[neon_invoice]", input.InvoiceID)
		form.Set(prefix+"[neon_binding]", input.Binding)
	}
	if err = s.request(ctx, "POST", "/v1/checkout/sessions", input.IdempotencyKey, form, &out); err != nil {
		return out, err
	}
	if err = validSession(out); err != nil || out.AmountTotal != input.AmountMinor || out.Metadata["neon_invoice"] != input.InvoiceID || out.Metadata["neon_binding"] != input.Binding {
		return Session{}, ErrProtocol
	}
	return out, nil
}
func validSession(session Session) error {
	if !strings.HasPrefix(session.ID, "cs_") || !stripeID.MatchString(session.ID) || session.Object != "checkout.session" || session.Mode != "payment" || session.Currency != "usd" || session.AmountTotal < 1 || session.AmountTotal > MaxMinor {
		return ErrProtocol
	}
	return nil
}
func (s *Stripe) Session(ctx context.Context, id string) (Session, error) {
	var out Session
	if !strings.HasPrefix(id, "cs_") || !stripeID.MatchString(id) {
		return out, ErrProtocol
	}
	if err := s.request(ctx, "GET", "/v1/checkout/sessions/"+id, "", nil, &out); err != nil {
		return out, err
	}
	if out.ID != id || validSession(out) != nil {
		return Session{}, ErrProtocol
	}
	return out, nil
}
func (s *Stripe) Intent(ctx context.Context, id string) (PaymentIntent, error) {
	var out PaymentIntent
	if !strings.HasPrefix(id, "pi_") || !stripeID.MatchString(id) {
		return out, ErrProtocol
	}
	if err := s.request(ctx, "GET", "/v1/payment_intents/"+id, "", nil, &out); err != nil {
		return out, err
	}
	if out.ID != id || out.Object != "payment_intent" || out.Currency != "usd" || out.Amount < 1 || out.Amount > MaxMinor {
		return PaymentIntent{}, ErrProtocol
	}
	return out, nil
}
func (s *Stripe) Charge(ctx context.Context, id string) (Charge, error) {
	var out Charge
	if !strings.HasPrefix(id, "ch_") || !stripeID.MatchString(id) {
		return out, ErrProtocol
	}
	if err := s.request(ctx, "GET", "/v1/charges/"+id, "", nil, &out); err != nil {
		return out, err
	}
	if out.ID != id || out.Object != "charge" || out.Currency != "usd" || out.Amount < 1 || out.Amount > MaxMinor || out.Refunded < 0 || out.Refunded > out.Amount {
		return Charge{}, ErrProtocol
	}
	return out, nil
}

func (s *Stripe) Dispute(ctx context.Context, id string) (Dispute, error) {
	var out Dispute
	if !strings.HasPrefix(id, "dp_") && !strings.HasPrefix(id, "du_") || !stripeID.MatchString(id) {
		return out, ErrProtocol
	}
	if err := s.request(ctx, "GET", "/v1/disputes/"+id, "", nil, &out); err != nil {
		return out, err
	}
	if out.ID != id || out.Object != "dispute" || out.Amount < 1 || out.Amount > MaxMinor || out.Currency != "usd" {
		return Dispute{}, ErrProtocol
	}
	return out, nil
}

// Refunds can arrive before the checkout callback. Resolve only the original
// payment intent; never scan arbitrary customers or create another session.
func (s *Stripe) SessionForIntent(ctx context.Context, id string) (Session, error) {
	var out struct {
		Object string    `json:"object"`
		More   bool      `json:"has_more"`
		Items  []Session `json:"data"`
	}
	if !strings.HasPrefix(id, "pi_") || !stripeID.MatchString(id) {
		return Session{}, ErrProtocol
	}
	if err := s.request(ctx, "GET", "/v1/checkout/sessions?payment_intent="+url.QueryEscape(id)+"&limit=2", "", nil, &out); err != nil {
		return Session{}, err
	}
	if out.Object != "list" || out.More || len(out.Items) != 1 || validSession(out.Items[0]) != nil || out.Items[0].PaymentIntent != id {
		return Session{}, ErrProtocol
	}
	return out.Items[0], nil
}
