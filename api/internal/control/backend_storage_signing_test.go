package control

import (
	"bytes"
	"testing"
)

func TestStorageSigningEnvelopePurposeAndGeneration(t *testing.T) {
	s := &server{backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}}}
	c, e := s.storageSigningCipher("v1")
	if e != nil {
		t.Fatal(e)
	}
	nonce := bytes.Repeat([]byte{2}, c.NonceSize())
	aad := storageSigningAAD("bcr_0123456789abcdef01234567", 1)
	token := "ncb_0123456789abcdef01234567." + string(bytes.Repeat([]byte{'a'}, 64))
	sealed := c.Seal(nonce, nonce, []byte(token), aad)
	if bytes.Contains(sealed, []byte(token)) {
		t.Fatal("plaintext token in envelope")
	}
	if _, e = c.Open(nil, nonce, sealed[c.NonceSize():], storageSigningAAD("bcr_0123456789abcdef01234567", 2)); e == nil {
		t.Fatal("rotation generation swap accepted")
	}
	saved, e := s.savedCipher("v1")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = saved.Open(nil, nonce, sealed[c.NonceSize():], aad); e == nil {
		t.Fatal("cross-purpose envelope accepted")
	}
	if storageSigningSecret(token) == storageSigningSecret(token+"rotated") || len(storageSigningSecret(token)) != 43 {
		t.Fatal("rotation did not change S3 signing identity")
	}
}
