package realtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type SSEOptions struct {
	Cursor    CursorSigner
	Scope     CursorScope
	ExpiresAt time.Time
	CursorTTL time.Duration
	Heartbeat time.Duration
}

// ServeSSE writes one event stream for an already authenticated principal.
// Authorize rechecks access to the frozen query before each delivery.
func (s Store) ServeSSE(w http.ResponseWriter, r *http.Request, notifications *Notifications,
	sub Subscription, options SSEOptions, authorize func(context.Context) error,
	openWriter func(context.Context) (*pgx.Conn, error)) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method is not supported", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now()
	if sub.Key != options.Scope.Source || options.CursorTTL < time.Second ||
		options.Heartbeat <= 0 || sub.DeliveryLimit <= 0 || authorize == nil || !now.Before(options.ExpiresAt) {
		http.Error(w, "subscription unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := options.Cursor.mac(options.Scope, nil); err != nil {
		http.Error(w, "subscription unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), options.ExpiresAt)
	defer cancel()
	if err := authorize(ctx); err != nil {
		http.Error(w, "subscription unavailable", http.StatusForbidden)
		return
	}
	if _, err := s.Head(ctx, sub.Key); err != nil {
		http.Error(w, "subscription unavailable", http.StatusServiceUnavailable)
		return
	}
	previous := r.Header.Values("Last-Event-ID")
	switch len(previous) {
	case 0:
	case 1:
		position, err := options.Cursor.Verify(options.Scope, previous[0], now)
		switch {
		case err == nil:
			sub.After = &position
		case errors.Is(err, ErrCursorExpired):
			sub.After = nil
		default:
			http.Error(w, "invalid subscription cursor", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "invalid subscription cursor", http.StatusBadRequest)
		return
	}

	type outgoing struct {
		ctx      context.Context
		delivery Delivery
		ack      chan error
	}
	events := make(chan outgoing)
	finished := make(chan error, 1)
	go func() {
		finished <- s.Subscribe(ctx, notifications, sub, authorize, openWriter,
			func(call context.Context, delivery Delivery) error {
				item := outgoing{ctx: call, delivery: delivery, ack: make(chan error, 1)}
				select {
				case events <- item:
				case <-call.Done():
					return call.Err()
				}
				select {
				case err := <-item.ack:
					return err
				case <-call.Done():
					return call.Err()
				}
			})
	}()

	controller := http.NewResponseController(w)
	started := false
	write := func(call context.Context, data string) error {
		if err := call.Err(); err != nil {
			return err
		}
		deadline, _ := call.Deadline()
		if err := controller.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer controller.SetWriteDeadline(time.Time{})
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			started = true
		}
		if _, err := fmt.Fprint(w, data); err != nil {
			return err
		}
		return controller.Flush()
	}
	timer := time.NewTicker(options.Heartbeat)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			if !started && r.Context().Err() == nil {
				http.Error(w, "subscription unavailable", http.StatusServiceUnavailable)
			}
			return
		case err := <-finished:
			if !started && ctx.Err() == nil {
				status := http.StatusServiceUnavailable
				if errors.Is(err, ErrSubscriptionCapacity) {
					status = http.StatusTooManyRequests
				}
				http.Error(w, "subscription unavailable", status)
			}
			return
		case <-timer.C:
			if err := authorize(ctx); err != nil {
				if !started {
					http.Error(w, "subscription unavailable", http.StatusForbidden)
				}
				return
			}
			heartbeat, finish := context.WithTimeout(ctx, sub.DeliveryLimit)
			err := write(heartbeat, ": keepalive\n\n")
			finish()
			if err != nil {
				return
			}
		case item := <-events:
			err := authorize(item.ctx)
			if err == nil {
				expiration := time.Now().Add(options.CursorTTL)
				if expiration.After(options.ExpiresAt) {
					expiration = options.ExpiresAt
				}
				var cursor string
				cursor, err = options.Cursor.Sign(options.Scope, item.delivery.Position, expiration)
				if err == nil {
					kind := "change"
					if item.delivery.Snapshot {
						kind = "snapshot"
					}
					err = write(item.ctx, fmt.Sprintf("id: %s\nevent: %s\ndata: %s\n\n", cursor, kind, item.delivery.Data))
				}
			}
			item.ack <- err
			if err != nil {
				return
			}
		}
	}
}
