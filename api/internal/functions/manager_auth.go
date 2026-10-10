package functions

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const managerWindow = 30 * time.Second
const maxManagerNonces = 4096

// ManagerAuthenticator belongs to the privileged guest supervisor, never to
// the untrusted Node child. A manager key is unique to an immutable instance.
type ManagerAuthenticator struct {
	key   []byte
	mu    sync.Mutex
	used  map[string]time.Time
	clock func() time.Time
}

func NewManagerAuthenticator(key []byte) (*ManagerAuthenticator, error) {
	if len(key) < 32 || len(key) > 128 {
		return nil, errors.New("manager key must contain 32 to 128 bytes")
	}
	return &ManagerAuthenticator{key: append([]byte(nil), key...), used: make(map[string]time.Time), clock: time.Now}, nil
}

func managerMAC(key []byte, method, target, digest, stamp, nonce string) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "functions-manager-v1\n%s\n%s\n%s\n%s\n%s", method, target, digest, stamp, nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignManagerRequest signs the exact escaped path and query used on the wire.
// Bodies are buffered and bounded before calling this helper, so neither side
// may reinterpret a signed body as an arbitrary destination or command.
func SignManagerRequest(request *http.Request, body, key []byte, now time.Time) error {
	if len(key) < 32 || len(key) > 128 {
		return errors.New("invalid manager key")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	nonce := hex.EncodeToString(random[:])
	stamp := strconv.FormatInt(now.Unix(), 10)
	digest := sha256.Sum256(body)
	bodyDigest := hex.EncodeToString(digest[:])
	request.Header.Set("X-Neon-Manager-Time", stamp)
	request.Header.Set("X-Neon-Manager-Nonce", nonce)
	request.Header.Set("X-Neon-Manager-Digest", bodyDigest)
	request.Header.Set("Authorization", "NeonManager "+managerMAC(key, request.Method, request.URL.RequestURI(), bodyDigest, stamp, nonce))
	return nil
}

// Verify consumes a nonce only after validating every signed field. The cache
// fails closed at capacity rather than evicting a still-valid nonce and opening
// a replay window. Concurrent copies of a request admit exactly one call.
func (a *ManagerAuthenticator) Verify(request *http.Request, body []byte) error {
	stamp, nonce, bodyDigest := request.Header.Get("X-Neon-Manager-Time"), request.Header.Get("X-Neon-Manager-Nonce"), request.Header.Get("X-Neon-Manager-Digest")
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != stamp || len(nonce) != 32 || len(bodyDigest) != 64 {
		return errors.New("manager authentication rejected")
	}
	if _, err = hex.DecodeString(nonce); err != nil {
		return errors.New("manager authentication rejected")
	}
	now := a.clock()
	issued := time.Unix(seconds, 0)
	if issued.Before(now.Add(-managerWindow)) || issued.After(now.Add(managerWindow)) {
		return errors.New("manager authentication rejected")
	}
	digest := sha256.Sum256(body)
	if subtle.ConstantTimeCompare([]byte(bodyDigest), []byte(hex.EncodeToString(digest[:]))) != 1 {
		return errors.New("manager authentication rejected")
	}
	if !strings.HasPrefix(request.Header.Get("Authorization"), "NeonManager ") {
		return errors.New("manager authentication rejected")
	}
	actual := strings.TrimPrefix(request.Header.Get("Authorization"), "NeonManager ")
	expected := managerMAC(a.key, request.Method, request.URL.RequestURI(), bodyDigest, stamp, nonce)
	if len(actual) != len(expected) || !hmac.Equal([]byte(actual), []byte(expected)) {
		return errors.New("manager authentication rejected")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, until := range a.used {
		if !now.Before(until) {
			delete(a.used, key)
		}
	}
	if _, seen := a.used[nonce]; seen || len(a.used) >= maxManagerNonces {
		return errors.New("manager authentication rejected")
	}
	// A future-dated signed request remains valid until its timestamp + window;
	// storing only now + window would allow replay near that later boundary.
	a.used[nonce] = issued.Add(managerWindow + time.Second)
	return nil
}
