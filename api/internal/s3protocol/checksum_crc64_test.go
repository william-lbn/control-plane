package s3protocol

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/signer"
)

func TestTrailerCRC64NVMEActualSignerAndTamper(t *testing.T) {
	payload := []byte("123456789")
	value := base64.StdEncoding.EncodeToString([]byte{0xae, 0x8b, 0x14, 0x86, 0x0a, 0x79, 0x98, 0x88})
	r, _ := http.NewRequest("PUT", "http://192.0.2.1:30893/bucket/crc64", nil)
	r.Header.Set("X-Amz-Content-Sha256", UnsignedTrailerPayload)
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(payload)))
	r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc64nvme")
	r.Header.Set("Content-Encoding", "aws-chunked")
	r = signer.SignV4(*r, policyID, policySecret, "", policyRegion)
	s, err := Parse(r, policyRegion, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wire := []byte(fmt.Sprintf("%x\r\n%s\r\n0\r\nx-amz-checksum-crc64nvme:%s\r\n\r\n", len(payload), payload, value))
	checkTrailerWire(t, r, s, wire, payload)
	for _, bad := range [][]byte{
		bytes.Replace(wire, payload, []byte(strings.Repeat("z", len(payload))), 1),
		bytes.Replace(wire, []byte(value), []byte("AAAAAAAAAAA="), 1),
		append(append([]byte{}, wire...), 'x'),
		wire[:len(wire)-1],
	} {
		if s.VerifyAndDecodeTo(r, bytes.NewReader(bad), io.Discard, policySecret, policyRegion, int64(len(payload))) == nil {
			t.Fatal("tampered CRC64 trailer accepted")
		}
	}
}
