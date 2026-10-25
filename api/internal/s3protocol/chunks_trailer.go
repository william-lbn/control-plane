package s3protocol

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"hash/crc32"

	"github.com/minio/crc64nvme"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const SignedTrailerPayload = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
const UnsignedTrailerPayload = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"

func isTrailerPayload(value string) bool {
	return value == SignedTrailerPayload || value == UnsignedTrailerPayload
}

// Trailers remain inside the authenticated aws-chunked entity, not Go's outer
// HTTP transport trailers. Bytes are only written to an uncommitted sink.
// https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sigv4-streaming-trailers.html
func (s Signature) decodeTrailerChunksTo(r *http.Request, source io.Reader, destination io.Writer, secret, region string, limit int64) error {
	marker := r.Header.Get("X-Amz-Content-Sha256")
	signed := marker == SignedTrailerPayload
	encoding := r.Header.Get("Content-Encoding")
	if !isTrailerPayload(marker) || limit < 0 || s.Presigned || r.Method != http.MethodPut || len(s.Date) != 16 || len(secret) < 32 || len(secret) > 256 || !keyID.MatchString(s.KeyID) || !digestPattern.MatchString(s.Value) || (encoding != "" && encoding != "aws-chunked") {
		return ErrSignature
	}
	if len(r.Header.Values("X-Amz-Trailer")) != 1 {
		return ErrSignature
	}
	name := r.Header.Get("X-Amz-Trailer")
	var checksum hash.Hash
	switch name {
	case "x-amz-checksum-sha256":
		checksum = sha256.New()
	case "x-amz-checksum-sha1":
		checksum = sha1.New()
	case "x-amz-checksum-crc32":
		checksum = crc32.NewIEEE()
	case "x-amz-checksum-crc32c":
		checksum = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "x-amz-checksum-crc64nvme":
		checksum = crc64nvme.New()
	default:
		return ErrSignature
	}
	// A header and a trailer must never compete for the same checksum.
	if len(r.Header.Values(name)) != 0 {
		return ErrSignature
	}
	length := r.Header.Get("X-Amz-Decoded-Content-Length")
	declared, e := strconv.ParseUint(length, 10, 32)
	if e != nil || length != strconv.FormatUint(declared, 10) || declared > uint64(limit) || s.verifyCanonical(r, marker, secret, region) != nil {
		return ErrSignature
	}
	wire := &io.LimitedReader{R: source, N: limit + (1 << 20) + 1}
	reader := bufio.NewReaderSize(wire, 256)
	key := s.signingKey(secret, region)
	previous := s.Value
	written := int64(0)
	buffer := make([]byte, 64<<10)
	line := func(requireCRLF bool) (string, bool, bool) {
		raw, e := reader.ReadSlice('\n')
		if e != nil || len(raw) > 130 || len(raw) < 1 {
			return "", false, false
		}
		crlf := len(raw) >= 2 && raw[len(raw)-2] == '\r'
		if requireCRLF && !crlf {
			return "", false, false
		}
		raw = raw[:len(raw)-1]
		if crlf {
			raw = raw[:len(raw)-1]
		}
		if strings.ContainsAny(string(raw), "\r\n\x00") {
			return "", false, false
		}
		return string(raw), crlf, true
	}
	for count := 0; count < 4096; count++ {
		header, _, ok := line(true)
		if !ok {
			return ErrSignature
		}
		size, signature, hasSignature := strings.Cut(header, ";chunk-signature=")
		if signed {
			if !hasSignature || !digestPattern.MatchString(signature) {
				return ErrSignature
			}
		} else if hasSignature || strings.ContainsRune(size, ';') {
			return ErrSignature
		}
		n, e := strconv.ParseUint(size, 16, 32)
		if e != nil || size == "" || n > uint64(limit-written) {
			return ErrSignature
		}
		digest := sha256.New()
		actual, e := io.CopyBuffer(io.MultiWriter(destination, digest, checksum), io.LimitReader(reader, int64(n)), buffer)
		if e != nil {
			return e
		}
		if actual != int64(n) {
			return ErrSignature
		}
		if n > 0 {
			var ending [2]byte
			if _, e = io.ReadFull(reader, ending[:]); e != nil || ending != [2]byte{'\r', '\n'} {
				return ErrSignature
			}
		}
		if signed {
			expected := hex.EncodeToString(mac(key, "AWS4-HMAC-SHA256-PAYLOAD\n"+s.Date+"\n"+s.Scope+"\n"+previous+"\n"+Hash(nil)+"\n"+hex.EncodeToString(digest.Sum(nil))))
			if !hmac.Equal([]byte(expected), []byte(signature)) {
				return ErrSignature
			}
			previous = signature
		}
		written += actual
		if n != 0 {
			continue
		}
		if uint64(written) != declared {
			return ErrSignature
		}
		trailer, crlf, ok := line(false)
		if !ok {
			return ErrSignature
		}
		if trailer == "" {
			trailer, crlf, ok = line(false)
			if !ok {
				return ErrSignature
			}
		}
		actualName, value, found := strings.Cut(trailer, ":")
		expected := base64.StdEncoding.EncodeToString(checksum.Sum(nil))
		if !found || actualName != name || !hmac.Equal([]byte(value), []byte(expected)) {
			return ErrSignature
		}
		// MinIO's signed trailer canonical line ends in LF, followed by a CRLF
		// separator. Other clients put CRLF directly after the checksum line.
		if !crlf {
			empty, _, ok := line(true)
			if !ok || empty != "" {
				return ErrSignature
			}
		}
		if signed {
			trailing, _, ok := line(true)
			trailerName, trailerSignature, found := strings.Cut(trailing, ":")
			if !ok || !found || trailerName != "x-amz-trailer-signature" || !digestPattern.MatchString(trailerSignature) {
				return ErrSignature
			}
			expectedSignature := hex.EncodeToString(mac(key, "AWS4-HMAC-SHA256-TRAILER\n"+s.Date+"\n"+s.Scope+"\n"+previous+"\n"+Hash([]byte(name+":"+value+"\n"))))
			if !hmac.Equal([]byte(expectedSignature), []byte(trailerSignature)) {
				return ErrSignature
			}
		}
		empty, _, ok := line(true)
		if !ok || empty != "" {
			return ErrSignature
		}
		if _, e = reader.ReadByte(); e != io.EOF || wire.N == 0 {
			return ErrSignature
		}
		// Only a verified trailer is made available to the checksum admission layer.
		if r.Trailer == nil {
			r.Trailer = http.Header{}
		}
		r.Trailer.Set(name, value)
		return nil
	}
	return ErrSignature
}
