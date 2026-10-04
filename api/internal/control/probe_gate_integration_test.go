package control

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
	"time"
)

func TestEndpointProbeGateAcrossIndependentPools(t *testing.T) {
	url := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	first, second := &server{db: a}, &server{db: b}
	endpoint := newID("ep_probe_gate_")
	releaseA, err := first.acquireProbeGate(ctx, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	releaseB, err := second.acquireProbeGate(ctx, endpoint, true)
	if err != nil {
		releaseA()
		t.Fatal(err)
	}
	blocked, cancelBlocked := context.WithTimeout(ctx, 150*time.Millisecond)
	if release, err := second.acquireProbeGate(blocked, endpoint, false); err == nil {
		release()
		releaseB()
		releaseA()
		cancelBlocked()
		t.Fatal("suspend bypassed a live internal probe")
	}
	cancelBlocked()
	releaseB()
	releaseA()
	exclusive, err := second.acquireProbeGate(ctx, endpoint, false)
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancelBlocked = context.WithTimeout(ctx, 150*time.Millisecond)
	if release, err := first.acquireProbeGate(blocked, endpoint, true); err == nil {
		release()
		exclusive()
		cancelBlocked()
		t.Fatal("monitor bypassed an in-progress suspend")
	}
	cancelBlocked()
	unrelated, err := first.acquireProbeGate(ctx, newID("ep_probe_other_"), true)
	if err != nil {
		exclusive()
		t.Fatal(err)
	}
	unrelated()
	exclusive()
	// Cancellation and release must not leave session locks in either pool.
	again, err := first.acquireProbeGate(ctx, endpoint, false)
	if err != nil {
		t.Fatal(err)
	}
	again()
	t.Log("two independent pools: parallel monitors, suspend exclusion, no cross-endpoint blocking, cancellation and pool reuse passed")
}
