package s3protocol

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/signer"
)

type trailerSHA256 struct{ hash.Hash }

func (trailerSHA256) Close() {}

func TestTrailerChunksSDKChecksumAlgorithms(t *testing.T) {
	payload := bytes.Repeat([]byte("algorithm"), 8000)
	for name, factory := range map[string]func() hash.Hash{"sha256": sha256.New, "sha1": sha1.New, "crc32": func() hash.Hash { return crc32.NewIEEE() }, "crc32c": func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }} {
		for _, signed := range []bool{false, true} {
			if signed && name == "sha256" {
				continue
			}
			t.Run(fmt.Sprint(name, "/", signed), func(t *testing.T) {
				digest := factory()
				digest.Write(payload)
				r, _ := http.NewRequest("PUT", "http://192.0.2.1:30893/bucket/key", bytes.NewReader(payload))
				r.Trailer = http.Header{}
				r.Trailer.Set("X-Amz-Checksum-"+name, base64.StdEncoding.EncodeToString(digest.Sum(nil)))
				if signed {
					r = signer.StreamingSignV4(r, policyID, policySecret, "", policyRegion, int64(len(payload)), time.Now().UTC(), trailerSHA256{sha256.New()})
				} else {
					r.Header.Set("X-Amz-Content-Sha256", UnsignedTrailerPayload)
					r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(payload)))
					r.Header.Set("X-Amz-Trailer", "x-amz-checksum-"+name)
					r = signer.StreamingUnsignedV4(r, "", int64(len(payload)), time.Now().UTC())
					r = signer.SignV4(*r, policyID, policySecret, "", policyRegion)
				}
				wire, e := io.ReadAll(r.Body)
				r.Body.Close()
				if e != nil {
					t.Fatal(e)
				}
				s, e := Parse(r, policyRegion, time.Now())
				if e != nil {
					t.Fatal(e)
				}
				checkTrailerWire(t, r, s, wire, payload)
			})
		}
	}
}

func TestTrailerChunksActualSDKSignerAndTamper(t *testing.T) {
	payload := bytes.Repeat([]byte("trailer 中文"), 17000)
	sum := sha256.Sum256(payload)
	value := base64.StdEncoding.EncodeToString(sum[:])
	r, _ := http.NewRequest("PUT", "http://192.0.2.1:30893/bucket/trailer", bytes.NewReader(payload))
	r.Trailer = http.Header{"X-Amz-Checksum-Sha256": []string{value}}
	r = signer.StreamingSignV4(r, policyID, policySecret, "", policyRegion, int64(len(payload)), time.Now().UTC(), trailerSHA256{sha256.New()})
	wire, e := io.ReadAll(r.Body)
	r.Body.Close()
	if e != nil {
		t.Fatal(e)
	}
	s, e := Parse(r, policyRegion, time.Now())
	if e != nil || r.Header.Get("X-Amz-Content-Sha256") != SignedTrailerPayload {
		t.Fatal("actual SDK trailer signature unavailable", e)
	}
	checkTrailerWire(t, r, s, wire, payload)
	changedChecksum := bytes.Replace(wire, []byte(value), []byte(strings.Repeat("A", len(value))), 1)
	badByte := append([]byte{}, wire...)
	badByte[bytes.Index(badByte, []byte("\r\n"))+2] ^= 1
	for _, bad := range [][]byte{changedChecksum, badByte, wire[:len(wire)-1], wire[:len(wire)-90], append(append([]byte{}, wire...), 'x'), bytes.Replace(wire, []byte("x-amz-trailer-signature:"), []byte("x-amz-trailer-signature:0"), 1)} {
		if s.VerifyAndDecodeTo(r, bytes.NewReader(bad), io.Discard, policySecret, policyRegion, int64(len(payload))) == nil {
			t.Fatal("damaged signed trailer accepted")
		}
	}
	if s.VerifyAndDecodeTo(r, bytes.NewReader(wire), io.Discard, policySecret, policyRegion, int64(len(payload)-1)) == nil {
		t.Fatal("trailer object limit bypassed")
	}
}
func checkTrailerWire(t *testing.T, r *http.Request, s Signature, wire, payload []byte) {
	t.Helper()
	var out bytes.Buffer
	if e := s.VerifyAndDecodeTo(r, boundedStreamReader{bytes.NewReader(wire)}, &out, policySecret, policyRegion, int64(len(payload))); e != nil || !bytes.Equal(out.Bytes(), payload) {
		t.Fatal("verified trailer stream differs", e)
	}
	data, e := s.VerifyAndDecode(r, wire, policySecret, policyRegion, len(payload))
	if e != nil || !bytes.Equal(data, payload) {
		t.Fatal("small part trailer decoder differs", e)
	}
}
func TestTrailerChunksUnsignedSeedChecksumAndFraming(t *testing.T) {
	payload := []byte("unsigned trailer authenticated request")
	sum := sha256.Sum256(payload)
	value := base64.StdEncoding.EncodeToString(sum[:])
	r, _ := http.NewRequest("PUT", "http://192.0.2.1:30893/bucket/trailer", nil)
	r.Header.Set("X-Amz-Content-Sha256", UnsignedTrailerPayload)
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(payload)))
	r.Header.Set("X-Amz-Trailer", "x-amz-checksum-sha256")
	r.Header.Set("Content-Encoding", "aws-chunked")
	r = signer.SignV4(*r, policyID, policySecret, "", policyRegion)
	s, e := Parse(r, policyRegion, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	wire := []byte(fmt.Sprintf("%x\r\n%s\r\n0\r\nx-amz-checksum-sha256:%s\r\n\r\n", len(payload), payload, value))
	checkTrailerWire(t, r, s, wire, payload)
	for _, bad := range [][]byte{bytes.Replace(wire, payload, []byte(strings.Repeat("z", len(payload))), 1), bytes.Replace(wire, []byte("x-amz-checksum-sha256:"), []byte("x-amz-meta-extra:"), 1), bytes.Replace(wire, []byte("\r\n\r\n"), []byte("\r\nx-amz-checksum-sha256:"+value+"\r\n\r\n"), 1), append(append([]byte{}, wire...), 'x'), wire[:len(wire)-1]} {
		if s.VerifyAndDecodeTo(r, bytes.NewReader(bad), io.Discard, policySecret, policyRegion, int64(len(payload))) == nil {
			t.Fatal("invalid unsigned trailer accepted")
		}
	}
	changed := r.Clone(r.Context())
	changed.Header.Set("X-Amz-Decoded-Content-Length", "0")
	if s.VerifyAndDecodeTo(changed, bytes.NewReader(wire), io.Discard, policySecret, policyRegion, int64(len(payload))) == nil {
		t.Fatal("changed seed accepted")
	}
	changed = r.Clone(r.Context())
	changed.Header.Set("X-Amz-Trailer", "x-amz-meta-anything")
	if s.VerifyAndDecodeTo(changed, bytes.NewReader(wire), io.Discard, policySecret, policyRegion, int64(len(payload))) == nil {
		t.Fatal("undeclared checksum algorithm accepted")
	}
}
