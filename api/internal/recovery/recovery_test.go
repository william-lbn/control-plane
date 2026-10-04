package recovery

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestPrivateAttemptBoundaryAndShellQuote(t *testing.T) {
	root := t.TempDir()
	if !within(filepath.Join(root, "attempt"), root) || within(root, root) || within(filepath.Dir(root), root) || within(root+"-neighbor", root) {
		t.Fatal("private path boundary failed")
	}
	if shellQuote("a'b") != "'a'\"'\"'b'" {
		t.Fatal("literal quote not preserved")
	}
	if !strings.HasPrefix(shellQuote("$(secret)"), "'") {
		t.Fatal("shell substitution not quoted")
	}
}

func TestNegotiationUsesOnlyKnownExactHostKeys(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("192.168.146.101 " + string(ssh.MarshalAuthorizedKey(key)))
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	verify, err := knownhosts.New(path)
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("192.168.146.101"), Port: 22}
	algorithms, err := trustedHostKeyAlgorithms(data, "192.168.146.101:22", remote, verify)
	if err != nil || len(algorithms) != 1 || algorithms[0] != ssh.KeyAlgoED25519 {
		t.Fatal("trusted exact key not selected")
	}
	if _, err = trustedHostKeyAlgorithms(data, "192.168.146.102:22", &net.TCPAddr{IP: net.ParseIP("192.168.146.102"), Port: 22}, verify); err == nil {
		t.Fatal("another destination's key trusted")
	}
}
