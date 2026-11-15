package realtime

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrSlotUnavailable = errors.New("original logical slot is unavailable")
	ErrSlotAhead       = errors.New("logical slot advanced beyond the durable source cursor")
	ErrSlotWALBudget   = errors.New("logical slot exceeded its retained WAL budget")
)

func checkSlot(ctx context.Context, conn *pgx.Conn, lease Lease, maxWALBytes int64, streaming bool) error {
	var database, plugin, walStatus, confirmed, restart string
	var active, temporary, writer bool
	var lag int64
	err := conn.QueryRow(ctx, `
SELECT COALESCE(database::text,''),COALESCE(plugin,''),COALESCE(wal_status,''),
       COALESCE(confirmed_flush_lsn::text,''),COALESCE(restart_lsn::text,''),active,temporary,
       NOT pg_is_in_recovery(),COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(),restart_lsn),-1)::bigint
FROM pg_replication_slots
WHERE slot_name=$1 AND slot_type='logical' AND database=current_database()`, lease.Slot).
		Scan(&database, &plugin, &walStatus, &confirmed, &restart, &active, &temporary, &writer, &lag)
	if err != nil || database != lease.Key.Database || plugin != "pgoutput" || (!streaming && active) ||
		temporary || !writer || (walStatus != "reserved" && walStatus != "extended") || lag < 0 {
		return ErrSlotUnavailable
	}
	if lag > maxWALBytes {
		return ErrSlotWALBudget
	}
	if !streaming {
		applied, err := pglogrepl.ParseLSN(confirmed)
		if err != nil || applied == 0 {
			return ErrSlotUnavailable
		}
		oldest, err := pglogrepl.ParseLSN(restart)
		if err != nil || oldest == 0 {
			return ErrSlotUnavailable
		}
		if applied > lease.ConfirmedLSN || oldest > lease.ConfirmedLSN {
			return ErrSlotAhead
		}
	}
	return nil
}

type WorkerLimits struct {
	LeaseDuration    time.Duration
	FeedbackInterval time.Duration
	MaxFrameBytes    int
	MaxWALBytes      int64
}

// RunSource consumes one registered source. Each opener must return its own
// authorized connection, bound to the original branch, database and current
// verified Writer. openReplication configures PostgreSQL replication=database.
func (s Store) RunSource(ctx context.Context, key SourceKey, limits WorkerLimits,
	openSQL func(context.Context, Lease) (*pgx.Conn, error),
	openReplication func(context.Context, Lease) (*pgconn.PgConn, error)) error {
	if limits.LeaseDuration < 3*time.Microsecond || limits.FeedbackInterval <= 0 ||
		limits.MaxFrameBytes <= 0 || limits.MaxWALBytes <= 0 || openSQL == nil || openReplication == nil {
		return errors.New("incomplete logical source worker configuration")
	}
	lease, err := s.Acquire(ctx, key, limits.LeaseDuration)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Release(cleanup, lease)
	}()
	starting, cancelStart := context.WithTimeout(ctx, limits.LeaseDuration/3)
	defer cancelStart()
	sql, err := openSQL(starting, lease)
	if errors.Is(err, ErrSlotWALBudget) {
		return err
	}
	if err != nil || sql == nil {
		return ErrSourceUnavailable
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = sql.Close(cleanup)
	}()
	if err := checkSlot(starting, sql, lease, limits.MaxWALBytes, false); err != nil {
		return err
	}
	if err := VerifyPublication(starting, sql, lease.Publication, lease.Relations); err != nil {
		return err
	}
	replication, err := openReplication(starting, lease)
	if err != nil || replication == nil {
		return ErrSourceUnavailable
	}
	cancelStart()
	active, stop := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := limits.LeaseDuration / 3
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-ticker.C:
				checkCtx, cancel := context.WithTimeout(active, interval)
				err := checkSlot(checkCtx, sql, lease, limits.MaxWALBytes, true)
				if err == nil {
					err = VerifyPublication(checkCtx, sql, lease.Publication, lease.Relations)
				}
				if err == nil {
					err = s.Renew(checkCtx, lease, limits.LeaseDuration)
				}
				cancel()
				if err != nil {
					stop(err)
					return
				}
			}
		}
	}()
	source := Source{
		SystemID: lease.SystemID, Timeline: lease.PGTimeline, Database: key.Database, Slot: lease.Slot,
		Publication: lease.Publication, ConfirmedLSN: lease.ConfirmedLSN, Relations: lease.Relations,
		MaxFrameBytes: limits.MaxFrameBytes, FeedbackInterval: limits.FeedbackInterval,
	}
	err = Consume(active, replication, source, func(call context.Context, commit Commit) error {
		return s.Record(call, lease, commit)
	})
	stop(err)
	<-done
	return context.Cause(active)
}
