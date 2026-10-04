// Install a digest-verified official PostgREST binary in a Linux image build.
// No package-manager repository or mutable runtime image is used.
package main

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/ulikunitz/xz"
)

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("destination required")
	}
	var lock struct {
		PostgREST struct {
			URL string `json:"linux_amd64_url"`
			SHA string `json:"linux_amd64_sha256"`
		} `json:"postgrest"`
	}
	data, err := os.ReadFile("containers/services.lock.json")
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &lock); err != nil {
		return err
	}
	u, err := url.Parse(lock.PostgREST.URL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || len(lock.PostgREST.SHA) != 64 {
		return fmt.Errorf("invalid artifact lock")
	}
	client := http.Client{Timeout: 120 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || len(via) > 5 {
			return fmt.Errorf("unsafe artifact redirect")
		}
		return nil
	}}
	response, err := client.Get(lock.PostgREST.URL)
	if err != nil {
		return fmt.Errorf("artifact download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("artifact HTTP %d", response.StatusCode)
	}
	temp, err := os.MkdirTemp("", "postgrest-verified-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	archive := filepath.Join(temp, "postgrest.tar.xz")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(response.Body, (64<<20)+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n > 64<<20 || hex.EncodeToString(hash.Sum(nil)) != lock.PostgREST.SHA {
		return fmt.Errorf("artifact digest or size verification failed")
	}
	compressed, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer compressed.Close()
	decoder, err := xz.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("verified archive decode failed")
	}
	files := tar.NewReader(decoder)
	var binary []byte
	for {
		header, e := files.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if header.Name != "postgrest" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 128<<20 {
			return fmt.Errorf("unexpected binary archive entry")
		}
		binary, e = io.ReadAll(io.LimitReader(files, (128<<20)+1))
		if e != nil {
			return e
		}
		break
	}
	if len(binary) == 0 {
		return fmt.Errorf("verified archive lacks binary")
	}
	if err = os.MkdirAll(os.Args[1], 0755); err != nil {
		return err
	}
	destination := filepath.Join(os.Args[1], "postgrest")
	if err = os.WriteFile(destination, binary, 0755); err != nil {
		return err
	}
	return exec.Command(destination, "--version").Run()
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
