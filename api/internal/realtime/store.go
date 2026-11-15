package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrSourceUnavailable = errors.New("logical source unavailable for its original branch")
	ErrLeaseLost         = errors.New("logical source lease is no longer current")
	ErrCursorExpired     = errors.New("logical source cursor requires a fresh snapshot")
)

type Scope struct {
	Project, Branch, Timeline, Database string
}

type SourceKey struct {
	ID string
	Scope
}

type Lease struct {
	Key          SourceKey
	Epoch        int64
	SystemID     string
	PGTimeline   int32
	Slot         string
	Publication  string
	Relations    []Relation
	ConfirmedLSN pglogrepl.LSN
}

type Invalidation struct {
	Position int64
	Commit
}

type Store struct {
	DB *pgxpool.Pool
}

// Register consumes the identity and consistent point of a newly provisioned
// slot after the caller verifies the original branch and holds its setup fence.
func (s Store) Register(ctx context.Context, key SourceKey, source Source, retain int32) error {
	if _, err := newDecoder(source.Relations); err != nil {
		return err
	}
	if key.Database != source.Database {
		return ErrSourceUnavailable
	}
	tag, err := s.DB.Exec(ctx, `
INSERT INTO realtime_sources(id,project_id,branch_id,timeline_id,database_name,system_id,pg_timeline,slot_name,publication,relations,confirmed_lsn,retain_commits)
SELECT $1,p.id,b.id,b.timeline_id,$5,$6,$7,$8,$9,$10,$11::text::pg_lsn,$12
FROM projects p JOIN branches b ON b.project_id=p.id
WHERE p.id=$2 AND b.id=$3 AND b.timeline_id=$4 AND p.state='ready' AND p.deleted_at IS NULL
  AND b.state='ready' AND b.deleted_at IS NULL
FOR SHARE OF p,b`, key.ID, key.Project, key.Branch, key.Timeline, key.Database, source.SystemID,
		source.Timeline, source.Slot, source.Publication, source.Relations, source.ConfirmedLSN.String(), retain)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrSourceUnavailable
	}
	return err
}

const sourceScope = `s.id=$1 AND s.project_id=$2 AND s.branch_id=$3 AND s.timeline_id=$4
 AND s.database_name=$5 AND s.retired_at IS NULL
 AND EXISTS (SELECT 1 FROM projects p JOIN branches b ON b.project_id=p.id
             WHERE p.id=s.project_id AND b.id=s.branch_id AND b.timeline_id=s.timeline_id
               AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL)`

func (s Store) Acquire(ctx context.Context, key SourceKey, duration time.Duration) (Lease, error) {
	if duration < time.Microsecond {
		return Lease{}, ErrLeaseLost
	}
	lease := Lease{Key: key}
	var lsn string
	err := s.DB.QueryRow(ctx, `UPDATE realtime_sources s
SET lease_epoch=lease_epoch+1, lease_until=clock_timestamp()+$6::interval
WHERE `+sourceScope+` AND (lease_until IS NULL OR lease_until<=clock_timestamp())
RETURNING lease_epoch,system_id,pg_timeline,slot_name,publication,relations,confirmed_lsn::text`,
		key.ID, key.Project, key.Branch, key.Timeline, key.Database, pgtype.Interval{Microseconds: duration.Microseconds(), Valid: true}).
		Scan(&lease.Epoch, &lease.SystemID, &lease.PGTimeline, &lease.Slot, &lease.Publication, &lease.Relations, &lsn)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrLeaseLost
	}
	if err != nil {
		return Lease{}, err
	}
	lease.ConfirmedLSN, err = pglogrepl.ParseLSN(lsn)
	return lease, err
}

func (s Store) Renew(ctx context.Context, lease Lease, duration time.Duration) error {
	if duration < time.Microsecond {
		return ErrLeaseLost
	}
	key := lease.Key
	tag, err := s.DB.Exec(ctx, `UPDATE realtime_sources s SET lease_until=clock_timestamp()+$7::interval
WHERE `+sourceScope+` AND lease_epoch=$6 AND lease_until>clock_timestamp()`,
		key.ID, key.Project, key.Branch, key.Timeline, key.Database, lease.Epoch,
		pgtype.Interval{Microseconds: duration.Microseconds(), Valid: true})
	if err == nil && tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return err
}

func (s Store) Release(ctx context.Context, lease Lease) error {
	key := lease.Key
	_, err := s.DB.Exec(ctx, `UPDATE realtime_sources SET lease_until=NULL
WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND timeline_id=$4 AND database_name=$5 AND lease_epoch=$6`,
		key.ID, key.Project, key.Branch, key.Timeline, key.Database, lease.Epoch)
	return err
}

func (s Store) Retire(ctx context.Context, key SourceKey) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE realtime_sources SET retired_at=COALESCE(retired_at,clock_timestamp()), lease_until=NULL
WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND timeline_id=$4 AND database_name=$5`,
			key.ID, key.Project, key.Branch, key.Timeline, key.Database)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrSourceUnavailable
		}
		_, err = tx.Exec(ctx, `DELETE FROM realtime_invalidations WHERE source_id=$1`, key.ID)
		return err
	})
}

// Record commits the invalidation and PostgreSQL cursor atomically. An ACK can
// follow only after this transaction is durably committed.
func (s Store) Record(ctx context.Context, lease Lease, commit Commit) error {
	if commit.LSN == 0 || commit.EndLSN <= commit.LSN {
		return errors.New("invalid logical commit cursor")
	}
	if commit.Relations == nil {
		commit.Relations = []Relation{}
	}
	key := lease.Key
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit='on'`); err != nil {
			return err
		}
		var latest int64
		var retain int32
		var lsn string
		var allowed json.RawMessage
		err := tx.QueryRow(ctx, `SELECT s.confirmed_lsn::text,s.latest,s.retain_commits,s.relations
FROM realtime_sources s JOIN branches b ON b.id=s.branch_id AND b.project_id=s.project_id AND b.timeline_id=s.timeline_id
JOIN projects p ON p.id=b.project_id
WHERE s.id=$1 AND s.project_id=$2 AND s.branch_id=$3 AND s.timeline_id=$4 AND s.database_name=$5
  AND s.retired_at IS NULL AND s.lease_epoch=$6 AND s.lease_until>clock_timestamp()
  AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL
FOR UPDATE OF s FOR SHARE OF p,b`, key.ID, key.Project, key.Branch, key.Timeline, key.Database, lease.Epoch).
			Scan(&lsn, &latest, &retain, &allowed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		var authorized bool
		if err := tx.QueryRow(ctx, `SELECT $1::jsonb <@ $2::jsonb`, commit.Relations, allowed).Scan(&authorized); err != nil {
			return err
		}
		if !authorized {
			return errors.New("logical commit includes unauthorized relations")
		}
		durableLSN, err := pglogrepl.ParseLSN(lsn)
		if err != nil {
			return err
		}
		if commit.EndLSN == durableLSN {
			var replay bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM realtime_invalidations
WHERE source_id=$1 AND position=$2 AND commit_lsn=$3::text::pg_lsn AND relations=$4::jsonb)`,
				key.ID, latest, commit.LSN.String(), commit.Relations).Scan(&replay)
			if err == nil && !replay {
				return errors.New("logical commit replay differs from its receipt")
			}
			return err
		}
		if commit.LSN < durableLSN {
			return errors.New("logical commit moved behind its durable cursor")
		}
		latest++
		_, err = tx.Exec(ctx, `INSERT INTO realtime_invalidations(source_id,position,commit_lsn,end_lsn,relations)
VALUES($1,$2,$3::text::pg_lsn,$4::text::pg_lsn,$5)`, key.ID, latest, commit.LSN.String(), commit.EndLSN.String(), commit.Relations)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE realtime_sources SET latest=$2,confirmed_lsn=$3::text::pg_lsn
WHERE id=$1 AND lease_epoch=$4 AND lease_until>clock_timestamp()`, key.ID, latest, commit.EndLSN.String(), lease.Epoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		_, err = tx.Exec(ctx, `DELETE FROM realtime_invalidations WHERE source_id=$1 AND position<=$2`,
			key.ID, latest-int64(retain))
		return err
	})
}

// ReadAfter takes one metadata snapshot for the retention bound and the events.
// A caller that fell behind this generation's retained history needs a snapshot.
func (s Store) ReadAfter(ctx context.Context, key SourceKey, cursor int64, limit int32) ([]Invalidation, error) {
	if cursor < 0 || limit <= 0 {
		return nil, ErrCursorExpired
	}
	var result []Invalidation
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var latest int64
		var retain int32
		err := tx.QueryRow(ctx, `SELECT latest,retain_commits FROM realtime_sources s WHERE `+sourceScope,
			key.ID, key.Project, key.Branch, key.Timeline, key.Database).Scan(&latest, &retain)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSourceUnavailable
		}
		if err != nil {
			return err
		}
		if cursor > latest || cursor < latest-int64(retain) {
			return ErrCursorExpired
		}
		rows, err := tx.Query(ctx, `SELECT position,commit_lsn::text,end_lsn::text,relations
FROM realtime_invalidations WHERE source_id=$1 AND position>$2 ORDER BY position LIMIT $3`, key.ID, cursor, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		result = []Invalidation{}
		for rows.Next() {
			var item Invalidation
			var begin, end string
			if err := rows.Scan(&item.Position, &begin, &end, &item.Relations); err != nil {
				return err
			}
			if item.LSN, err = pglogrepl.ParseLSN(begin); err != nil {
				return err
			}
			if item.EndLSN, err = pglogrepl.ParseLSN(end); err != nil {
				return err
			}
			result = append(result, item)
		}
		return rows.Err()
	})
	return result, err
}
