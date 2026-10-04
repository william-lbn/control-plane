package control

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIdleDrainPreservesEveryActivityGuard(t *testing.T) {
	for index := 0; index < 4; index++ {
		ctx := context.Background()
		read := func(context.Context) (sqlResult, error) {
			counts := []any{int64(0), int64(0), int64(0), int64(0)}
			counts[index] = int64(1)
			return sqlResult{Rows: [][]any{counts}}, nil
		}
		var busy computeBusyError
		if err := awaitComputeIdle(ctx, 5*time.Millisecond, time.Millisecond, read); !errors.As(err, &busy) {
			t.Fatalf("activity guard %d did not block suspend", index)
		}
	}
}

func TestIdleDrainWaitsForNormalClosure(t *testing.T) {
	calls := 0
	read := func(context.Context) (sqlResult, error) {
		calls++
		count := int64(1)
		if calls >= 2 {
			count = 0
		}
		return sqlResult{Rows: [][]any{{count, int64(0), int64(0), int64(0)}}}, nil
	}
	if err := awaitComputeIdle(context.Background(), time.Second, time.Millisecond, read); err != nil || calls != 2 {
		t.Fatal("normal closure did not admit suspension")
	}
}

func TestIdleDrainFailsClosedOnUnavailableEvidence(t *testing.T) {
	for _, read := range []func(context.Context) (sqlResult, error){
		func(context.Context) (sqlResult, error) { return sqlResult{}, errors.New("unavailable") },
		func(context.Context) (sqlResult, error) { return sqlResult{Rows: [][]any{{0, 0, 0}}}, nil },
	} {
		if err := awaitComputeIdle(context.Background(), time.Second, time.Millisecond, read); err == nil {
			t.Fatal("unavailable or incomplete SQL evidence admitted suspension")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := awaitComputeIdle(ctx, time.Second, time.Millisecond, func(context.Context) (sqlResult, error) {
		return sqlResult{Rows: [][]any{{1, 0, 0, 0}}}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled suspension did not stop draining")
	}
}
