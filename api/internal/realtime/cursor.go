package realtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"time"
)

var ErrInvalidCursor = errors.New("realtime cursor differs from the authorized subscription")

type CursorScope struct {
	Source           SourceKey
	Query            string
	QueryVersion     int64
	Principal        string
	Authorization    int64
	ParametersSHA256 [sha256.Size]byte
}

type CursorSigner struct {
	Key []byte
}

const (
	cursorPayloadBytes = 16
	cursorBytes        = cursorPayloadBytes + sha256.Size
)

func (s CursorSigner) mac(scope CursorScope, data []byte) ([]byte, error) {
	if len(s.Key) < sha256.Size || scope.Source.ID == "" || scope.Source.Project == "" || scope.Source.Branch == "" ||
		scope.Source.Timeline == "" || scope.Source.Database == "" || scope.Query == "" ||
		scope.QueryVersion <= 0 || scope.Principal == "" || scope.Authorization <= 0 {
		return nil, ErrInvalidCursor
	}
	bound, err := json.Marshal(scope)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	mac := hmac.New(sha256.New, s.Key)
	_, _ = mac.Write([]byte("neon-realtime-cursor-v1\x00"))
	_, _ = mac.Write(bound)
	_, _ = mac.Write(data)
	return mac.Sum(nil), nil
}

func (s CursorSigner) Sign(scope CursorScope, pos int64, expires time.Time) (string, error) {
	if pos < 0 || expires.Unix() <= 0 {
		return "", ErrInvalidCursor
	}
	payload := make([]byte, cursorPayloadBytes, cursorBytes)
	binary.BigEndian.PutUint64(payload[:8], uint64(pos))
	binary.BigEndian.PutUint64(payload[8:], uint64(expires.Unix()))
	mac, err := s.mac(scope, payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(payload, mac...)), nil
}

func (s CursorSigner) Verify(scope CursorScope, token string, now time.Time) (int64, error) {
	if len(token) != base64.RawURLEncoding.EncodedLen(cursorBytes) {
		return 0, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != cursorBytes {
		return 0, ErrInvalidCursor
	}
	mac, err := s.mac(scope, raw[:cursorPayloadBytes])
	if err != nil || !hmac.Equal(mac, raw[cursorPayloadBytes:]) {
		return 0, ErrInvalidCursor
	}
	pos, expires := binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:cursorPayloadBytes])
	if pos > math.MaxInt64 || expires > math.MaxInt64 {
		return 0, ErrInvalidCursor
	}
	if int64(expires) <= now.Unix() {
		return 0, ErrCursorExpired
	}
	return int64(pos), nil
}
