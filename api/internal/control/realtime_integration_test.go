package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

func TestRealtimeSourceWorker(t *testing.T) {
	dsn := os.Getenv("NEON_REALTIME_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable PostgreSQL cluster required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "realtime_wire_test" {
		t.Fatal("integration test requires a disposable realtime_wire_test database")
	}
	sql, err := pgx.ConnectConfig(ctx, cfg.ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer sql.Close(ctx)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string) {
		t.Helper()
		_, err := sql.Exec(ctx, query)
		must(err)
	}
	exec(`CREATE SCHEMA wire_worker_meta;
CREATE ROLE rt_wire_worker LOGIN REPLICATION;
CREATE ROLE rt_wire_reader LOGIN;
CREATE TABLE public.rt_worker_events(id integer PRIMARY KEY);
CREATE TABLE public.rt_worker_private(id integer PRIMARY KEY);
INSERT INTO public.rt_worker_private VALUES(4);
GRANT SELECT ON public.rt_worker_events TO rt_wire_reader;
CREATE PUBLICATION rt_worker_pub FOR TABLE public.rt_worker_events;`)
	cfg.ConnConfig.RuntimeParams["search_path"] = "wire_worker_meta"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer db.Close()
	must(migrate(ctx, db))
	_, err = db.Exec(ctx, `INSERT INTO organizations(id,slug,name) VALUES('org_worker','worker','Realtime worker');
INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at)
VALUES('prj_worker','org_worker','Worker','33333333333333333333333333333333','rke2-lab',17,'ready','managed',now(),now());
INSERT INTO branches(id,project_id,name,timeline_id,is_default,state,created_at)
VALUES('br_worker','prj_worker','main','44444444444444444444444444444444',true,'ready',now());`)
	must(err)

	var created string
	must(sql.QueryRow(ctx, "SELECT lsn::text FROM pg_create_logical_replication_slot('rt_worker_slot','pgoutput')").Scan(&created))
	start, err := pglogrepl.ParseLSN(created)
	must(err)
	config := cfg.ConnConfig.Config.Copy()
	config.User = "rt_wire_worker"
	config.RuntimeParams = map[string]string{"replication": "database"}
	open := func(ctx context.Context, _ realtime.Lease) (*pgconn.PgConn, error) {
		return pgconn.ConnectConfig(ctx, config.Copy())
	}
	replication, err := open(ctx, realtime.Lease{})
	must(err)
	pgIdentity, err := pglogrepl.IdentifySystem(ctx, replication)
	must(err)
	must(replication.Close(ctx))

	store := realtime.Store{DB: db}
	key := realtime.SourceKey{ID: "rts_333333333333333333333333", Scope: realtime.Scope{
		Project: "prj_worker", Branch: "br_worker", Timeline: "44444444444444444444444444444444", Database: cfg.ConnConfig.Database,
	}}
	relation := realtime.Relation{Schema: "public", Name: "rt_worker_events"}
	must(store.Register(ctx, key, realtime.Source{
		SystemID: pgIdentity.SystemID, Timeline: pgIdentity.Timeline, Database: key.Database, Slot: "rt_worker_slot",
		Publication: "rt_worker_pub", ConfirmedLSN: start, Relations: []realtime.Relation{relation},
	}, 4))
	readConfig := cfg.ConnConfig.Copy()
	readConfig.User = "rt_wire_reader"
	reader, err := pgx.ConnectConfig(ctx, readConfig)
	must(err)
	defer reader.Close(ctx)
	notification, err := pgx.ConnectConfig(ctx, cfg.ConnConfig.Copy())
	must(err)
	defer notification.Close(ctx)
	_, err = notification.Exec(ctx, "LISTEN realtime_invalidations")
	must(err)
	query := realtime.Query{Statement: "SELECT id FROM public.rt_worker_events WHERE id>$1 ORDER BY id", Params: []any{0},
		Role: readConfig.User, MaxRows: 10, MaxBytes: 4096, MaxFrameBytes: 128 << 10, Timeout: 2 * time.Second}
	readSnapshot := func(expectedPosition int64, expectedRows int) {
		t.Helper()
		result, err := store.Snapshot(ctx, key, reader, query)
		must(err)
		var data struct {
			Columns []string `json:"columns"`
			Rows    [][]int  `json:"rows"`
		}
		must(json.Unmarshal(result.Data, &data))
		if result.Position != expectedPosition || len(data.Columns) != 1 || data.Columns[0] != "id" || len(data.Rows) != expectedRows {
			t.Fatalf("bounded query snapshot at %d: %+v", result.Position, data)
		}
	}
	readSnapshot(0, 0)
	private := query
	private.Statement, private.Params = "SELECT id FROM public.rt_worker_private", nil
	if _, err := store.Snapshot(ctx, key, reader, private); !errors.Is(err, realtime.ErrQueryRejected) {
		t.Fatalf("ungranted relation was readable: %v", err)
	}
	notifications := realtime.NewNotifications(db, 1)
	listening, stopListening := context.WithCancel(ctx)
	defer stopListening()
	listener := make(chan error, 1)
	go func() { listener <- notifications.Run(listening, 25*time.Millisecond) }()
	var blocked atomic.Bool
	revoked := errors.New("original query grant revoked")
	beginSubscription := func(after *int64) (context.CancelFunc, <-chan realtime.Delivery, <-chan error) {
		live, closeStream := context.WithCancel(ctx)
		t.Cleanup(closeStream)
		deliveries := make(chan realtime.Delivery, 1)
		finish := make(chan error, 1)
		go func() {
			finish <- store.Subscribe(live, notifications, realtime.Subscription{
				Key: key, Query: query, Relations: []realtime.Relation{relation}, After: after, PageSize: 2, DeliveryLimit: time.Second,
			}, func(context.Context) error {
				if blocked.Load() {
					return revoked
				}
				return nil
			}, func(call context.Context) (*pgx.Conn, error) {
				return pgx.ConnectConfig(call, readConfig.Copy())
			}, func(call context.Context, result realtime.Delivery) error {
				select {
				case deliveries <- result:
					return nil
				case <-call.Done():
					return call.Err()
				}
			})
		}()
		return closeStream, deliveries, finish
	}
	checkDelivery := func(deliveries <-chan realtime.Delivery, finish <-chan error, pos int64, snapshot bool) {
		t.Helper()
		select {
		case result := <-deliveries:
			if result.Position != pos || result.Snapshot != snapshot || !json.Valid(result.Data) {
				t.Fatalf("subscription result %+v, expected position %d, snapshot %v", result, pos, snapshot)
			}
		case err := <-finish:
			t.Fatalf("subscription ended before delivering position %d: %v", pos, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	closeStream, deliveries, finish := beginSubscription(nil)
	checkDelivery(deliveries, finish, 0, true)
	if _, _, err := notifications.Watch(key.ID); !errors.Is(err, realtime.ErrSubscriptionCapacity) {
		t.Fatalf("subscription limit did not apply: %v", err)
	}
	openSQL := func(ctx context.Context, _ realtime.Lease) (*pgx.Conn, error) {
		cfg := cfg.ConnConfig.Copy()
		cfg.User = "rt_wire_worker"
		return pgx.ConnectConfig(ctx, cfg)
	}
	limits := realtime.WorkerLimits{LeaseDuration: 3 * time.Second, FeedbackInterval: 25 * time.Millisecond,
		MaxFrameBytes: 128 * 1024, MaxWALBytes: 16 << 20}
	follow := func() (context.CancelFunc, <-chan error) {
		t.Helper()
		active, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- store.RunSource(active, key, limits, openSQL, open) }()
		t.Cleanup(stop)
		return stop, done
	}
	observe := func(cursor int64, done <-chan error) realtime.Invalidation {
		t.Helper()
		for {
			batch, err := store.ReadAfter(ctx, key, cursor, 2)
			must(err)
			var confirmed string
			must(sql.QueryRow(ctx, "SELECT confirmed_flush_lsn::text FROM pg_replication_slots WHERE slot_name='rt_worker_slot'").Scan(&confirmed))
			if len(batch) == 1 && batch[0].EndLSN.String() == confirmed {
				return batch[0]
			}
			select {
			case err := <-done:
				t.Fatalf("worker exited before a committed invalidation: %v", err)
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	stop, done := follow()
	exec("INSERT INTO public.rt_worker_events VALUES (1)")
	a := observe(0, done)
	if a.Position != 1 || len(a.Relations) != 1 || a.Relations[0] != relation {
		t.Fatalf("first durable invalidation: %+v", a)
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, 2*time.Second)
	note, err := notification.WaitForNotification(waitCtx)
	cancelWait()
	must(err)
	if note.Channel != "realtime_invalidations" || note.Payload != key.ID {
		t.Fatalf("notification differs from the committed source: %+v", note)
	}
	readSnapshot(1, 1)
	checkDelivery(deliveries, finish, 1, false)
	closeStream()
	if err := <-finish; !errors.Is(err, context.Canceled) {
		t.Fatalf("subscription cancellation: %v", err)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker cancellation: %v", err)
	}
	stop, done = follow()
	closeStream, deliveries, finish = beginSubscription(&a.Position)
	exec("INSERT INTO public.rt_worker_events VALUES (2)")
	b := observe(a.Position, done)
	if b.Position != 2 || b.LSN < a.EndLSN {
		t.Fatalf("worker did not resume the original logical slot: %+v %+v", a, b)
	}
	readSnapshot(2, 2)
	checkDelivery(deliveries, finish, 2, false)
	excess := query
	excess.MaxRows = 1
	if _, err := store.Snapshot(ctx, key, reader, excess); !errors.Is(err, realtime.ErrResultBudget) {
		t.Fatalf("over-budget query was delivered: %v", err)
	}
	write := query
	write.Statement, write.Params = "INSERT INTO public.rt_worker_events VALUES(7) RETURNING id", nil
	exec("GRANT INSERT ON public.rt_worker_events TO rt_wire_reader")
	if _, err := store.Snapshot(ctx, key, reader, write); !errors.Is(err, realtime.ErrQueryRejected) {
		t.Fatalf("read-only transaction admitted a write: %v", err)
	}
	exec("REVOKE INSERT ON public.rt_worker_events FROM rt_wire_reader")
	readSnapshot(2, 2)
	blocked.Store(true)
	exec("INSERT INTO public.rt_worker_events VALUES (3)")
	select {
	case err := <-finish:
		if !errors.Is(err, revoked) {
			t.Fatalf("revoked subscription ended with %v", err)
		}
	case result := <-deliveries:
		t.Fatalf("revoked subscription received new data: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closeStream()
	stopListening()
	if err := <-listener; !errors.Is(err, context.Canceled) {
		t.Fatalf("notification listener exit: %v", err)
	}
	must(store.Retire(ctx, key))
	if _, err := store.Snapshot(ctx, key, reader, query); !errors.Is(err, realtime.ErrSourceUnavailable) {
		t.Fatalf("retired source snapshot: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, realtime.ErrLeaseLost) {
			t.Fatalf("retirement did not cancel the worker: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	exec("SELECT pg_drop_replication_slot('rt_worker_slot')")
	t.Logf("PostgreSQL %s resumed the durable source across two worker leases at %s and %s",
		sql.PgConn().ParameterStatus("server_version"), a.EndLSN, b.EndLSN)
}
