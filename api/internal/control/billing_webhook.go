package control

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/billing"
)

var billingProviderObjectID = regexp.MustCompile(`^(evt|cs|pi|ch|dp|du)_[A-Za-z0-9_]{3,240}$`)

func billingEventObject(kind string) (string, bool) {
	switch kind {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded", "checkout.session.expired":
		return "cs_", true
	case "charge.refunded":
		return "ch_", true
	case "charge.dispute.created", "charge.dispute.updated", "charge.dispute.closed":
		return "dp_", true
	}
	return "", false
}

// This route has provider authentication rather than a browser session/CSRF.
// Acknowledge only the committed minimal inbox; no payment is fulfilled here.
func (s *server) billingWebhook(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil {
		fail(w, r, 503, "billing_disabled", "Billing webhook is not configured")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || len(r.Header.Values("Stripe-Signature")) != 1 || r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
		fail(w, r, 400, "billing_event_invalid", "A signed original JSON event is required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, billing.BodyLimit))
	if err != nil || billing.VerifyStripeSignature(body, r.Header.Get("Stripe-Signature"), s.billing.config.Provider.WebhookSecrets, time.Now()) != nil {
		fail(w, r, 400, "billing_signature_invalid", "Payment event signature is invalid or expired")
		return
	}
	var event billing.Event
	if billing.DecodeJSON(body, &event) != nil || !strings.HasPrefix(event.ID, "evt_") || !billingProviderObjectID.MatchString(event.ID) || event.Object != "event" || event.Created < 1 || event.Live == nil || event.Version != s.billing.config.Provider.APIVersion || *event.Live != s.billing.config.Provider.Live || event.Account != "" && event.Account != s.billing.config.Provider.Account || len(event.Type) == 0 || len(event.Type) > 128 {
		fail(w, r, 400, "billing_event_invalid", "Payment event does not match the approved merchant, mode or version")
		return
	}
	var object struct {
		ID   string `json:"id"`
		Type string `json:"object"`
	}
	prefix, supported := billingEventObject(event.Type)
	state := "pending"
	if !supported {
		state = "ignored"
		object.ID = event.ID
	} else if billing.DecodeJSON(event.Data.Object, &object) != nil || (!strings.HasPrefix(object.ID, prefix) && !(prefix == "dp_" && strings.HasPrefix(object.ID, "du_"))) || !billingProviderObjectID.MatchString(object.ID) || object.Type != map[string]string{"cs_": "checkout.session", "ch_": "charge", "dp_": "dispute"}[prefix] {
		fail(w, r, 400, "billing_event_invalid", "Payment event has an invalid provider object reference")
		return
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('billing-event:'||$1||':'||$2,0))`, s.billing.identity, event.ID); err != nil {
		billingFailure(w, r, err)
		return
	}
	var id, oldType, oldObject, version string
	var live bool
	err = tx.QueryRow(ctx, `SELECT id,event_type,object_id,api_version,livemode FROM billing_inbox WHERE provider_identity=$1 AND event_id=$2`, s.billing.identity, event.ID).Scan(&id, &oldType, &oldObject, &version, &live)
	if err == nil {
		if oldType != event.Type || oldObject != object.ID || version != event.Version || live != *event.Live {
			fail(w, r, 409, "billing_event_conflict", "Provider event ID conflicts with its original receipt")
			return
		}
		// JSON formatting or harmless provider extension changes do not create a
		// new business event. Its first raw-body proof remains immutable.
		jsonResponse(w, 200, record{"receipt_id": id, "replayed": true})
		return
	}
	if !isNoRows(err) {
		billingFailure(w, r, err)
		return
	}
	if supported {
		var pending int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM billing_inbox WHERE provider_identity=$1 AND state IN ('pending','processing')`, s.billing.identity).Scan(&pending); err != nil {
			billingFailure(w, r, err)
			return
		}
		if pending >= 1000 {
			fail(w, r, 429, "billing_inbox_limit", "Retain the original event and retry delivery later")
			return
		}
	}
	id = newID("ble_")
	_, err = tx.Exec(ctx, `INSERT INTO billing_inbox(id,provider_identity,event_id,event_type,object_id,api_version,livemode,payload_sha256,source_kind,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'signed_webhook',$9)`, id, s.billing.identity, event.ID, event.Type, object.ID, event.Version, *event.Live, hash, state)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	jsonResponse(w, 202, record{"receipt_id": id, "accepted": true})
}
