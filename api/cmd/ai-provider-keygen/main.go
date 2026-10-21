// ai-provider-keygen writes an operator's new exclusive keyring directory.
// Private bytes never reach stdout, CLI arguments, or the source checkout.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	return closed
}

func generate(folder, version string) error {
	if folder == "" || !filepath.IsAbs(folder) {
		return fmt.Errorf("an absolute new private output directory is required")
	}
	pub, priv, err := aigateway.GenerateProviderKeysets(version)
	if err != nil {
		return fmt.Errorf("provider key version rejected")
	}
	defer clear(priv)
	if err = os.Mkdir(folder, 0o700); err != nil {
		return fmt.Errorf("exclusive provider output directory unavailable; preserve any prior attempt")
	}
	if err = writeExclusive(filepath.Join(folder, "public-keyring.json"), pub, 0o644); err != nil {
		return fmt.Errorf("public key output failed; preserve original output directory")
	}
	if err = writeExclusive(filepath.Join(folder, "private-keyring.json"), priv, 0o600); err != nil {
		return fmt.Errorf("private key output failed; preserve original output directory")
	}
	return nil
}

func main() {
	folder := flag.String("output", "", "new absolute private output directory")
	version := flag.String("version", "provider-v1", "operator key version")
	flag.Parse()
	if flag.NArg() != 0 || generate(*folder, *version) != nil {
		fmt.Fprintln(os.Stderr, "provider key generation failed; retain exclusive output and inspect operator inputs")
		os.Exit(1)
	}
	fmt.Println("Provider keyring generated in the protected output directory; public projection and private file are separate")
}
