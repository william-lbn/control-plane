package control

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"
)

//go:embed testdata/better_auth_factor_vectors.json
var pinnedManagedAuthFactorVectors []byte

func TestManagedAuthFactorCipherMatchesPinnedBetterAuth(t *testing.T) {
	raw := pinnedManagedAuthFactorVectors
	var fixture struct {
		Provider  string `json:"provider"`
		Synthetic bool   `json:"synthetic_only"`
		Vectors   []struct {
			Secret, Plaintext, Ciphertext string
			Envelope                      bool
		} `json:"vectors"`
	}
	if json.Unmarshal(raw, &fixture) != nil || fixture.Provider != "better-auth-1.7.7" || !fixture.Synthetic || len(fixture.Vectors) != 4 {
		t.Fatal("pinned synthetic SDK fixtures required")
	}
	for _, v := range fixture.Vectors {
		plain, err := openManagedAuthFactor(v.Secret, v.Ciphertext)
		if err != nil || string(plain) != v.Plaintext {
			t.Fatal("pinned provider ciphertext did not authenticate")
		}
		child := v.Secret + "-child"
		encrypted, err := sealManagedAuthFactor(child, plain, v.Envelope)
		if err != nil || encrypted == v.Ciphertext || strings.HasPrefix(encrypted, "$ba$0$") != v.Envelope {
			t.Fatal("child factor was not independently wrapped")
		}
		actual, err := openManagedAuthFactor(child, encrypted)
		if err != nil || !bytes.Equal(actual, plain) {
			t.Fatal("child factor bytes changed")
		}
		if _, err := openManagedAuthFactor(v.Secret, encrypted); err == nil {
			t.Fatal("parent key decrypted child factor")
		}
		clear(plain)
		clear(actual)
	}
}

func TestManagedAuthFactorCipherRejectsTamperingAndUnsupportedEnvelopes(t *testing.T) {
	secret := strings.Repeat("synthetic-fixture", 4)
	value, err := sealManagedAuthFactor(secret, []byte("JBSWY3DPEHPK3PXP"), false)
	if err != nil {
		t.Fatal(err)
	}
	last := "0"
	if value[len(value)-1] == '0' {
		last = "1"
	}
	for _, input := range []string{"", "abc", strings.Repeat("0", 78), value[:len(value)-1] + last, "$ba$1$" + value, "$ba$00$" + value, "$ba$0$invalid", strings.Repeat("0", 2*(managedAuthFactorPlaintextLimit+41))} {
		if _, err := openManagedAuthFactor(secret, input); err == nil {
			t.Fatal("malformed or unsupported factor accepted")
		}
	}
	if _, err := openManagedAuthFactor(secret+"other", value); err == nil {
		t.Fatal("wrong source branch key accepted")
	}
	if _, err := sealManagedAuthFactor("short", []byte("secret"), false); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := sealManagedAuthFactor(secret, make([]byte, managedAuthFactorPlaintextLimit+1), false); err == nil {
		t.Fatal("unbounded plaintext accepted")
	}
}
