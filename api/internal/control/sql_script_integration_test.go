package control

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSQLScriptActualPostgreSQLParsingResultsAndOutcomes(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "control_ci" {
		t.Fatal("isolated control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	run := func(script string) sqlScriptResult {
		t.Helper()
		out, err := executeSQLScript(ctx, conn, script)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Run("multiple_real_result_sets", func(t *testing.T) {
		out := run("SELECT 1::integer AS first; SELECT 'two;not split'::text AS second")
		if out.Outcome != "completed" || out.StatementsCompleted != 2 || len(out.Results) != 2 || out.Results[0].Rows[0][0] != int32(1) || out.Results[1].Rows[0][0] != "two;not split" {
			t.Fatal("real result sets not preserved", out)
		}
	})
	t.Run("dollar_quoted_body_and_actual_write", func(t *testing.T) {
		out := run("CREATE TEMP TABLE script_probe(id integer PRIMARY KEY); DO $$BEGIN INSERT INTO script_probe VALUES(1); END$$; SELECT count(*)::integer FROM script_probe")
		if out.Outcome != "completed" || out.StatementsCompleted != 3 || out.Results[2].Rows[0][0] != int32(1) {
			t.Fatal("server parsing or actual write failed", out)
		}
	})
	t.Run("statement_error_no_fake_success", func(t *testing.T) {
		out := run("INSERT INTO script_probe VALUES(2); SELECT missing_column FROM script_probe")
		if out.Outcome != "failed" || out.Error == nil || out.Error.SQLState != "42703" {
			t.Fatal("SQL error fabricated success", out)
		}
		var count int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM script_probe").Scan(&count); err != nil || count != 1 {
			t.Fatal("PostgreSQL implicit transaction outcome", count, err)
		}
	})
	t.Run("explicit_prior_commit_is_not_claimed_rolled_back", func(t *testing.T) {
		out := run("BEGIN; INSERT INTO script_probe VALUES(3); COMMIT; SELECT missing_column FROM script_probe")
		if out.Outcome != "failed" || out.OpenTransactionDiscarded {
			t.Fatal("incorrect explicit commit outcome", out)
		}
		var count int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM script_probe WHERE id=3").Scan(&count); err != nil || count != 1 {
			t.Fatal("explicit commit not durable", count, err)
		}
	})
	t.Run("unclosed_transaction_is_explicitly_marked", func(t *testing.T) {
		out := run("BEGIN; INSERT INTO script_probe VALUES(4)")
		if out.Outcome != "failed" || !out.OpenTransactionDiscarded || out.Error == nil || out.Error.Code != "sql_script_transaction_open" {
			t.Fatal("uncommitted script claimed durable", out)
		}
		if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("bounded_results_without_truncating_execution", func(t *testing.T) {
		script := ""
		for i := 0; i < 20; i++ {
			script += "SELECT 1;"
		}
		script += "INSERT INTO script_probe VALUES(5)"
		out := run(script)
		if out.Outcome != "completed" || len(out.Results) != scriptMaxResults || out.StatementsCompleted != 21 || !out.ResultsTruncated {
			t.Fatal("result budget changed execution", out)
		}
		var count int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM script_probe WHERE id=5").Scan(&count); err != nil || count != 1 {
			t.Fatal("tail intent was not executed", count, err)
		}
	})
	t.Run("bounded_rows_and_nonfinite_values", func(t *testing.T) {
		out := run("SELECT generate_series(1,205); SELECT 'NaN'::float8")
		if out.Outcome != "completed" || len(out.Results[0].Rows) != 200 || !out.ResultsTruncated {
			t.Fatal("row budget failed", out)
		}
		out = run("SELECT 'NaN'::float8,'Infinity'::float8,'NaN'::float4,9223372036854775807::bigint")
		if out.Results[0].Rows[0][0] != "NaN" || out.Results[0].Rows[0][1] != "Infinity" || out.Results[0].Rows[0][2] != "NaN" || out.Results[0].Rows[0][3] != "9223372036854775807" {
			t.Fatal("exact nonfinite and bigint JSON projection", out)
		}
	})
	t.Run("bounded_cell_payload", func(t *testing.T) {
		out := run("SELECT repeat('x',1100000); SELECT 7")
		if out.Outcome != "completed" || !out.ResultsTruncated || len(out.Results[0].Rows) != 0 || len(out.Results[1].Rows) != 1 {
			t.Fatal("cell payload budget failed", out)
		}
	})
}
