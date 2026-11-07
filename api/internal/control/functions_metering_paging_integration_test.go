package control

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This isolated table exercises only collector pagination. It deliberately
// exceeds the current preview fleet cap without provisioning runtime fixtures.
func TestFunctionResourceMeterCandidatesAdvanceAcrossPages(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_function_meter_page_" + randomToken(8)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(ctx, `CREATE TABLE function_definitions(id text PRIMARY KEY,project_id text,branch_id text,slug text,state text)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 17; i++ {
		if _, err = db.Exec(ctx, `INSERT INTO function_definitions VALUES($1,'project','branch','hello','active')`, fmt.Sprintf("fnc_%016x", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(ctx, `INSERT INTO function_definitions VALUES('fnc_0000000000000000','project','branch','stopped','suspended')`); err != nil {
		t.Fatal(err)
	}
	s := &server{db: db}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		rows, err := s.functionResourceMeterCandidates(ctx, cursor)
		if err != nil || len(rows) == 0 || len(rows) > 8 {
			t.Fatal("bounded page missing", err)
		}
		for _, row := range rows {
			id := stringVal(row["id"])
			if id <= cursor || seen[id] {
				t.Fatal("pagination repeated or regressed")
			}
			seen[id] = true
			cursor = id
		}
	}
	rows, err := s.functionResourceMeterCandidates(ctx, cursor)
	if err != nil || len(rows) != 0 || len(seen) != 17 {
		t.Fatal("later active definitions were starved", err)
	}
	rows, err = s.functionResourceMeterCandidates(ctx, "")
	if err != nil || len(rows) != 8 || stringVal(rows[0]["id"]) != "fnc_0000000000000001" {
		t.Fatal("new sweep did not restart from first active function", err)
	}
	cancel()
	if _, err = s.functionResourceMeterCandidates(ctx, ""); err == nil {
		t.Fatal("canceled collector continued metadata work")
	}
}
