package functions

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Actual TLS, HMAC and nonce verification on every request. A transient read
// retry must never reuse an already consumed authentication nonce.
func TestArtifactFetchRetriesOnlyTransientTrustedGET(t *testing.T) {
	for _, first := range []int{429, 502, 503, 504, 401, 404, 302} {
		t.Run(http.StatusText(first), func(t *testing.T) {
			b, archive, _ := artifactFixture(t)
			key, _ := secretKey(b.ArtifactKey)
			auth, _ := NewManagerAuthenticator(key)
			var calls atomic.Int32
			peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if auth.Verify(r, nil) != nil {
					t.Error("fresh signed attempt required")
					w.WriteHeader(401)
					return
				}
				if calls.Add(1) == 1 {
					w.WriteHeader(first)
					return
				}
				_, _ = w.Write(archive)
			}))
			defer peer.Close()
			b.ArtifactURL, b.ArtifactCA = peer.URL+b.artifactPath(), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))
			actual, err := b.FetchBundle(context.Background())
			transient := first == 429 || first == 502 || first == 503 || first == 504
			if transient {
				if err != nil || calls.Load() != 2 || !bytes.Equal(actual, archive) {
					t.Fatal("trusted retry failed", err, calls.Load())
				}
			} else {
				var failure *ArtifactFetchError
				if !errors.As(err, &failure) || failure.Status != first || failure.Attempts != 1 || calls.Load() != 1 {
					t.Fatal("permanent rejection retried or unsafe error", err)
				}
			}
		})
	}
}

func TestArtifactFetchRetryBudgetAndCancellation(t *testing.T) {
	b, _, _ := artifactFixture(t)
	var calls atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer peer.Close()
	b.ArtifactURL, b.ArtifactCA = peer.URL+b.artifactPath(), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))
	_, err := b.FetchBundle(context.Background())
	var failure *ArtifactFetchError
	if !errors.As(err, &failure) || failure.Attempts != 3 || failure.Status != 503 || calls.Load() != 3 {
		t.Fatal("unbounded retries", err, calls.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = b.FetchBundle(ctx)
	if !errors.As(err, &failure) || failure.Reason != "cancelled" || calls.Load() != 4 {
		t.Fatal("cancelled retry continued", err)
	}
}
