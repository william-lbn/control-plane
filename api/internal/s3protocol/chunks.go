package s3protocol

import (
	"bytes"
	"crypto/hmac"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

const SignedChunkPayload = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"

// VerifyAndDecode authenticates the seed, each chained chunk, and the terminal
// zero chunk before returning bytes to the SQL/blob Driver. The bounded preview
// accepts signed chunks without trailers; it never accepts unsigned writes.
// AWS algorithm: https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html
func (s Signature) VerifyAndDecode(r *http.Request, wire []byte, secret, region string, limit int) ([]byte, error) {
	if limit < 0 || len(wire) > limit+(1<<20) {
		return nil, ErrSignature
	}
	if isTrailerPayload(r.Header.Get("X-Amz-Content-Sha256")) {
		var decoded bytes.Buffer
		if err := s.decodeTrailerChunksTo(r, bytes.NewReader(wire), &decoded, secret, region, int64(limit)); err != nil {
			return nil, err
		}
		return decoded.Bytes(), nil
	}
	if r.Header.Get("X-Amz-Content-Sha256") != SignedChunkPayload {
		if len(wire) > limit || s.Verify(r, wire, secret, region) != nil {
			return nil, ErrSignature
		}
		return wire, nil
	}
	encoding := r.Header.Get("Content-Encoding")
	// minio-go signs the streaming marker and decoded length without setting
	// Content-Encoding. AWS clients may additionally send aws-chunked. The
	// signature chain, not an optional transport header, authenticates bytes.
	if s.Presigned || r.Method != http.MethodPut || len(s.Date) != 16 || len(secret) < 32 || len(secret) > 256 || !keyID.MatchString(s.KeyID) || !digestPattern.MatchString(s.Value) || (encoding != "" && encoding != "aws-chunked") || r.Header.Get("X-Amz-Trailer") != "" {
		return nil, ErrSignature
	}
	length := r.Header.Get("X-Amz-Decoded-Content-Length")
	n, err := strconv.ParseUint(length, 10, 32)
	if err != nil || length != strconv.FormatUint(n, 10) || n > uint64(limit) || s.verifyCanonical(r, SignedChunkPayload, secret, region) != nil {
		return nil, ErrSignature
	}
	// Parse already requires every X-Amz control header to be signed. The
	// declared decoded length cannot be altered independently of the seed.
	key := s.signingKey(secret, region)
	previous := s.Value
	out := make([]byte, 0, int(n))
	for count := 0; count < 4096; count++ {
		end := bytes.Index(wire, []byte("\r\n"))
		if end < 0 || end > 128 {
			return nil, ErrSignature
		}
		size, signature, ok := strings.Cut(string(wire[:end]), ";chunk-signature=")
		chunkLength, err := strconv.ParseUint(size, 16, 32)
		if !ok || err != nil || size == "" || !digestPattern.MatchString(signature) || chunkLength > uint64(limit-len(out)) {
			return nil, ErrSignature
		}
		wire = wire[end+2:]
		if chunkLength+2 > uint64(len(wire)) || !bytes.Equal(wire[int(chunkLength):int(chunkLength)+2], []byte("\r\n")) {
			return nil, ErrSignature
		}
		chunk := wire[:int(chunkLength)]
		toSign := "AWS4-HMAC-SHA256-PAYLOAD\n" + s.Date + "\n" + s.Scope + "\n" + previous + "\n" + Hash(nil) + "\n" + Hash(chunk)
		expected := hex.EncodeToString(mac(key, toSign))
		if !hmac.Equal([]byte(expected), []byte(signature)) {
			return nil, ErrSignature
		}
		wire = wire[int(chunkLength)+2:]
		out = append(out, chunk...)
		previous = signature
		if chunkLength == 0 {
			if len(wire) != 0 || uint64(len(out)) != n {
				return nil, ErrSignature
			}
			return out, nil
		}
	}
	return nil, ErrSignature
}
