package control

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Internal checksums always cover complete bytes. This separate publication
// policy records the negotiated S3 checksum, including composite part count.
// Copies retain it even though copying does not retain a multipart layout.
type storageChecksumPolicy struct {
	Algorithm string `json:"algorithm,omitempty"`
	Type      string `json:"type,omitempty"`
	Value     string `json:"value,omitempty"`
	Parts     int    `json:"parts_count,omitempty"`
}

type multipartWirePart struct {
	Number   int       `xml:"PartNumber"`
	ETag     string    `xml:"ETag"`
	Size     int64     `xml:"Size"`
	Modified time.Time `xml:"LastModified"`
	storageAttributeChecksum
}

var storageChecksumAlgorithms = map[string]string{
	"CRC32": "X-Amz-Checksum-Crc32", "CRC32C": "X-Amz-Checksum-Crc32c",
	"CRC64NVME": "X-Amz-Checksum-Crc64nvme", "SHA1": "X-Amz-Checksum-Sha1", "SHA256": "X-Amz-Checksum-Sha256",
}

func normalizeMultipartChecksum(algorithm, kind string) (string, error) {
	if algorithm == "" {
		if kind != "" {
			return "", multipartFault(400, "InvalidArgument")
		}
		return "", nil
	}
	if storageChecksumAlgorithms[algorithm] == "" {
		return "", multipartFault(501, "NotImplemented")
	}
	if kind == "" {
		kind = "COMPOSITE"
		if algorithm == "CRC64NVME" {
			kind = "FULL_OBJECT"
		}
	}
	if kind != "FULL_OBJECT" && kind != "COMPOSITE" || kind == "COMPOSITE" && algorithm == "CRC64NVME" || kind == "FULL_OBJECT" && (algorithm == "SHA1" || algorithm == "SHA256") {
		return "", multipartFault(400, "InvalidRequest")
	}
	return kind, nil
}

// Operation-specific, closed grammar. Never silently ignore a checksum header
// or accept duplicate values, including on list/abort requests.
func multipartChecksumHeaders(h http.Header, operation string) error {
	for raw, values := range h {
		name := http.CanonicalHeaderKey(raw)
		if !strings.HasPrefix(strings.ToLower(raw), "x-amz-checksum-") && name != "X-Amz-Sdk-Checksum-Algorithm" && name != "X-Amz-Mp-Object-Size" {
			continue
		}
		allowed := false
		switch operation {
		case "create":
			allowed = name == "X-Amz-Checksum-Algorithm" || name == "X-Amz-Checksum-Type"
		case "part":
			allowed = storageChecksumSizes[name] != 0 || name == "X-Amz-Sdk-Checksum-Algorithm"
		case "complete":
			allowed = storageChecksumSizes[name] != 0 || name == "X-Amz-Checksum-Type" || name == "X-Amz-Mp-Object-Size"
		}
		if !allowed {
			return multipartFault(501, "NotImplemented")
		}
		if len(values) != 1 || values[0] == "" {
			return multipartFault(400, "InvalidArgument")
		}
	}
	return nil
}

func multipartPartChecksums(u multipartUpload, payload *storagePayload, h http.Header, copied bool) (map[string]string, error) {
	if err := multipartChecksumHeaders(h, "part"); err != nil {
		return nil, err
	}
	name := storageChecksumAlgorithms[u.ChecksumAlgorithm]
	if sdk := h.Get("X-Amz-Sdk-Checksum-Algorithm"); sdk != "" && sdk != u.ChecksumAlgorithm {
		return nil, multipartFault(400, "InvalidRequest")
	}
	for field := range storageChecksumSizes {
		if values := h.Values(field); len(values) != 0 && (field != name || values[0] != payload.checksums[field]) {
			return nil, multipartFault(400, "BadDigest")
		}
	}
	if u.ChecksumType == "COMPOSITE" && !copied && h.Get(name) == "" {
		return nil, multipartFault(400, "BadDigest")
	}
	return payload.storedChecksums(), nil
}

type multipartCompletionChecksums struct {
	Type   string            `json:"checksum_type,omitempty"`
	Values map[string]string `json:"checksums,omitempty"`
	Size   *int64            `json:"object_size,omitempty"`
}

func multipartCompletionHeaders(h http.Header) (multipartCompletionChecksums, error) {
	out := multipartCompletionChecksums{Type: h.Get("X-Amz-Checksum-Type")}
	if err := multipartChecksumHeaders(h, "complete"); err != nil {
		return out, err
	}
	for name := range storageChecksumSizes {
		if value := h.Get(name); value != "" {
			if out.Values == nil {
				out.Values = map[string]string{}
			}
			out.Values[name] = value
		}
	}
	if value := h.Get("X-Amz-Mp-Object-Size"); value != "" {
		if strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return out, multipartFault(400, "InvalidArgument")
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 || n > storageObjectLimit {
			return out, multipartFault(400, "InvalidArgument")
		}
		out.Size = &n
	}
	return out, nil
}

func multipartCompletionFingerprint(parts []multipartSelection, match, none string, options []multipartCompletionChecksums) (string, multipartCompletionChecksums, error) {
	if len(options) > 1 {
		return "", multipartCompletionChecksums{}, multipartFault(400, "InvalidArgument")
	}
	legacy, err := multipartManifestHash(parts, match, none)
	if err != nil {
		return "", multipartCompletionChecksums{}, err
	}
	var opt multipartCompletionChecksums
	if len(options) == 1 {
		opt = options[0]
	}
	if opt.Type == "" && len(opt.Values) == 0 && opt.Size == nil {
		return legacy, opt, nil
	}
	// Retain the exact legacy hash for requests without the new optional fields.
	raw, err := json.Marshal(record{"manifest": legacy, "checksum": opt})
	if err != nil {
		return "", opt, err
	}
	p, err := smallStoragePayload(raw)
	if err != nil {
		return "", opt, err
	}
	return p.SHA256, opt, nil
}

func multipartValidateSelection(u multipartUpload, selected multipartSelection, part multipartPart, index int) error {
	name := storageChecksumAlgorithms[u.ChecksumAlgorithm]
	if name == "" {
		if len(selected.Checksums) != 0 {
			return multipartFault(400, "InvalidRequest")
		}
		return nil
	}
	if selected.Number != index+1 {
		return multipartFault(400, "InvalidPartOrder")
	}
	values, err := storageStoredChecksums(part.SHA256, part.Checksums)
	if err != nil || values[name] == "" {
		return multipartFault(409, "InvalidPart")
	}
	if u.ChecksumType == "COMPOSITE" && selected.Checksums[name] == "" {
		return multipartFault(400, "InvalidPart")
	}
	for field, value := range selected.Checksums {
		if field != name || value != values[field] {
			return multipartFault(400, "InvalidPart")
		}
	}
	return nil
}

func multipartPublishedChecksum(u multipartUpload, selection []multipartSelection, parts map[int]multipartPart, content *storagePayload, opt multipartCompletionChecksums) (storageChecksumPolicy, error) {
	policy := storageChecksumPolicy{}
	name := storageChecksumAlgorithms[u.ChecksumAlgorithm]
	if opt.Size != nil && *opt.Size != content.Size {
		return policy, multipartFault(400, "InvalidRequest")
	}
	if opt.Type != "" && opt.Type != u.ChecksumType {
		return policy, multipartFault(400, "BadDigest")
	}
	if name == "" {
		if len(opt.Values) != 0 {
			return policy, multipartFault(400, "InvalidRequest")
		}
		return policy, nil
	}
	value := content.checksums[name]
	if u.ChecksumType == "COMPOSITE" {
		h := storagePayloadHashes()[name]
		for _, selected := range selection {
			raw, err := base64.StdEncoding.Strict().DecodeString(parts[selected.Number].Checksums[name])
			if err != nil || len(raw) != storageChecksumSizes[name] {
				return policy, multipartFault(409, "InvalidPart")
			}
			_, _ = h.Write(raw)
		}
		value = base64.StdEncoding.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(selection))
	}
	for field, expected := range opt.Values {
		if field != name || expected != value {
			return policy, multipartFault(400, "BadDigest")
		}
	}
	return storageChecksumPolicy{Algorithm: u.ChecksumAlgorithm, Type: u.ChecksumType, Value: value, Parts: len(selection)}, nil
}

func storagePolicyValues(full map[string]string, policy storageChecksumPolicy) (map[string]string, string, error) {
	if policy == (storageChecksumPolicy{}) {
		return full, "FULL_OBJECT", nil
	}
	kind := policy.Type
	name := storageChecksumAlgorithms[policy.Algorithm]
	if name == "" || kind != "FULL_OBJECT" && kind != "COMPOSITE" || kind == "COMPOSITE" && policy.Algorithm == "CRC64NVME" || policy.Parts < 1 || policy.Parts > multipartPartLimit {
		return nil, "", fmt.Errorf("invalid stored checksum policy")
	}
	value := policy.Value
	if kind == "COMPOSITE" {
		suffix := "-" + strconv.Itoa(policy.Parts)
		if !strings.HasSuffix(value, suffix) {
			return nil, "", fmt.Errorf("invalid composite part count")
		}
		value = strings.TrimSuffix(value, suffix)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(raw) != storageChecksumSizes[name] || base64.StdEncoding.EncodeToString(raw) != value || kind == "FULL_OBJECT" && full[name] != policy.Value {
		return nil, "", fmt.Errorf("invalid stored checksum value")
	}
	return map[string]string{name: policy.Value}, kind, nil
}

func storagePartChecksumXML(values map[string]string) storageAttributeChecksum {
	return storageAttributeChecksum{SHA256: values["X-Amz-Checksum-Sha256"], SHA1: values["X-Amz-Checksum-Sha1"], CRC32: values["X-Amz-Checksum-Crc32"], CRC32C: values["X-Amz-Checksum-Crc32c"], CRC64NVME: values["X-Amz-Checksum-Crc64nvme"]}
}

// Original multipart layouts can independently prove the composite. A copy has
// no original layout and retains its validated source policy instead.
func storageValidatePolicyLayout(layout []storageObjectPart, size int64, full map[string]string, policy storageChecksumPolicy) error {
	if _, _, err := storagePolicyValues(full, policy); err != nil {
		return err
	}
	if policy == (storageChecksumPolicy{}) || len(layout) == 0 {
		return nil
	}
	if len(layout) != policy.Parts {
		return fmt.Errorf("checksum layout count differs")
	}
	name := storageChecksumAlgorithms[policy.Algorithm]
	h := storagePayloadHashes()[name]
	var total int64
	for i, part := range layout {
		values, err := storageStoredChecksums(part.SHA256, part.Checksums)
		if err != nil || part.Number != i+1 || part.Size < 0 || part.Size > storagePartLimit || values[name] == "" {
			return fmt.Errorf("invalid checksum layout")
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(values[name])
		if err != nil {
			return err
		}
		_, _ = h.Write(raw)
		total += part.Size
	}
	if total != size || policy.Type == "COMPOSITE" && policy.Value != base64.StdEncoding.EncodeToString(h.Sum(nil))+"-"+strconv.Itoa(len(layout)) {
		return fmt.Errorf("checksum layout differs from publication")
	}
	return nil
}
