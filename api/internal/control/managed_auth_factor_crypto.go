package control

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/chacha20poly1305"
)

// This format is checked against the pinned Better Auth 1.7.7 implementation.
// Our provider config uses a single legacy secret, never a versioned keyring.
const managedAuthFactorPlaintextLimit = 4096

var errManagedAuthFactorCipher = errors.New("Auth factor ciphertext or key is not supported")

func openManagedAuthFactor(secret, ciphertext string) ([]byte, error) {
	if len(secret) < 32 || len(secret) > 256 || len(ciphertext) > 2*(managedAuthFactorPlaintextLimit+40)+6 {
		return nil, errManagedAuthFactorCipher
	}
	if strings.HasPrefix(ciphertext, "$ba$") {
		if !strings.HasPrefix(ciphertext, "$ba$0$") {
			return nil, errManagedAuthFactorCipher
		}
		ciphertext = strings.TrimPrefix(ciphertext, "$ba$0$")
	}
	raw, err := hex.DecodeString(ciphertext)
	if err != nil || len(raw) < chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead || len(raw) > managedAuthFactorPlaintextLimit+40 {
		return nil, errManagedAuthFactorCipher
	}
	key := sha256.Sum256([]byte(secret))
	aead, err := chacha20poly1305.NewX(key[:])
	clear(key[:])
	if err != nil {
		return nil, errManagedAuthFactorCipher
	}
	value, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil || !utf8.Valid(value) {
		clear(value)
		return nil, errManagedAuthFactorCipher
	}
	return value, nil
}

func sealManagedAuthFactor(secret string, plaintext []byte, envelope bool) (string, error) {
	if len(secret) < 32 || len(secret) > 256 || len(plaintext) > managedAuthFactorPlaintextLimit || !utf8.Valid(plaintext) {
		return "", errManagedAuthFactorCipher
	}
	key := sha256.Sum256([]byte(secret))
	aead, err := chacha20poly1305.NewX(key[:])
	clear(key[:])
	if err != nil {
		return "", errManagedAuthFactorCipher
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", errManagedAuthFactorCipher
	}
	value := hex.EncodeToString(aead.Seal(nonce, nonce, plaintext, nil))
	if envelope {
		value = "$ba$0$" + value
	}
	return value, nil
}
