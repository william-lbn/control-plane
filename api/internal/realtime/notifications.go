package realtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrSubscriptionCapacity = errors.New("realtime notification capacity exhausted")

const invalidationChannel = "realtime_invalidations"

// Notifications shares one metadata connection between streams in a process.
// A notification is a wake-up hint. The durable source remains authoritative
// when messages coalesce, a listener reconnects, or the process restarts.
type Notifications struct {
	db    *pgxpool.Pool
	limit int

	mu      sync.Mutex
	watches map[string]map[chan struct{}]struct{}
	count   int
	running bool
}

func NewNotifications(db *pgxpool.Pool, limit int) *Notifications {
	return &Notifications{db: db, limit: limit, watches: make(map[string]map[chan struct{}]struct{})}
}

// Watch registers before the caller's first durable read. Stop releases that
// stream's capacity and is safe to call on every exit path.
func (n *Notifications) Watch(sourceID string) (<-chan struct{}, func(), error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if sourceID == "" || n.count >= n.limit {
		return nil, nil, ErrSubscriptionCapacity
	}
	watch := make(chan struct{}, 1)
	watch <- struct{}{}
	if n.watches[sourceID] == nil {
		n.watches[sourceID] = make(map[chan struct{}]struct{})
	}
	n.watches[sourceID][watch] = struct{}{}
	n.count++
	return watch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		set := n.watches[sourceID]
		if _, ok := set[watch]; !ok {
			return
		}
		delete(set, watch)
		n.count--
		if len(set) == 0 {
			delete(n.watches, sourceID)
		}
		close(watch)
	}, nil
}

func signal(set map[chan struct{}]struct{}) {
	for watch := range set {
		select {
		case watch <- struct{}{}:
		default:
		}
	}
}

// Run retries only the notification connection. Once LISTEN becomes active,
// all streams read their durable cursor again to cover the reconnect gap.
func (n *Notifications) Run(ctx context.Context, reconnectDelay time.Duration) error {
	if n.db == nil || n.limit <= 0 || reconnectDelay <= 0 {
		return errors.New("incomplete realtime notification configuration")
	}
	n.mu.Lock()
	if n.running {
		n.mu.Unlock()
		return errors.New("realtime notification listener already running")
	}
	n.running = true
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.running = false
		n.mu.Unlock()
	}()
	for ctx.Err() == nil {
		if err := n.listen(ctx); err != nil && ctx.Err() == nil {
			timer := time.NewTimer(reconnectDelay)
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
		}
	}
	return ctx.Err()
}

func (n *Notifications) listen(ctx context.Context) error {
	conn, err := n.db.Acquire(ctx)
	if err != nil {
		return err
	}
	// The connection stays in LISTEN state until it is closed. It must
	// never become a normal metadata query connection in the shared pool.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Hijack().Close(cleanup)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+invalidationChannel); err != nil {
		return err
	}
	n.mu.Lock()
	for _, set := range n.watches {
		signal(set)
	}
	n.mu.Unlock()
	for {
		event, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if event.Channel == invalidationChannel {
			n.mu.Lock()
			signal(n.watches[event.Payload])
			n.mu.Unlock()
		}
	}
}
