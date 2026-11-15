// Package realtime reads committed relation invalidations for the live-query service.
package realtime

import (
	"context"
	"encoding/binary"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

type Relation struct {
	Schema string
	Name   string
}

type Commit struct {
	LSN       pglogrepl.LSN
	EndLSN    pglogrepl.LSN
	Relations []Relation
}

// The caller must bind the connection to the authorized Neon branch and its
// database. PostgreSQL's system ID can be shared by forks, so it only checks
// consistency of that binding. The caller must hold the source lease and
// verify that the slot is pgoutput and its confirmed_flush_lsn is at or
// behind ConfirmedLSN before exclusively starting the stream.
type Source struct {
	SystemID         string
	Timeline         int32
	Database         string
	Slot             string
	Publication      string
	ConfirmedLSN     pglogrepl.LSN
	Relations        []Relation
	MaxFrameBytes    int
	FeedbackInterval time.Duration
}

var replicationName = regexp.MustCompile(`^[a-z0-9_]{1,63}$`)

type transaction struct {
	final   pglogrepl.LSN
	changed map[Relation]struct{}
}

type decoder struct {
	allowed map[Relation]struct{}
	byID    map[uint32]Relation
	byName  map[Relation]uint32
	tx      *transaction
}

func newDecoder(relations []Relation) (*decoder, error) {
	d := &decoder{allowed: make(map[Relation]struct{}, len(relations)), byID: make(map[uint32]Relation), byName: make(map[Relation]uint32)}
	for _, relation := range relations {
		if relation.Schema == "" || relation.Name == "" || strings.ContainsRune(relation.Schema, 0) || strings.ContainsRune(relation.Name, 0) {
			return nil, errors.New("logical source has an invalid authorized relation")
		}
		if _, exists := d.allowed[relation]; exists {
			return nil, errors.New("logical source repeats an authorized relation")
		}
		d.allowed[relation] = struct{}{}
	}
	if len(d.allowed) == 0 {
		return nil, errors.New("logical source requires authorized relations")
	}
	return d, nil
}

func (d *decoder) change(id uint32) error {
	relation, ok := d.byID[id]
	if d.tx == nil || !ok {
		return errors.New("logical change has no original transaction or relation")
	}
	d.tx.changed[relation] = struct{}{}
	return nil
}

func (d *decoder) accept(msg pglogrepl.Message) (*Commit, error) {
	switch m := msg.(type) {
	case *pglogrepl.BeginMessage:
		if d.tx != nil || m.FinalLSN == 0 {
			return nil, errors.New("invalid pgoutput transaction begin")
		}
		d.tx = &transaction{final: m.FinalLSN, changed: make(map[Relation]struct{})}
	case *pglogrepl.CommitMessage:
		if d.tx == nil || m.Flags != 0 || m.CommitLSN != d.tx.final || m.TransactionEndLSN <= m.CommitLSN {
			return nil, errors.New("pgoutput commit differs from original transaction")
		}
		out := &Commit{LSN: m.CommitLSN, EndLSN: m.TransactionEndLSN, Relations: make([]Relation, 0, len(d.tx.changed))}
		for relation := range d.tx.changed {
			out.Relations = append(out.Relations, relation)
		}
		slices.SortFunc(out.Relations, func(a, b Relation) int {
			if result := strings.Compare(a.Schema, b.Schema); result != 0 {
				return result
			}
			return strings.Compare(a.Name, b.Name)
		})
		d.tx = nil
		return out, nil
	case *pglogrepl.RelationMessage:
		relation := Relation{Schema: m.Namespace, Name: m.RelationName}
		if _, ok := d.allowed[relation]; !ok || m.RelationID == 0 {
			return nil, errors.New("pgoutput relation is outside the authorized publication")
		}
		if id, ok := d.byName[relation]; ok {
			delete(d.byID, id)
		}
		if previous, ok := d.byID[m.RelationID]; ok {
			delete(d.byName, previous)
		}
		d.byID[m.RelationID], d.byName[relation] = relation, m.RelationID
	case *pglogrepl.OriginMessage, *pglogrepl.TypeMessage:
		// Only relation IDs on data messages influence invalidations.
	default:
		return nil, errors.New("unsupported message in pgoutput protocol version 1")
	}
	return nil, nil
}

func skipTuple(data []byte) ([]byte, bool) {
	if len(data) < 2 {
		return nil, false
	}
	count := binary.BigEndian.Uint16(data)
	data = data[2:]
	for i := uint16(0); i < count; i++ {
		if len(data) == 0 {
			return nil, false
		}
		switch data[0] {
		case 'n', 'u':
			data = data[1:]
		case 't', 'b':
			if len(data) < 5 {
				return nil, false
			}
			size := binary.BigEndian.Uint32(data[1:])
			data = data[5:]
			if uint64(size) > uint64(len(data)) {
				return nil, false
			}
			data = data[size:]
		default:
			return nil, false
		}
	}
	return data, true
}

func (d *decoder) acceptFrame(data []byte) (*Commit, error) {
	if len(data) == 0 {
		return nil, errors.New("empty pgoutput frame")
	}
	switch data[0] {
	case 'I', 'U', 'D':
		// Validate tuple boundaries in place. The query invalidation stream
		// only carries relation names, so no tuple values need to be copied.
		if len(data) < 6 {
			return nil, errors.New("incomplete pgoutput row change")
		}
		kind, id, tuple := data[0], binary.BigEndian.Uint32(data[1:]), data[5:]
		if kind == 'U' && (tuple[0] == 'K' || tuple[0] == 'O') {
			var valid bool
			tuple, valid = skipTuple(tuple[1:])
			if !valid || len(tuple) == 0 {
				return nil, errors.New("invalid pgoutput original row")
			}
		}
		marker := tuple[0]
		rest, valid := skipTuple(tuple[1:])
		if !valid || len(rest) != 0 || (kind == 'D' && marker != 'K' && marker != 'O') || (kind != 'D' && marker != 'N') {
			return nil, errors.New("invalid pgoutput row change")
		}
		return nil, d.change(id)
	case 'T':
		if len(data) < 6 || (len(data)-6)%4 != 0 || uint64(binary.BigEndian.Uint32(data[1:])) != uint64((len(data)-6)/4) {
			return nil, errors.New("invalid pgoutput truncate frame")
		}
		for span := data[6:]; len(span) > 0; span = span[4:] {
			if err := d.change(binary.BigEndian.Uint32(span)); err != nil {
				return nil, err
			}
		}
		return nil, nil
	default:
		message, err := parseMessage(data)
		if err != nil {
			return nil, err
		}
		return d.accept(message)
	}
}

func parseMessage(data []byte) (message pglogrepl.Message, err error) {
	switch data[0] {
	case 'B', 'C', 'R', 'O', 'Y':
	default:
		return nil, errors.New("unsupported message in pgoutput protocol version 1")
	}
	// Some variable-length fields in this decoder index the byte slice directly.
	// An incomplete frame ends the stream and leaves the confirmed LSN unchanged.
	defer func() {
		if recover() != nil {
			message, err = nil, errors.New("invalid pgoutput frame")
		}
	}()
	message, err = pglogrepl.Parse(data)
	if err != nil {
		return nil, errors.New("invalid pgoutput frame")
	}
	return message, nil
}

// Consume owns the replication connection until it returns. Persist must
// atomically save each invalidation with its EndLSN before returning success.
// Replay starts from the last durable EndLSN on a newly verified connection.
func Consume(ctx context.Context, conn *pgconn.PgConn, source Source, persist func(context.Context, Commit) error) error {
	if conn == nil {
		return errors.New("missing logical replication connection")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}()
	if persist == nil || source.SystemID == "" || source.Timeline <= 0 || source.Database == "" ||
		!replicationName.MatchString(source.Slot) || !replicationName.MatchString(source.Publication) ||
		source.ConfirmedLSN == 0 || source.MaxFrameBytes <= 0 || source.FeedbackInterval <= 0 {
		return errors.New("incomplete logical stream source")
	}
	d, err := newDecoder(source.Relations)
	if err != nil {
		return err
	}
	conn.Frontend().SetMaxBodyLen(source.MaxFrameBytes)
	identity, err := pglogrepl.IdentifySystem(ctx, conn)
	if err != nil || identity.SystemID != source.SystemID || identity.Timeline != source.Timeline || identity.DBName != source.Database {
		return errors.New("logical connection differs from the original database source")
	}
	if err := pglogrepl.StartReplication(ctx, conn, source.Slot, source.ConfirmedLSN, pglogrepl.StartReplicationOptions{
		Mode: pglogrepl.LogicalReplication, PluginArgs: []string{"proto_version '1'", "publication_names '" + source.Publication + "'"},
	}); err != nil {
		return errors.New("logical replication could not start from the confirmed LSN")
	}

	confirmed := source.ConfirmedLSN
	next := time.Now().Add(source.FeedbackInterval)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(next) {
			deadline := time.Now().Add(time.Second)
			if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
				deadline = limit
			}
			if err := conn.Conn().SetWriteDeadline(deadline); err != nil {
				return err
			}
			err := pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{WALWritePosition: confirmed})
			if reset := conn.Conn().SetWriteDeadline(time.Time{}); err != nil || reset != nil {
				return errors.New("logical feedback could not be confirmed")
			}
			next = time.Now().Add(source.FeedbackInterval)
		}
		readCtx, cancel := context.WithDeadline(ctx, next)
		msg, err := conn.ReceiveMessage(readCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil && pgconn.Timeout(err) {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("logical stream receive failed")
		}
		if _, isNotice := msg.(*pgproto3.NoticeResponse); isNotice {
			continue
		}
		copyData, ok := msg.(*pgproto3.CopyData)
		if !ok || len(copyData.Data) == 0 {
			return errors.New("unexpected message in active logical stream")
		}
		data := copyData.Data
		switch data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			keepalive, err := pglogrepl.ParsePrimaryKeepaliveMessage(data[1:])
			if err != nil {
				return errors.New("invalid logical keepalive")
			}
			if keepalive.ReplyRequested {
				next = time.Now()
			}
		case pglogrepl.XLogDataByteID:
			wal, err := pglogrepl.ParseXLogData(data[1:])
			if err != nil {
				return errors.New("invalid logical WAL frame")
			}
			commit, err := d.acceptFrame(wal.WALData)
			if err != nil {
				return err
			}
			if commit == nil || commit.EndLSN <= source.ConfirmedLSN {
				continue
			}
			if commit.LSN < confirmed {
				return errors.New("pgoutput commit moved behind the confirmed LSN")
			}
			if err := persist(ctx, *commit); err != nil {
				return err
			}
			confirmed = commit.EndLSN
		default:
			return errors.New("unsupported logical CopyData frame")
		}
	}
}
