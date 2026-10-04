package control

import (
	"context"
	"errors"
	"time"
)

// acquireProbeGate serializes internal observational SQL with VM suspension
// across control processes. A monitor observes runtime only after taking its
// shared lock; a suspend holds the exclusive lock through UID-checked deletion.
// This does NOT fence external Proxy connections or replace ingress admission.
// Use a dedicated pooled session; never return an uncertain session lock to it.
func (s *server) acquireProbeGate(ctx context.Context, endpoint string, shared bool) (func(), error) {
	if endpoint == "" {
		return nil, errors.New("probe gate requires endpoint identity")
	}
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	lock, unlock := "pg_try_advisory_lock", "pg_advisory_unlock"
	if shared {
		lock += "_shared"
		unlock += "_shared"
	}
	for {
		var acquired bool
		err = conn.QueryRow(ctx, "SELECT "+lock+"(hashtextextended($1, 741932))", endpoint).Scan(&acquired)
		if err != nil {
			conn.Hijack().Close(context.Background())
			return nil, err
		}
		if acquired {
			return func() {
				release, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var released bool
				if conn.QueryRow(release, "SELECT "+unlock+"(hashtextextended($1, 741932))", endpoint).Scan(&released) != nil || !released {
					conn.Hijack().Close(release)
				} else {
					conn.Release()
				}
			}, nil
		}
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
