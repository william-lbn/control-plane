package realtime

import (
	"context"
	"errors"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
)

// VerifySlotWALRetention checks PostgreSQL's checkpoint retention on the
// original Writer independently of the maximum lag enforced by the worker.
func VerifySlotWALRetention(ctx context.Context, conn *pgx.Conn, budget int64) error {
	var retained int64
	err := conn.QueryRow(ctx, "SELECT setting::bigint*1048576 FROM pg_settings WHERE name='max_slot_wal_keep_size' AND unit='MB'").Scan(&retained)
	if err != nil || retained <= 0 || retained > budget {
		return ErrSlotWALBudget
	}
	return nil
}

// ReadInactiveSlot validates the physical object before a provisioning
// replay or retirement. The caller owns the reserved slot name and holds
// its lifecycle fence.
func ReadInactiveSlot(ctx context.Context, conn *pgx.Conn, key SourceKey, name string, budget int64) (pglogrepl.LSN, bool, error) {
	if name != key.ID || !replicationName.MatchString(name) {
		return 0, false, ErrSlotUnavailable
	}
	var database, kind, plugin, status, confirmed string
	var active, temporary bool
	var lag int64
	err := conn.QueryRow(ctx, `
SELECT COALESCE(database::text,''),slot_type,COALESCE(plugin,''),COALESCE(wal_status,''),
       COALESCE(confirmed_flush_lsn::text,''),active,temporary,
       COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(),restart_lsn),-1)::bigint
FROM pg_catalog.pg_replication_slots WHERE slot_name=$1`, name).
		Scan(&database, &kind, &plugin, &status, &confirmed, &active, &temporary, &lag)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil || database != key.Database || kind != "logical" || plugin != "pgoutput" || active || temporary {
		return 0, false, ErrSlotUnavailable
	}
	// A retired source can clean up its original slot after PostgreSQL
	// invalidated the retained WAL. A registered source needs a live cursor.
	if budget == 0 {
		return 0, true, nil
	}
	if lag > budget {
		return 0, false, ErrSlotWALBudget
	}
	point, err := pglogrepl.ParseLSN(confirmed)
	if err != nil || point == 0 || lag < 0 || (status != "reserved" && status != "extended") {
		return 0, false, ErrSlotUnavailable
	}
	return point, true, nil
}
