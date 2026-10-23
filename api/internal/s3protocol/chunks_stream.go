package s3protocol

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// VerifyAndDecodeTo writes only to a caller's uncommitted private buffer. A
// failure invalidates every byte already written; callers must discard it.
// The seed, body hash or all chained chunks, terminal signature and EOF must
// succeed before the caller publishes any SQL object reference.
func (s Signature) VerifyAndDecodeTo(r *http.Request, source io.Reader, destination io.Writer, secret, region string, limit int64) error {
	if isTrailerPayload(r.Header.Get("X-Amz-Content-Sha256")) {
		return s.decodeTrailerChunksTo(r, source, destination, secret, region, limit)
	}
	if limit < 0 || r.Method != http.MethodPut || len(s.Date) != 16 || len(secret) < 32 || len(secret) > 256 || !keyID.MatchString(s.KeyID) || !digestPattern.MatchString(s.Value) || r.Header.Get("X-Amz-Trailer") != "" {
		return ErrSignature
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == SignedChunkPayload {
		return s.decodeSignedChunksTo(r, source, destination, secret, region, limit)
	}
	if s.Presigned {
		payload = "UNSIGNED-PAYLOAD"
	} else if !digestPattern.MatchString(payload) {
		return ErrSignature
	}
	if s.verifyCanonical(r, payload, secret, region) != nil {
		return ErrSignature
	}
	digest := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(destination, digest), io.LimitReader(source, limit+1), make([]byte, 64<<10))
	if err != nil {
		return err
	}
	if n > limit {
		return ErrSignature
	}
	if payload != "UNSIGNED-PAYLOAD" && !hmac.Equal([]byte(payload), []byte(hex.EncodeToString(digest.Sum(nil)))) {
		return ErrSignature
	}
	return nil
}

func (s Signature) decodeSignedChunksTo(r *http.Request, source io.Reader, destination io.Writer, secret, region string, limit int64) error {
	encoding := r.Header.Get("Content-Encoding")
	if s.Presigned || encoding != "" && encoding != "aws-chunked" {
		return ErrSignature
	}
	length := r.Header.Get("X-Amz-Decoded-Content-Length")
	declared, err := strconv.ParseUint(length, 10, 32)
	if err != nil || length != strconv.FormatUint(declared, 10) || declared > uint64(limit) || s.verifyCanonical(r, SignedChunkPayload, secret, region) != nil {
		return ErrSignature
	}
	wire := &io.LimitedReader{R: source, N: limit + (1 << 20) + 1}
	reader := bufio.NewReaderSize(wire, 256)
	key := s.signingKey(secret, region)
	previous := s.Value
	written := int64(0)
	buffer := make([]byte, 64<<10)
	for count := 0; count < 4096; count++ {
		line, e := reader.ReadSlice('\n')
		if e != nil || len(line) < 2 || len(line) > 130 || line[len(line)-2] != '\r' {
			return ErrSignature
		}
		size, signature, ok := strings.Cut(string(line[:len(line)-2]), ";chunk-signature=")
		chunkLength, e := strconv.ParseUint(size, 16, 32)
		if !ok || e != nil || size == "" || !digestPattern.MatchString(signature) || chunkLength > uint64(limit-written) {
			return ErrSignature
		}
		digest := sha256.New()
		n, e := io.CopyBuffer(io.MultiWriter(destination, digest), io.LimitReader(reader, int64(chunkLength)), buffer)
		if e != nil {
			return e
		}
		if n != int64(chunkLength) {
			return ErrSignature
		}
		var ending [2]byte
		if _, e = io.ReadFull(reader, ending[:]); e != nil || ending != [2]byte{'\r', '\n'} {
			return ErrSignature
		}
		toSign := "AWS4-HMAC-SHA256-PAYLOAD\n" + s.Date + "\n" + s.Scope + "\n" + previous + "\n" + Hash(nil) + "\n" + hex.EncodeToString(digest.Sum(nil))
		expected := hex.EncodeToString(mac(key, toSign))
		if !hmac.Equal([]byte(expected), []byte(signature)) {
			return ErrSignature
		}
		previous = signature
		written += n
		if chunkLength == 0 {
			if _, e = reader.ReadByte(); e != io.EOF || wire.N == 0 || uint64(written) != declared {
				return ErrSignature
			}
			return nil
		}
	}
	return ErrSignature
}
