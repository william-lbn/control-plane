package control

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const scriptMaxResults = 16
const scriptMaxRows = 200
const scriptMaxBytes = 1 << 20

type scriptStatement struct {
	Ordinal int `json:"ordinal"`
	sqlResult
}
type scriptFailure struct {
	Code     string `json:"code"`
	SQLState string `json:"sqlstate"`
}
type sqlScriptResult struct {
	Outcome                  string            `json:"outcome"`
	Results                  []scriptStatement `json:"results"`
	StatementsCompleted      int               `json:"statements_completed"`
	ResultsTruncated         bool              `json:"results_truncated"`
	OpenTransactionDiscarded bool              `json:"open_transaction_discarded"`
	Error                    *scriptFailure    `json:"error,omitempty"`
}

// PostgreSQL parses the entire script, including dollar quotes and comments.
// Never split on semicolons, inject BEGIN/COMMIT, or automatically replay a
// submitted script. Response budgets limit retained data, not execution intent.
func executeSQLScript(ctx context.Context, conn *pgx.Conn, script string) (sqlScriptResult, error) {
	result := sqlScriptResult{Outcome: "completed", Results: []scriptStatement{}}
	// An oversized wire result fails the connection rather than buffering an
	// arbitrary customer payload. The caller then reports an unknown outcome.
	conn.PgConn().Frontend().SetMaxBodyLen(8 << 20)
	reader := conn.PgConn().Exec(ctx, script)
	remainingRows, remainingBytes := scriptMaxRows, scriptMaxBytes
	var decodeErr error
	for reader.NextResult() {
		resultReader := reader.ResultReader()
		rows := pgx.RowsFromResultReader(conn.TypeMap(), resultReader)
		statement := scriptStatement{Ordinal: result.StatementsCompleted + 1, sqlResult: sqlResult{Columns: []string{}, Rows: [][]any{}}}
		retained := len(result.Results) < scriptMaxResults
		fields := rows.FieldDescriptions()
		for _, field := range fields {
			if len(statement.Columns) < 64 {
				statement.Columns = append(statement.Columns, string(field.Name))
			} else {
				statement.Truncated = true
			}
		}
		for rows.Next() {
			if !retained || remainingRows <= 0 {
				statement.Truncated = true
				continue
			}
			rawSize := 0
			for _, raw := range rows.RawValues() {
				rawSize += len(raw)
			}
			if rawSize > remainingBytes {
				statement.Truncated = true
				continue
			}
			values, err := rows.Values()
			if err != nil {
				decodeErr = err
				statement.Truncated = true
				continue
			}
			if len(values) > 64 {
				values = values[:64]
				statement.Truncated = true
			}
			for i, value := range values {
				switch number := value.(type) {
				case int64:
					if number > 9007199254740991 || number < -9007199254740991 {
						values[i] = strconv.FormatInt(number, 10)
					}
				case float64:
					values[i] = finiteScriptNumber(number)
				case float32:
					if math.IsNaN(float64(number)) || math.IsInf(float64(number), 0) {
						values[i] = finiteScriptNumber(float64(number))
					}
				}
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				decodeErr = err
				statement.Truncated = true
				continue
			}
			if len(encoded) > remainingBytes {
				statement.Truncated = true
				continue
			}
			remainingBytes -= len(encoded)
			remainingRows--
			statement.Rows = append(statement.Rows, values)
		}
		rows.Close()
		statement.Command = rows.CommandTag().String()
		if rows.Err() == nil {
			result.StatementsCompleted++
		}
		if retained {
			result.Results = append(result.Results, statement)
		} else {
			result.ResultsTruncated = true
		}
		if statement.Truncated {
			result.ResultsTruncated = true
		}
	}
	err := reader.Close()
	result.OpenTransactionDiscarded = conn.PgConn().TxStatus() != 'I'
	if err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) {
			result.Outcome = "failed"
			result.Error = &scriptFailure{Code: "sql_error", SQLState: pgError.Code}
			return result, nil
		}
		return result, err
	}
	if decodeErr != nil {
		return result, errors.New("submitted SQL script result could not be decoded")
	}
	if result.OpenTransactionDiscarded {
		result.Outcome = "failed"
		result.Error = &scriptFailure{Code: "sql_script_transaction_open", SQLState: ""}
	}
	return result, nil
}

func finiteScriptNumber(number float64) any {
	switch {
	case math.IsNaN(number):
		return "NaN"
	case math.IsInf(number, 1):
		return "Infinity"
	case math.IsInf(number, -1):
		return "-Infinity"
	default:
		return number
	}
}

func (s *server) querySQLScript(w http.ResponseWriter, r *http.Request) {
	item, ok := s.requireEndpoint(w, r)
	if !ok {
		return
	}
	var body struct {
		Password string `json:"password"`
		SQL      string `json:"sql"`
		Role     string `json:"role,omitempty"`
		Database string `json:"database,omitempty"`
	}
	if readJSON(r, &body) != nil || body.Password == "" || strings.TrimSpace(body.SQL) == "" || len(body.SQL) > 32000 {
		fail(w, r, 422, "invalid_request", "Password and a bounded SQL script required")
		return
	}
	role, database := body.Role, body.Database
	defaults, err := s.publicConnectionDefaults(r.Context(), item)
	if err != nil {
		fail(w, r, 503, "connection_defaults_unavailable", "Script was not submitted because customer connection defaults could not be read")
		return
	}
	if role == "" {
		role = stringVal(defaults["role"])
	}
	if database == "" {
		database = stringVal(defaults["database"])
	}
	if !validPGIdentifier(role) || !validPGIdentifier(database) || (role != "cloud_admin" && protectedPGRole(role)) {
		fail(w, r, 422, "invalid_connection_target", "Choose a valid public database role and database")
		return
	}
	history, historyErr := s.beginSQLHistory(r, item, "script", role, database, body.SQL)
	if historyErr != nil {
		fail(w, r, 503, "history_unavailable", "SQL was not submitted because its history admission could not be recorded")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	conn, err := connectSQL(ctx, s.proxyHost, s.proxyPort, role, body.Password, database, stringVal(item["selector"]))
	if err != nil {
		outcome, state := sqlHistoryFailure(err)
		history.finish(w, outcome, state)
		s.sqlFailureResponse(w, r, err)
		return
	}
	result, err := executeSQLScript(ctx, conn, body.SQL)
	// Close rolls back any unclosed transaction. Earlier explicit COMMITs may
	// already be durable; neither failures nor transport loss authorize replay.
	_ = conn.Close(context.Background())
	if err != nil {
		history.finish(w, "outcome_unknown", "")
		s.audit(r.Context(), userFrom(r).ID, "execute_sql_script", "endpoint", r.PathValue("endpoint"), "unknown", requestID(r))
		jsonResponse(w, 503, apiError{"sql_script_outcome_unknown", "Script was submitted; inspect original data before retrying. No automatic replay is safe.", requestID(r), false})
		return
	}
	state := ""
	if result.Error != nil {
		state = result.Error.SQLState
	}
	history.finish(w, result.Outcome, state)
	s.audit(r.Context(), userFrom(r).ID, "execute_sql_script", "endpoint", r.PathValue("endpoint"), result.Outcome, requestID(r))
	jsonResponse(w, 200, result)
}
