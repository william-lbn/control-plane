package control

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
)

// These fields describe complete bytes, independently of the opaque ETag and
// part layout. Old objects have no invented CRC/SHA1 values or implicit backfill.
var storageChecksumSizes = map[string]int{
	"X-Amz-Checksum-Sha256":    32,
	"X-Amz-Checksum-Sha1":      20,
	"X-Amz-Checksum-Crc32":     4,
	"X-Amz-Checksum-Crc32c":    4,
	"X-Amz-Checksum-Crc64nvme": 8,
}

func storageStoredChecksums(digest string, stored map[string]string) (map[string]string, error) {
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != 32 || !storageSHA256.MatchString(digest) {
		return nil, errors.New("invalid object checksum identity")
	}
	out := map[string]string{"X-Amz-Checksum-Sha256": base64.StdEncoding.EncodeToString(raw)}
	for name, value := range stored {
		size, ok := storageChecksumSizes[name]
		decoded, e := base64.StdEncoding.Strict().DecodeString(value)
		if !ok || e != nil || len(decoded) != size || base64.StdEncoding.EncodeToString(decoded) != value {
			return nil, errors.New("invalid stored object checksum")
		}
		if name == "X-Amz-Checksum-Sha256" && out[name] != value {
			return nil, errors.New("stored checksum differs from object identity")
		}
		out[name] = value
	}
	return out, nil
}

func (p *storagePayload) storedChecksums() map[string]string {
	out := make(map[string]string, len(storageChecksumSizes))
	for name := range storageChecksumSizes {
		out[name] = p.checksums[name]
	}
	return out
}

func writeStoredStorageChecksums(w http.ResponseWriter, values map[string]string, kinds ...string) {
	for name, value := range values {
		w.Header().Set(name, value)
	}
	kind := "FULL_OBJECT"
	if len(kinds) == 1 {
		kind = kinds[0]
	}
	w.Header().Set("X-Amz-Checksum-Type", kind)
}

type storageAttributeChecksum struct {
	SHA256    string `xml:"ChecksumSHA256,omitempty"`
	SHA1      string `xml:"ChecksumSHA1,omitempty"`
	CRC32     string `xml:"ChecksumCRC32,omitempty"`
	CRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	CRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	Type      string `xml:"ChecksumType,omitempty"`
}

func storageAttributeChecksums(digest string, stored map[string]string, policies ...storageChecksumPolicy) (*storageAttributeChecksum, error) {
	values, err := storageStoredChecksums(digest, stored)
	if err != nil {
		return nil, err
	}
	kind := "FULL_OBJECT"
	if len(policies) == 1 {
		values, kind, err = storagePolicyValues(values, policies[0])
		if err != nil {
			return nil, err
		}
	}
	out := storagePartChecksumXML(values)
	out.Type = kind
	return &out, nil
}
