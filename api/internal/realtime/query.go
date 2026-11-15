package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrQueryRejected = errors.New("registered realtime query could not be read")
	ErrResultBudget  = errors.New("registered realtime query exceeded its result budget")
)

// Query is supplied by the server's registered query definition. Subscriber
// input only supplies bound parameters to that definition.
type Query struct {
	Statement     string
	Role          string
	Params        []any
	MaxRows       int
	MaxBytes      int
	MaxFrameBytes int
	Timeout       time.Duration
}

type QueryResult struct {
	Position int64
	Data     json.RawMessage
}

// Snapshot reads from the verified original Writer using a dedicated
// least-privileged SQL connection. Taking the metadata fence first prevents a
// committed invalidation observed during the SELECT from being skipped.
func (s Store) Snapshot(ctx context.Context, key SourceKey, conn *pgx.Conn, query Query) (QueryResult, error) {
	if conn == nil || query.Statement == "" || query.Role == "" || query.MaxRows <= 0 ||
		query.MaxBytes <= 0 || query.MaxFrameBytes <= 0 || query.Timeout < time.Millisecond {
		return QueryResult{}, ErrQueryRejected
	}
	position, err := s.Head(ctx, key)
	if err != nil {
		return QueryResult{}, err
	}
	ctx, stop := context.WithTimeout(ctx, query.Timeout)
	defer stop()
	conn.PgConn().Frontend().SetMaxBodyLen(query.MaxFrameBytes)
	var result json.RawMessage
	err = pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var database, login, role string
		var writer bool
		err := tx.QueryRow(ctx, "SELECT current_database(),session_user,current_user,NOT pg_is_in_recovery()").
			Scan(&database, &login, &role, &writer)
		if err != nil || database != key.Database || login != query.Role || role != query.Role || !writer {
			return ErrQueryRejected
		}
		if _, err := tx.Exec(ctx, "SET LOCAL search_path='pg_catalog'"); err != nil {
			return ErrQueryRejected
		}
		deadline := strconv.FormatInt(query.Timeout.Milliseconds(), 10) + "ms"
		if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout',$1,true)", deadline); err != nil {
			return ErrQueryRejected
		}
		args := make([]any, 1, len(query.Params)+1)
		args[0] = pgx.QueryExecModeDescribeExec
		args = append(args, query.Params...)
		rows, err := tx.Query(ctx, query.Statement, args...)
		if err != nil {
			return ErrQueryRejected
		}
		defer rows.Close()
		fields := rows.FieldDescriptions()
		if len(fields) == 0 {
			return ErrQueryRejected
		}
		columns := make([]string, len(fields))
		for index, field := range fields {
			columns[index] = field.Name
		}
		encodedColumns, err := json.Marshal(columns)
		if err != nil {
			return ErrQueryRejected
		}
		var data bytes.Buffer
		data.WriteString(`{"columns":`)
		data.Write(encodedColumns)
		data.WriteString(`,"rows":[`)
		for n := 0; rows.Next(); n++ {
			if n == query.MaxRows {
				return ErrResultBudget
			}
			values, err := rows.Values()
			if err != nil {
				return ErrQueryRejected
			}
			row, err := json.Marshal(values)
			if err != nil {
				return ErrQueryRejected
			}
			if n > 0 {
				data.WriteByte(',')
			}
			if len(row) > query.MaxBytes-data.Len()-2 {
				return ErrResultBudget
			}
			data.Write(row)
		}
		if rows.Err() != nil {
			return ErrQueryRejected
		}
		data.WriteString("]}")
		if data.Len() > query.MaxBytes {
			return ErrResultBudget
		}
		result = data.Bytes()
		return nil
	})
	if err != nil {
		return QueryResult{}, err
	}
	if _, err := s.Head(ctx, key); err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Position: position, Data: result}, nil
}
