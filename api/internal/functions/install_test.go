package functions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBundleInstallationIsContainedAndImmutable(t *testing.T) {
	parent := t.TempDir()
	archive := archiveFor(t, "index.mjs", "assets/message.txt")
	bundle, err := InstallBundle(archive, parent)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, "bundle-"+bundle.Digest)
	for _, file := range bundle.Files {
		name := filepath.Join(directory, file.Name)
		content, err := os.ReadFile(name)
		if err != nil || string(content) != string(file.Content) {
			t.Fatal("installed bytes changed")
		}
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm()&0222 != 0 {
			t.Fatal("installed source is writable")
		}
	}
	if _, err := InstallBundle(archive, parent); err == nil {
		t.Fatal("existing generation files adopted")
	}
	// TempDir cleanup needs owner write access; this test only adjusts its own
	// generated directories after asserting the sealed installation contract.
	t.Cleanup(func() { _ = os.Chmod(directory, 0755); _ = os.Chmod(filepath.Join(directory, "assets"), 0755) })
}

func TestInvalidBundleCreatesNoFragments(t *testing.T) {
	parent := t.TempDir()
	if _, err := InstallBundle(archiveFor(t, "index.mjs", "../escape"), parent); err == nil {
		t.Fatal("unsafe bundle installed")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid archive left executable fragments")
	}
}
