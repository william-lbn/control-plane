package control

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestControllerLeadershipIntegration(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_SCHEMA") + "_leader"
	if !regexp.MustCompile(`^v2_migration_[a-z0-9_]+_leader$`).MatchString(schema) || len(schema) > 63 {
		t.Fatal("new dedicated leadership schema required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal("retained schema exists or creation failed; use new attempt:", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 5
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	first, err := acquireControllerLease(ctx, db, roleWorker)
	if err != nil || first == nil {
		t.Fatalf("first leader not acquired: %v", err)
	}
	defer first.close()
	t.Run("second_worker_cannot_overlap", func(t *testing.T) {
		second, err := acquireControllerLease(ctx, db, roleWorker)
		if err != nil || second != nil {
			if second != nil {
				second.close()
			}
			t.Fatalf("overlapping leader: %v", err)
		}
		if err = first.heartbeat(ctx); err != nil {
			t.Fatal(err)
		}
	})
	firstEpoch := first.epoch
	first.close()
	second, err := acquireControllerLease(ctx, db, roleWorker)
	if err != nil || second == nil {
		t.Fatalf("takeover failed: %v", err)
	}
	defer second.close()
	t.Run("takeover_advances_epoch", func(t *testing.T) {
		if second.epoch != firstEpoch+1 {
			t.Fatalf("epoch=%d predecessor=%d", second.epoch, firstEpoch)
		}
		if err := first.heartbeat(ctx); !errors.Is(err, errControllerLeadershipLost) {
			t.Fatal("closed predecessor may renew")
		}
	})
	t.Run("terminated_session_fails_closed", func(t *testing.T) {
		var terminated bool
		if err := db.QueryRow(ctx, "SELECT pg_terminate_backend($1)", second.connection.Conn().PgConn().PID()).Scan(&terminated); err != nil || !terminated {
			t.Fatalf("terminate dedicated test leader: %v", err)
		}
		if err := second.heartbeat(ctx); !errors.Is(err, errControllerLeadershipLost) {
			t.Fatal("lost connection retained leadership")
		}
	})
	second.close()
	third, err := acquireControllerLease(ctx, db, roleWorker)
	if err != nil || third == nil {
		t.Fatalf("post-disconnect takeover failed: %v", err)
	}
	defer third.close()
	t.Run("successor_recovers_after_connection_loss", func(t *testing.T) {
		if third.epoch != second.epoch+1 {
			t.Fatal("disconnect takeover did not advance epoch")
		}
		if err := third.heartbeat(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("changed_generation_cannot_be_renewed_or_expired", func(t *testing.T) {
		if _, err := db.Exec(ctx, "UPDATE control_runtime_leases SET epoch=epoch+1 WHERE name='controllers'"); err != nil {
			t.Fatal(err)
		}
		if err := third.heartbeat(ctx); !errors.Is(err, errControllerLeadershipLost) {
			t.Fatal("stale generation renewed")
		}
		third.close()
		var live bool
		if err := db.QueryRow(ctx, "SELECT lease_expires_at>clock_timestamp() FROM control_runtime_leases WHERE name='controllers'").Scan(&live); err != nil || !live {
			t.Fatalf("old close modified successor generation: %v", err)
		}
	})
}
