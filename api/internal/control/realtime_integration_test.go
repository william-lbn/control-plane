package control

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/realtime"
)

func TestRealtimeSourcePersistence(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA") + "_realtime"
	if len(schema) > 63 || !regexp.MustCompile(`^v2_auth_[a-z0-9_]+$`).MatchString(schema) {
		t.Fatal("isolated schema required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.Exec(ctx, query, args...)
		must(err)
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_rt','realtime','Realtime');
INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at)
VALUES('prj_a','org_rt','A','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','rke2-lab',17,'ready','managed',now(),now()),
      ('prj_b','org_rt','B','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','rke2-lab',18,'ready','managed',now(),now());
INSERT INTO branches(id,project_id,name,timeline_id,is_default,state,created_at)
VALUES('br_a','prj_a','main','11111111111111111111111111111111',true,'ready',now()),
      ('br_b','prj_b','main','22222222222222222222222222222222',true,'ready',now());`)

	scope := realtime.Scope{Project: "prj_a", Branch: "br_a", Timeline: "11111111111111111111111111111111", Database: "neondb"}
	key := realtime.SourceKey{ID: "rts_111111111111111111111111", Scope: scope}
	relation := realtime.Relation{Schema: "public", Name: "orders"}
	source := realtime.Source{SystemID: "7654321", Timeline: 1, Database: scope.Database, Slot: "ws_one", Publication: "pub_one",
		ConfirmedLSN: 100, Relations: []realtime.Relation{relation}}
	store := realtime.Store{DB: db}
	wrong := key
	wrong.Project = "prj_b"
	if err := store.Register(ctx, wrong, source, 2); !errors.Is(err, realtime.ErrSourceUnavailable) {
		t.Fatalf("cross-project source registration: %v", err)
	}
	must(store.Register(ctx, key, source, 2))
	lease, err := store.Acquire(ctx, key, time.Minute)
	must(err)
	if lease.ConfirmedLSN != source.ConfirmedLSN || lease.SystemID != source.SystemID || lease.PGTimeline != source.Timeline ||
		lease.Slot != source.Slot || lease.Publication != source.Publication || len(lease.Relations) != 1 || lease.Relations[0] != relation {
		t.Fatalf("stored source differs: %+v", lease)
	}
	if _, err := store.Acquire(ctx, key, time.Minute); !errors.Is(err, realtime.ErrLeaseLost) {
		t.Fatalf("concurrent source acquisition: %v", err)
	}
	if _, err := store.ReadAfter(ctx, wrong, 0, 2); !errors.Is(err, realtime.ErrSourceUnavailable) {
		t.Fatalf("cross-project event read: %v", err)
	}
	first := realtime.Commit{LSN: 101, EndLSN: 105, Relations: []realtime.Relation{relation}}
	forged := first
	forged.Relations = []realtime.Relation{{Schema: "public", Name: "restricted"}}
	if err := store.Record(ctx, lease, forged); err == nil {
		t.Fatal("recorded an unauthorized relation")
	}
	if empty, err := store.ReadAfter(ctx, key, 0, 1); err != nil || len(empty) != 0 {
		t.Fatalf("read after refused commit: %+v %v", empty, err)
	}
	must(store.Record(ctx, lease, first))
	must(store.Record(ctx, lease, first))
	must(store.Renew(ctx, lease, time.Minute))
	must(store.Release(ctx, lease))
	secondLease, err := store.Acquire(ctx, key, time.Minute)
	must(err)
	if secondLease.Epoch <= lease.Epoch || secondLease.ConfirmedLSN != first.EndLSN {
		t.Fatalf("new lease did not resume the committed position: %+v", secondLease)
	}
	second := realtime.Commit{LSN: 105, EndLSN: 108, Relations: []realtime.Relation{relation}}
	if err := store.Record(ctx, lease, second); !errors.Is(err, realtime.ErrLeaseLost) {
		t.Fatalf("stale source updated durable position: %v", err)
	}
	must(store.Release(ctx, lease))
	must(store.Record(ctx, secondLease, second))
	must(store.Record(ctx, secondLease, realtime.Commit{LSN: 109, EndLSN: 111}))
	if _, err := store.ReadAfter(ctx, key, 0, 2); !errors.Is(err, realtime.ErrCursorExpired) {
		t.Fatalf("evicted cursor was accepted: %v", err)
	}
	page, err := store.ReadAfter(ctx, key, 1, 1)
	must(err)
	if len(page) != 1 || page[0].Position != 2 || page[0].EndLSN != second.EndLSN || page[0].Relations[0] != relation {
		t.Fatalf("bounded replay returned %+v", page)
	}
	exec(`UPDATE branches SET state='deleting' WHERE id='br_a'`)
	if err := store.Renew(ctx, secondLease, time.Minute); !errors.Is(err, realtime.ErrLeaseLost) {
		t.Fatalf("deleting branch renewed lease: %v", err)
	}
	must(store.Retire(ctx, key))
	must(store.Retire(ctx, key))
	var left int
	must(db.QueryRow(ctx, "SELECT count(*) FROM realtime_invalidations WHERE source_id=$1", key.ID).Scan(&left))
	if left != 0 {
		t.Fatalf("retired source retains %d event rows", left)
	}
	exec(`UPDATE branches SET state='ready' WHERE id='br_a'`)
	newKey := key
	newKey.ID = "rts_222222222222222222222222"
	source.ConfirmedLSN = 200
	source.Slot = "ws_two"
	source.Publication = "pub_two"
	must(store.Register(ctx, newKey, source, 2))
	if _, err := store.ReadAfter(ctx, key, 3, 2); !errors.Is(err, realtime.ErrSourceUnavailable) {
		t.Fatalf("retired generation cursor became readable: %v", err)
	}
}
