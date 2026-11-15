package realtime

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestInvalidPgoutputFrames(t *testing.T) {
	d, err := newDecoder([]Relation{{Schema: "public", Name: "c3_alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		nil,
		{'T', 255, 255, 255, 255, 0},
		{'I', 0, 0, 0, 1, 'N', 0, 1, 't', 255, 255, 255, 255},
		{'U', 0, 0, 0, 1, 'K', 0, 1, 'n'},
		{'D', 0, 0, 0, 1, 'N', 0, 1, 'n'},
		{'I', 0, 0, 0, 1, 'N', 0, 1, 'n', 0},
		{'R', 0, 0, 0, 1, 'n', 0, 'r', 0, 'd', 0, 1},
		{'M', 0, 0, 0, 0},
	} {
		if got, err := d.acceptFrame(data); err == nil || got != nil {
			t.Fatalf("frame %x: got %v, %v", data, got, err)
		}
	}
	if d.tx != nil || len(d.byID) != 0 {
		t.Fatal("invalid frames mutated decoder state")
	}
}

func TestPgoutputWire(t *testing.T) {
	url := os.Getenv("NEON_REALTIME_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("explicit disposable PostgreSQL cluster required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database != "realtime_wire_test" {
		t.Fatal("integration test requires a disposable realtime_wire_test database")
	}
	db, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	exec := func(sql string) {
		t.Helper()
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE ROLE rt_wire_client LOGIN REPLICATION;
		CREATE ROLE rt_wire_denied LOGIN;
		CREATE TABLE c3_alpha(id int PRIMARY KEY, note text);
		CREATE TABLE c3_beta(id int PRIMARY KEY);
		CREATE TABLE c3_hidden(id int PRIMARY KEY);
		ALTER TABLE c3_alpha REPLICA IDENTITY FULL;
		CREATE PUBLICATION c3_wire_pub FOR TABLE public.c3_beta, public.c3_alpha;`)
	alpha, beta := Relation{"public", "c3_alpha"}, Relation{"public", "c3_beta"}
	authorized := []Relation{alpha, beta}
	if err := VerifyPublication(ctx, db, "c3_wire_pub", authorized); err != nil {
		t.Fatal(err)
	}
	assertChanged := func(name string, relations []Relation) {
		t.Helper()
		if err := VerifyPublication(ctx, db, name, relations); !errors.Is(err, ErrPublicationChanged) {
			t.Fatalf("publication %s accepted with mismatched coverage: %v", name, err)
		}
	}
	assertChanged("c3_wire_pub", []Relation{alpha})
	exec(`CREATE PUBLICATION c3_wire_filter FOR TABLE public.c3_alpha WHERE (id > 0), public.c3_beta;
		CREATE PUBLICATION c3_wire_partial FOR TABLE public.c3_alpha, public.c3_beta WITH (publish='insert');
		CREATE PUBLICATION c3_wire_schema FOR TABLES IN SCHEMA public;`)
	for _, name := range []string{"c3_wire_filter", "c3_wire_partial", "c3_wire_schema", "c3_wire_missing"} {
		assertChanged(name, authorized)
	}
	exec("DROP PUBLICATION c3_wire_filter, c3_wire_partial, c3_wire_schema")
	var initial string
	if err := db.QueryRow(ctx, "SELECT lsn::text FROM pg_create_logical_replication_slot('c3_wire_slot','pgoutput')").Scan(&initial); err != nil {
		t.Fatal(err)
	}
	start, err := pglogrepl.ParseLSN(initial)
	if err != nil {
		t.Fatal(err)
	}

	connect := func(user string) (*pgconn.PgConn, error) {
		replication := cfg.Config.Copy()
		replication.User = user
		replication.RuntimeParams["replication"] = "database"
		return pgconn.ConnectConfig(ctx, replication)
	}
	if denied, err := connect("rt_wire_denied"); err == nil {
		_ = denied.Close(ctx)
		t.Fatal("non-replication role opened a replication connection")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatal(err)
		}
	}
	open := func() *pgconn.PgConn {
		t.Helper()
		conn, err := connect("rt_wire_client")
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	conn := open()
	identity, err := pglogrepl.IdentifySystem(ctx, conn)
	_ = conn.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source := Source{SystemID: identity.SystemID, Timeline: identity.Timeline, Database: cfg.Database,
		Slot: "c3_wire_slot", Publication: "c3_wire_pub", ConfirmedLSN: start, Relations: []Relation{alpha, beta},
		MaxFrameBytes: 128 * 1024, FeedbackInterval: 25 * time.Millisecond}
	lease := Lease{Key: SourceKey{Scope: Scope{Database: cfg.Database}}, Slot: source.Slot, ConfirmedLSN: start}
	if err := checkSlot(ctx, db, lease, 16<<20, false); err != nil {
		t.Fatalf("original slot preflight: %v", err)
	}
	oldLease := lease
	oldLease.ConfirmedLSN = start - 1
	if err := checkSlot(ctx, db, oldLease, 16<<20, false); !errors.Is(err, ErrSlotAhead) {
		t.Fatalf("slot advanced past the stale lease cursor: %v", err)
	}
	if err := checkSlot(ctx, db, lease, 1, false); !errors.Is(err, ErrSlotWALBudget) {
		t.Fatalf("original slot exceeded its test WAL allowance: %v", err)
	}
	run := func(runCtx context.Context, source Source, fn func(context.Context, Commit) error) error {
		t.Helper()
		return Consume(runCtx, open(), source, fn)
	}
	wrong := source
	wrong.Database = "another_database"
	called := false
	if err := run(ctx, wrong, func(context.Context, Commit) error { called = true; return nil }); err == nil || called {
		t.Fatal("stream accepted a mismatched source")
	}

	exec("BEGIN; INSERT INTO c3_alpha VALUES(999, 'rolled back'); ROLLBACK;")
	exec("INSERT INTO c3_hidden VALUES(1);")
	exec("BEGIN; INSERT INTO c3_beta VALUES(1); INSERT INTO c3_alpha VALUES(1, 'one'), (2, 'two'); COMMIT;")
	failure := errors.New("simulated persistence failure")
	var first Commit
	if err := run(ctx, source, func(_ context.Context, c Commit) error {
		first = c
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("first stream: %v", err)
	}
	if !slices.Equal(first.Relations, []Relation{alpha, beta}) || first.LSN < start || first.EndLSN <= first.LSN {
		t.Fatalf("unexpected first commit: %+v", first)
	}
	slot := func() pglogrepl.LSN {
		t.Helper()
		var value string
		if err := db.QueryRow(ctx, "SELECT confirmed_flush_lsn::text FROM pg_replication_slots WHERE slot_name = 'c3_wire_slot'").Scan(&value); err != nil {
			t.Fatal(err)
		}
		lsn, err := pglogrepl.ParseLSN(value)
		if err != nil {
			t.Fatal(err)
		}
		return lsn
	}
	if got := slot(); got != start {
		t.Fatalf("failed persistence changed confirmed LSN from %s to %s", start, got)
	}

	consumeAndConfirm := func(s Source, expected []Commit) []Commit {
		t.Helper()
		stream, stop := context.WithCancel(ctx)
		defer stop()
		result := make(chan error, 1)
		received := make(chan Commit, len(expected))
		replication := open()
		go func() {
			result <- Consume(stream, replication, s, func(call context.Context, c Commit) error {
				select {
				case received <- c:
					return nil
				case <-call.Done():
					return call.Err()
				}
			})
		}()
		got := make([]Commit, 0, len(expected))
		for _, want := range expected {
			select {
			case err := <-result:
				t.Fatalf("stream ended before commit: %v", err)
			case c := <-received:
				if (want.EndLSN != 0 && (c.LSN != want.LSN || c.EndLSN != want.EndLSN)) ||
					!slices.Equal(c.Relations, want.Relations) {
					t.Fatalf("commit=%+v, expected %+v", c, want)
				}
				got = append(got, c)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		last := got[len(got)-1].EndLSN
		for slot() < last {
			select {
			case err := <-result:
				t.Fatalf("stream ended before feedback: %v", err)
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if gotLSN := slot(); gotLSN != last {
			t.Fatalf("slot confirmed %s, last persisted %s", gotLSN, last)
		}
		select {
		case duplicate := <-received:
			t.Fatalf("unexpected commit: %+v", duplicate)
		default:
		}
		stop()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("stream cancellation: %v", err)
		}
		return got
	}
	consumeAndConfirm(source, []Commit{first})
	source.ConfirmedLSN = first.EndLSN
	exec("BEGIN; UPDATE c3_alpha SET note = 'changed' WHERE id=1; DELETE FROM c3_alpha WHERE id=2; COMMIT;")
	exec("TRUNCATE c3_beta, c3_alpha;")
	confirmed := consumeAndConfirm(source, []Commit{{Relations: []Relation{alpha}}, {Relations: []Relation{alpha, beta}}})
	if confirmed[0].LSN < first.EndLSN || confirmed[1].LSN < confirmed[0].EndLSN {
		t.Fatalf("reconnect delivered commits out of order: %+v", confirmed)
	}
	if err := checkSlot(ctx, db, Lease{Key: lease.Key, Slot: lease.Slot, ConfirmedLSN: confirmed[1].EndLSN}, 16<<20, false); err != nil {
		t.Fatalf("original slot did not settle after ending replication: %v", err)
	}
	exec("SELECT pg_drop_replication_slot('c3_wire_slot')")
	t.Logf("PostgreSQL %s confirmed commit %s through %s with recovery", db.PgConn().ParameterStatus("server_version"), first.LSN, confirmed[1].EndLSN)
}
