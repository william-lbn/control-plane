package control

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
)

// Storage receives only the existing event MAC, never Guest CA/artifact keys or
// Functions runtime credentials. A configured trigger without a matching MAC
// fails publication, preserving the original transactional outbox guarantee.
func loadStorageGatewayEvents() (*functionsPlatform, error) {
	p := &functionsPlatform{}
	path, version := os.Getenv("NEON_STORAGE_EVENT_MAC_KEY_FILE"), os.Getenv("NEON_STORAGE_EVENT_KEY_VERSION")
	if path == "" && version == "" {
		return p, nil
	}
	if path == "" || !functionMACKeyVersion.MatchString(version) {
		return nil, errors.New("Storage event MAC identity incomplete")
	}
	raw, err := functionPrivateFile(path, 256)
	if err != nil {
		return nil, errors.New("Storage event MAC unavailable")
	}
	p.key, err = base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(p.key) != 32 {
		return nil, errors.New("Storage event MAC invalid")
	}
	p.input.KeyVersion = version
	return p, nil
}
