package aigateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProviderSealedCredentialScopeTamperAndPublicProjection(t *testing.T) {
	pub, priv, err := GenerateProviderKeysets("provider-k1")
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewProviderSealer(pub)
	if err != nil {
		t.Fatal(err)
	}
	opener, err := NewProviderOpener(priv)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := opener.PublicKeyset()
	if err != nil || !bytes.Equal(pub, projected) {
		t.Fatal("public projection mismatched")
	}
	if _, err = NewProviderSealer(priv); err == nil {
		t.Fatal("API accepted private key configuration")
	}
	scope := ProviderCredentialScope{ProviderID: "aip_" + strings.Repeat("a", 24), RevisionID: "apv_" + strings.Repeat("b", 24), EndpointSHA256: strings.Repeat("c", 64)}
	secret := "disposable-provider-key-0123456789"
	sealed, err := sealer.Seal(scope, secret)
	if err != nil {
		t.Fatal(err)
	}
	value, err := opener.Open(scope, sealed)
	if err != nil || value != secret {
		t.Fatal("original scope failed to open")
	}
	second, err := sealer.Seal(scope, secret)
	if err != nil || second.Ciphertext == sealed.Ciphertext {
		t.Fatal("ephemeral sealing reused ciphertext")
	}
	raw, _ := json.Marshal(sealed)
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("stored credential included plaintext")
	}
	data, _ := json.Marshal(opener)
	if string(data) != "{}" || strings.Contains(fmt.Sprintf("%v %#v", opener, opener), "private_keys") {
		t.Fatal("private keyring serialized")
	}
	for _, kind := range []string{"provider", "revision", "endpoint", "key-version", "ciphertext", "version", "wrong-key"} {
		t.Run(kind, func(t *testing.T) {
			s, c := scope, sealed
			o := opener
			switch kind {
			case "provider":
				s.ProviderID = "aip_" + strings.Repeat("d", 24)
			case "revision":
				s.RevisionID = "apv_" + strings.Repeat("d", 24)
			case "endpoint":
				s.EndpointSHA256 = strings.Repeat("d", 64)
			case "key-version":
				c.KeyVersion = "missing"
			case "ciphertext":
				b, _ := base64.StdEncoding.DecodeString(c.Ciphertext)
				b[len(b)-1] ^= 1
				c.Ciphertext = base64.StdEncoding.EncodeToString(b)
			case "version":
				c.Version = 2
			case "wrong-key":
				_, private, e := GenerateProviderKeysets("provider-k1")
				if e != nil {
					t.Fatal(e)
				}
				o, e = NewProviderOpener(private)
				if e != nil {
					t.Fatal(e)
				}
			}
			if v, e := o.Open(s, c); e == nil || v != "" {
				t.Fatal("tampered credential or foreign scope opened")
			}
		})
	}
	for _, bad := range []string{"short", strings.Repeat("a", 8193), "disposable-secret\nvalue", "disposable secret value"} {
		if _, err = sealer.Seal(scope, bad); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
}

func TestProviderSealedCredentialKeyRotationRetainsOldVersion(t *testing.T) {
	pub1, priv1, err := GenerateProviderKeysets("k1")
	if err != nil {
		t.Fatal(err)
	}
	_, priv2, err := GenerateProviderKeysets("k2")
	if err != nil {
		t.Fatal(err)
	}
	var a, b providerPrivateKeyset
	if json.Unmarshal(priv1, &a) != nil || json.Unmarshal(priv2, &b) != nil {
		t.Fatal("invalid generated fixture")
	}
	b.Keys["k1"] = a.Keys["k1"]
	raw, _ := json.Marshal(b)
	opener, err := NewProviderOpener(raw)
	if err != nil {
		t.Fatal(err)
	}
	oldSealer, err := NewProviderSealer(pub1)
	if err != nil {
		t.Fatal(err)
	}
	scope := ProviderCredentialScope{ProviderID: "aip_" + strings.Repeat("a", 24), RevisionID: "apv_" + strings.Repeat("b", 24), EndpointSHA256: strings.Repeat("c", 64)}
	old, err := oldSealer.Seal(scope, "disposable-old-provider-key")
	if err != nil {
		t.Fatal(err)
	}
	if value, err := opener.Open(scope, old); err != nil || value != "disposable-old-provider-key" {
		t.Fatal("rotation lost old scope")
	}
	pub2, err := opener.PublicKeyset()
	if err != nil {
		t.Fatal(err)
	}
	current, err := NewProviderSealer(pub2)
	if err != nil {
		t.Fatal(err)
	}
	if current.active != "k2" {
		t.Fatal("new active key not projected")
	}
	next, err := current.Seal(scope, "disposable-new-provider-key")
	if err != nil || next.KeyVersion != "k2" {
		t.Fatal("rotation did not use active key")
	}
	if value, err := opener.Open(scope, next); err != nil || value != "disposable-new-provider-key" {
		t.Fatal("new credential failed")
	}
	delete(b.Keys, "k1")
	raw, _ = json.Marshal(b)
	withoutOld, err := NewProviderOpener(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = withoutOld.Open(scope, old); err == nil {
		t.Fatal("removed private key still opened old credential")
	}
}
