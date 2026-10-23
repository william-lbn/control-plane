package control

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// This snapshot is written in the same transaction as object publication.
// Upload receipts may outlive an overwrite and must never reconstruct it.
type storageObjectPart struct {
	Number int    `json:"part_number"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type storageAttributePart struct {
	Number int    `xml:"PartNumber"`
	Size   int64  `xml:"Size"`
	SHA256 string `xml:"ChecksumSHA256"`
}
type storageAttributeParts struct {
	Count     int                    `xml:"PartsCount"`
	Marker    int                    `xml:"PartNumberMarker"`
	Next      int                    `xml:"NextPartNumberMarker,omitempty"`
	Max       int                    `xml:"MaxParts"`
	Truncated bool                   `xml:"IsTruncated"`
	Parts     []storageAttributePart `xml:"Part"`
}

func storageAttributePagination(h http.Header) (max, marker int, err error) {
	max = 1000
	for _, field := range []struct {
		name      string
		target    *int
		low, high int
	}{
		{"X-Amz-Max-Parts", &max, 1, 1000}, {"X-Amz-Part-Number-Marker", &marker, 0, 10000},
	} {
		values := h.Values(field.name)
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 || values[0] == "" || strings.IndexFunc(values[0], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, 0, fmt.Errorf("invalid part pagination")
		}
		n, e := strconv.Atoi(values[0])
		if e != nil || n < field.low || n > field.high {
			return 0, 0, fmt.Errorf("invalid part pagination")
		}
		*field.target = n
	}
	return max, marker, nil
}
func storagePaginateObjectParts(layout []storageObjectPart, size int64, max, marker int) (*storageAttributeParts, error) {
	if len(layout) == 0 {
		return nil, nil
	}
	if len(layout) > multipartPartLimit || max < 1 || max > 1000 || marker < 0 || marker > 10000 {
		return nil, fmt.Errorf("invalid stored layout")
	}
	var total int64
	previous := 0
	out := &storageAttributeParts{Count: len(layout), Marker: marker, Max: max}
	for _, part := range layout {
		if part.Number <= previous || part.Number > 10000 || part.Size < 0 || part.Size > storagePartLimit || !storageSHA256.MatchString(part.SHA256) {
			return nil, fmt.Errorf("invalid stored layout")
		}
		previous = part.Number
		total += part.Size
		if part.Number <= marker {
			continue
		}
		if len(out.Parts) == max {
			out.Truncated = true
			continue
		}
		raw, _ := hex.DecodeString(part.SHA256)
		out.Parts = append(out.Parts, storageAttributePart{part.Number, part.Size, base64.StdEncoding.EncodeToString(raw)})
	}
	if total != size {
		return nil, fmt.Errorf("stored layout size mismatch")
	}
	if out.Truncated {
		out.Next = out.Parts[len(out.Parts)-1].Number
	}
	return out, nil
}
