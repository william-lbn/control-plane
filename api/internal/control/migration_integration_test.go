package control

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Run explicitly against an isolated test schema. The schema is kept as evidence.
func TestMigrateFreshSchema(t *testing.T) {
	url := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_SCHEMA")
	if !regexp.MustCompile(`^v2_migration_[a-z0-9_]+$`).MatchString(schema) {
		t.Fatal("NEON_V2_TEST_SCHEMA must be a dedicated v2_migration_* schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	entries, err := migrations.ReadDir("migrations")
	if err != nil || len(entries) == 0 {
		t.Fatalf("embedded migration registry is empty or unavailable: %v", err)
	}
	for run := 1; run <= 2; run++ {
		if err := migrate(ctx, pool); err != nil {
			t.Fatalf("migration run %d: %v", run, err)
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
			t.Fatal(err)
		}
		// 两份已发布058保留原字节，只有该已审查重复由高位前向join补齐。
		// 登记器对其他重复先失败，因此numeric登记比源码文件恰少1条。
		if count != len(entries)-1 {
			t.Fatalf("migration run %d: expected %d distinct versions, got %d", run, len(entries)-1, count)
		}
	}
	for _, table := range []string{"organizations", "projects", "branches", "branch_service_instances", "login_failures", "operations", "api_keys", "organization_quotas", "branch_roles", "branch_databases"} {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", schema+"."+table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("missing table %s", table)
		}
	}
	for _, column := range []string{"endpoint_type", "idle_timeout_seconds"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns
			WHERE table_schema=$1 AND table_name='endpoints' AND column_name=$2)`, schema, column).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("missing endpoint replica/lifecycle column %s", column)
		}
	}
}
