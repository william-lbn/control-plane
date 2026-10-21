package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFailedEndpointRetryAndCleanupRace(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	schema := os.Getenv("NEON_V2_TEST_AUTH_SCHEMA") + "_cleanup_race"
	if !regexp.MustCompile(`^v2_auth_[a-z0-9_]+$`).MatchString(schema) || len(schema) > 63 {
		t.Fatal("isolated schema required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 6
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_cleanup_race','local','race','test',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,is_default,protected,state,created_at) VALUES('br_cleanup_race','prj_cleanup_race','main',repeat('a',32),true,true,'ready',now())`)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at) VALUES('ep_cleanup_race','prj_cleanup_race','br_cleanup_race','ep-race','neonvm','cp-race','cp-race','provisioning','cloud_admin','postgres',now(),now())`)
	original, _ := json.Marshal(createPayload{ProjectID: "prj_cleanup_race", BranchID: "br_cleanup_race", EndpointID: "ep_cleanup_race"})
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,retryable,finished_at) VALUES('op_cleanup_race_origin','prj_cleanup_race','endpoint','ep_cleanup_race','create_endpoint','failed','test','race',$1,true,now())`, original)
	exec(`UPDATE endpoints SET state='error' WHERE id='ep_cleanup_race'`)
	cleanup, _ := json.Marshal(endpointDeletionPayload{ProjectID: "prj_cleanup_race", BranchID: "br_cleanup_race", EndpointID: "ep_cleanup_race", FailedCreateOperationID: "op_cleanup_race_origin"})
	gate, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err = gate.Exec(ctx, `SELECT id FROM projects WHERE id='prj_cleanup_race' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		action string
		err    error
	}
	pids := make(chan int32, 2)
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, action := range []string{"cleanup", "retry"} {
		go func(action string) {
			tx, e := db.Begin(ctx)
			if e != nil {
				pids <- 0
				results <- result{action, e}
				return
			}
			defer tx.Rollback(ctx)
			var pid int32
			if e = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
				pids <- 0
				results <- result{action, e}
				return
			}
			pids <- pid
			<-start
			if action == "cleanup" {
				_, e = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('op_cleanup_race_delete','prj_cleanup_race','endpoint','ep_cleanup_race','cleanup_failed_endpoint','queued','test','race',$1)`, cleanup)
				if e == nil {
					_, e = tx.Exec(ctx, `UPDATE endpoints SET state='deleting',deletion_operation_id='op_cleanup_race_delete' WHERE id='ep_cleanup_race'`)
				}
			} else {
				_, e = tx.Exec(ctx, `UPDATE operations SET state='queued',finished_at=NULL WHERE id='op_cleanup_race_origin'`)
			}
			if e == nil {
				e = tx.Commit(ctx)
			}
			results <- result{action, e}
		}(action)
	}
	connections := []int32{<-pids, <-pids}
	if connections[0] == 0 || connections[1] == 0 {
		close(start)
		t.Fatal("race participants could not connect")
	}
	close(start)
	deadline := time.Now().Add(3 * time.Second)
	locked := 0
	for time.Now().Before(deadline) {
		if err = db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid=ANY($1::int[]) AND wait_event_type='Lock'`, connections).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if locked == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if locked != 2 {
		t.Fatal("both admissions did not reach the held parent lock", locked)
	}
	if err = gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	winners := 0
	winner := ""
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err == nil {
			winners++
			winner = r.action
			continue
		}
		var rejected *pgconn.PgError
		if !errors.As(r.err, &rejected) || rejected.Code != "23514" {
			t.Fatal("race lost to a deadlock or transport failure rather than lifecycle denial", r.action, r.err)
		}
	}
	var queued int
	var endpointState, originState string
	if err = db.QueryRow(ctx, `SELECT count(*) FROM operations WHERE resource_id='ep_cleanup_race' AND state='queued'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, `SELECT e.state,o.state FROM endpoints e JOIN operations o ON o.resource_id=e.id WHERE e.id='ep_cleanup_race' AND o.id='op_cleanup_race_origin'`).Scan(&endpointState, &originState); err != nil {
		t.Fatal(err)
	}
	if winners != 1 || queued != 1 || winner == "cleanup" && (endpointState != "deleting" || originState != "failed") || winner == "retry" && (endpointState != "error" || originState != "queued") {
		t.Fatal("both intents admitted or original failure rewritten", winner, winners, queued, endpointState, originState)
	}
	t.Logf("real PostgreSQL parent-lock race winner=%s; exactly one intent admitted", winner)
}
