// Package functions implements the self-hosted Functions runtime contract.
// It does not enable a product capability until the isolated runner passes live
// acceptance. Bundles and environment values must never be written to logs.
package functions

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	Runtime             = "nodejs24"
	MaxArchiveBytes     = 8 << 20
	MaxExpandedBytes    = 16 << 20
	MaxFileBytes        = 8 << 20
	MaxFiles            = 128
	MaxEnvironmentBytes = 32 << 10
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]{1,20}$`)
var environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Bundle contains fully validated bytes. Consumers publish files only after
// ValidateBundle returns, so a late CRC error cannot leave executable fragments.
type Bundle struct {
	Digest        string
	Entry         string
	Files         []File
	ExpandedBytes int64
}

type File struct {
	Name    string
	Content []byte
}

func ValidSlug(slug string) bool { return slugPattern.MatchString(slug) }

func ValidateBundle(archive []byte) (Bundle, error) {
	var result Bundle
	if len(archive) == 0 || len(archive) > MaxArchiveBytes {
		return result, errors.New("bundle exceeds compressed size limit or is empty")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > MaxFiles {
		return result, errors.New("bundle must be a bounded, nonempty ZIP archive")
	}
	names := make(map[string]bool)
	for _, file := range reader.File {
		name := file.Name
		if !utf8.ValidString(name) || len(name) > 240 || name == "" || strings.ContainsAny(name, "\\:") || strings.IndexFunc(name, unicode.IsControl) >= 0 || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == "." || name == ".." || name == ".neon" || strings.HasPrefix(name, "../") || strings.HasPrefix(name, ".neon/") || names[name] || !file.Mode().IsRegular() || file.Flags&1 != 0 {
			return Bundle{}, errors.New("bundle contains a duplicate, unsafe or nonregular path")
		}
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return Bundle{}, errors.New("bundle uses an unsupported compression method")
		}
		if file.UncompressedSize64 > MaxFileBytes || file.UncompressedSize64 > uint64(MaxExpandedBytes-result.ExpandedBytes) {
			return Bundle{}, errors.New("bundle exceeds expanded size limit")
		}
		stream, err := file.Open()
		if err != nil {
			return Bundle{}, errors.New("bundle entry cannot be read")
		}
		content, readErr := io.ReadAll(io.LimitReader(stream, MaxFileBytes+1))
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || len(content) > MaxFileBytes || uint64(len(content)) != file.UncompressedSize64 {
			return Bundle{}, errors.New("bundle entry failed length or integrity verification")
		}
		result.ExpandedBytes += int64(len(content))
		if result.ExpandedBytes > MaxExpandedBytes {
			return Bundle{}, errors.New("bundle exceeds expanded size limit")
		}
		names[name] = true
		result.Files = append(result.Files, File{Name: name, Content: content})
	}
	// Reject file/directory collisions before any extraction. A deployment must
	// contain one unambiguous ESM entry rather than rely on filesystem ordering.
	for name := range names {
		for prefix := path.Dir(name); prefix != "."; prefix = path.Dir(prefix) {
			if names[prefix] {
				return Bundle{}, errors.New("bundle contains a file/directory collision")
			}
		}
	}
	if names["index.mjs"] == names["index.js"] {
		return Bundle{}, errors.New("bundle requires exactly one index.mjs or index.js entry")
	}
	result.Entry = "index.mjs"
	if names["index.js"] {
		result.Entry = "index.js"
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Name < result.Files[j].Name })
	digest := sha256.Sum256(archive)
	result.Digest = hex.EncodeToString(digest[:])
	return result, nil
}

// ValidateEnvironment rejects collisions with supervisor and SQL credentials.
// Errors deliberately omit values, which may contain customer secrets.
func ValidateEnvironment(environment map[string]string) error {
	if environment == nil || len(environment) > 32 {
		return errors.New("environment must be an object with at most 32 keys")
	}
	var total int
	for name, value := range environment {
		upper := strings.ToUpper(name)
		if !environmentPattern.MatchString(name) || upper == "DATABASE_URL" || upper == "PATH" || upper == "HOME" || upper == "TMPDIR" || upper == "PORT" || upper == "HOST" || strings.HasPrefix(upper, "NODE") || strings.HasPrefix(upper, "PG") || strings.HasPrefix(upper, "NEON_") || strings.HasPrefix(upper, "PLATFORM_") {
			return errors.New("environment contains an invalid or platform-reserved key")
		}
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > 8192 {
			return errors.New("environment value exceeds format or size limits")
		}
		total += len(name) + len(value)
	}
	if total > MaxEnvironmentBytes {
		return errors.New("environment exceeds total size limit")
	}
	return nil
}
