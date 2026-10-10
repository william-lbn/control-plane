package functions

import (
	"bytes"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func signedManager(t *testing.T, now time.Time) (*ManagerAuthenticator, []byte) {
	t.Helper()
	key := bytes.Repeat([]byte{42}, 32)
	a, err := NewManagerAuthenticator(key)
	if err != nil {
		t.Fatal(err)
	}
	a.clock = func() time.Time { return now }
	return a, key
}

func TestManagerReplayAdmitsExactlyOneConcurrentRequest(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a, key := signedManager(t, now)
	r := httptest.NewRequest("POST", "http://manager/internal/invoke?generation=7", bytes.NewReader([]byte("body")))
	if err := SignManagerRequest(r, []byte("body"), key, now); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.Verify(r, []byte("body")) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d copies", accepted.Load())
	}
}

func TestManagerRejectsTamperingAndExpiredRequests(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, kind := range []string{"body", "method", "path", "query", "key", "past", "future", "authorization", "missing_scheme"} {
		t.Run(kind, func(t *testing.T) {
			a, key := signedManager(t, now)
			r := httptest.NewRequest("POST", "http://manager/internal/invoke?generation=7", nil)
			stamp := now
			if kind == "past" {
				stamp = now.Add(-31 * time.Second)
			}
			if kind == "future" {
				stamp = now.Add(31 * time.Second)
			}
			if kind == "key" {
				key = bytes.Repeat([]byte{43}, 32)
			}
			if err := SignManagerRequest(r, []byte("body"), key, stamp); err != nil {
				t.Fatal(err)
			}
			body := []byte("body")
			switch kind {
			case "body":
				body = []byte("other")
			case "method":
				r.Method = "DELETE"
			case "path":
				r.URL.Path = "/internal/shutdown"
			case "query":
				r.URL.RawQuery = "generation=8"
			case "authorization":
				r.Header.Del("Authorization")
			case "missing_scheme":
				r.Header.Set("Authorization", r.Header.Get("Authorization")[len("NeonManager "):])
			}
			if a.Verify(r, body) == nil {
				t.Fatal("invalid manager request accepted")
			}
		})
	}
}

func TestManagerFutureNonceCannotReplayAfterEarlyCacheExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a, key := signedManager(t, now)
	r := httptest.NewRequest("POST", "http://manager/internal/shutdown", nil)
	if err := SignManagerRequest(r, nil, key, now.Add(29*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(r, nil); err != nil {
		t.Fatal(err)
	}
	a.clock = func() time.Time { return now.Add(40 * time.Second) }
	if a.Verify(r, nil) == nil {
		t.Fatal("future timestamp opened a replay window")
	}
}

func TestManagerNonceCapacityFailsClosed(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a, key := signedManager(t, now)
	for i := 0; i < maxManagerNonces; i++ {
		a.used[string(rune(i))] = now.Add(time.Minute)
	}
	r := httptest.NewRequest("POST", "http://manager/internal/status", nil)
	if err := SignManagerRequest(r, nil, key, now); err != nil {
		t.Fatal(err)
	}
	if a.Verify(r, nil) == nil {
		t.Fatal("full nonce cache evicted a live nonce")
	}
}
