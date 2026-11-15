package realtime

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

type Subscription struct {
	Key           SourceKey
	Query         Query
	Relations     []Relation
	After         *int64
	PageSize      int32
	DeliveryLimit time.Duration
}

type Delivery struct {
	QueryResult
	Snapshot bool
}

// Subscribe starts from an authorized replay position or fenced snapshot.
// OpenWriter must return an exclusive connection to the verified original
// Writer as the query role. Authorize rechecks the registered query and
// principal before reading and before each result leaves the service.
func (s Store) Subscribe(ctx context.Context, notifications *Notifications, sub Subscription,
	authorize func(context.Context) error, openWriter func(context.Context) (*pgx.Conn, error),
	emit func(context.Context, Delivery) error) error {
	if notifications == nil || sub.PageSize <= 0 || sub.DeliveryLimit <= 0 || sub.Query.Timeout <= 0 || len(sub.Relations) == 0 ||
		authorize == nil || openWriter == nil || emit == nil {
		return errors.New("incomplete realtime subscription")
	}
	if err := authorize(ctx); err != nil {
		return err
	}
	wake, stop, err := notifications.Watch(sub.Key.ID)
	if err != nil {
		return err
	}
	defer stop()
	var cursor int64
	deliver := func(snapshot bool) error {
		reading, cancel := context.WithTimeout(ctx, sub.Query.Timeout)
		defer cancel()
		if err := authorize(reading); err != nil {
			return err
		}
		conn, err := openWriter(reading)
		if err != nil || conn == nil {
			return ErrSourceUnavailable
		}
		result, err := s.Snapshot(reading, sub.Key, conn, sub.Query)
		cleanup, closeConn := context.WithTimeout(context.Background(), time.Second)
		_ = conn.Close(cleanup)
		closeConn()
		if err != nil {
			return err
		}
		if err := authorize(reading); err != nil {
			return err
		}
		writing, finish := context.WithTimeout(ctx, sub.DeliveryLimit)
		defer finish()
		if err := emit(writing, Delivery{QueryResult: result, Snapshot: snapshot}); err != nil {
			return err
		}
		cursor = result.Position
		return nil
	}
	if sub.After == nil {
		if err := deliver(true); err != nil {
			return err
		}
	} else {
		cursor = *sub.After
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
		for {
			if err := authorize(ctx); err != nil {
				return err
			}
			batch, err := s.ReadAfter(ctx, sub.Key, cursor, sub.PageSize)
			if errors.Is(err, ErrCursorExpired) {
				if err := deliver(true); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			affected := slices.ContainsFunc(batch, func(event Invalidation) bool {
				return slices.ContainsFunc(event.Relations, func(relation Relation) bool {
					return slices.Contains(sub.Relations, relation)
				})
			})
			if affected {
				if err := deliver(false); err != nil {
					return err
				}
				continue
			}
			cursor = batch[len(batch)-1].Position
		}
	}
}
