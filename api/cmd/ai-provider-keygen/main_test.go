package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

func TestAIProviderKeygenExclusivePrivateOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "operator")
	if err := generate(dir, "test-k1"); err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(dir, "public-keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := os.ReadFile(filepath.Join(dir, "private-keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	opener, err := aigateway.NewProviderOpener(priv)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := opener.PublicKeyset()
	if err != nil || !bytes.Equal(pub, actual) {
		t.Fatal("private/public output mismatched")
	}
	info, err := os.Stat(filepath.Join(dir, "private-keyring.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("private output permissions changed")
	}
	if err = generate(dir, "test-k2"); err == nil {
		t.Fatal("existing operator directory overwritten")
	}
	retained, _ := os.ReadFile(filepath.Join(dir, "private-keyring.json"))
	if !bytes.Equal(priv, retained) {
		t.Fatal("failed second generation replaced original private key")
	}
}
