package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type billingCheckoutBox struct {
	Invoice, Org, Provider, KeyVersion string
	Expires                            time.Time
	Ciphertext                         []byte
}

func (s *server) billingCipher(version string) (cipher.AEAD, error) {
	if s.backendKeys == nil || len(s.backendKeys.decoded[version]) < 32 {
		return nil, errors.New("billing encryption key unavailable")
	}
	mac := hmac.New(sha256.New, s.backendKeys.decoded[version])
	mac.Write([]byte("neon-billing-checkout-encryption-v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func billingCheckoutAAD(box billingCheckoutBox) []byte {
	raw, _ := json.Marshal([]any{"neon-billing-checkout-v1", box.Invoice, box.Org, box.Provider, box.KeyVersion, box.Expires.Unix()})
	return raw
}
func (s *server) billingSealCheckout(box *billingCheckoutBox, link string) error {
	if len(link) == 0 || len(link) > 8192 || s.backendKeys == nil || box.Invoice == "" || box.Org == "" || box.Provider == "" || box.Expires.IsZero() {
		return errors.New("billing checkout identity unavailable")
	}
	box.KeyVersion = s.backendKeys.Active
	aead, err := s.billingCipher(box.KeyVersion)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	box.Ciphertext = aead.Seal(nonce, nonce, []byte(link), billingCheckoutAAD(*box))
	return nil
}
func (s *server) billingOpenCheckout(box billingCheckoutBox) (string, error) {
	aead, err := s.billingCipher(box.KeyVersion)
	if err != nil || len(box.Ciphertext) < 28 || len(box.Ciphertext) > 16384 {
		return "", errors.New("billing checkout envelope unavailable")
	}
	data, err := aead.Open(nil, box.Ciphertext[:aead.NonceSize()], box.Ciphertext[aead.NonceSize():], billingCheckoutAAD(box))
	if err != nil || len(data) > 8192 {
		return "", errors.New("billing checkout envelope unavailable")
	}
	return string(data), nil
}
func (s *server) billingInvoiceBinding(id, org, actor, provider string, offer billingOffer) string {
	raw, _ := json.Marshal([]any{"neon-billing-invoice-v1", id, org, actor, provider, offer.ID, offer.Revision, offer.AmountMinor, offer.CreditMicros, "USD"})
	mac := hmac.New(sha256.New, s.idempotencyKey)
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}
