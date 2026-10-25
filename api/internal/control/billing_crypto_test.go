package control

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestBillingCheckoutEnvelopePurposeAndImmutableInvoiceBinding(t *testing.T) {
	s := &server{backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{9}, 32)}}, idempotencyKey: bytes.Repeat([]byte{7}, 32)}
	box := billingCheckoutBox{Invoice: "bil_" + strings.Repeat("a", 24), Org: "org_owned", Provider: strings.Repeat("b", 64), Expires: time.Unix(1800000000, 0)}
	link := "https://checkout.stripe.com/c/pay/cs_synthetic_private_fixture"
	if err := s.billingSealCheckout(&box, link); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(box.Ciphertext, []byte(link)) {
		t.Fatal("checkout bearer link stored in plain text")
	}
	if opened, err := s.billingOpenCheckout(box); err != nil || opened != link {
		t.Fatal(err)
	}
	for _, change := range []func(*billingCheckoutBox){func(b *billingCheckoutBox) { b.Invoice += "changed" }, func(b *billingCheckoutBox) { b.Org = "org_foreign" }, func(b *billingCheckoutBox) { b.Provider = strings.Repeat("c", 64) }, func(b *billingCheckoutBox) { b.Expires = b.Expires.Add(time.Second) }, func(b *billingCheckoutBox) {
		b.Ciphertext = append([]byte{}, b.Ciphertext...)
		b.Ciphertext[len(b.Ciphertext)-1] ^= 1
	}} {
		bad := box
		change(&bad)
		if _, err := s.billingOpenCheckout(bad); err == nil {
			t.Fatal("checkout reference retag/tamper accepted")
		}
	}
	offer := billingOffer{ID: "offer_" + strings.Repeat("a", 24), Revision: strings.Repeat("d", 64), AmountMinor: 125, CreditMicros: 1000000}
	original := s.billingInvoiceBinding(box.Invoice, box.Org, "actor", box.Provider, offer)
	offer.CreditMicros++
	if original == s.billingInvoiceBinding(box.Invoice, box.Org, "actor", box.Provider, offer) {
		t.Fatal("invoice binding ignored offered units")
	}
	oldCipher, _ := s.aiInputCipher("v1")
	if _, err := oldCipher.Open(nil, box.Ciphertext[:oldCipher.NonceSize()], box.Ciphertext[oldCipher.NonceSize():], billingCheckoutAAD(box)); err == nil {
		t.Fatal("billing and AI file encryption domains overlap")
	}
}
