package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDataAPINativeSQLIntegration(t *testing.T) {
	databaseURL := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("dedicated disposable PostgreSQL required for native Data API gate")
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil || config.Database != "control_ci" {
		t.Fatal("native Data API test must use disposable control_ci")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	p := dataAPITestPayload(t)
	p.BranchID = "br_" + hex.EncodeToString(suffix[:])
	p.Spec.Schema = "app_test_" + hex.EncodeToString(suffix[:])
	p.Spec.Database = "control_ci"
	verifier, err := scramVerifier("disposable-service-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"CREATE SCHEMA " + pgQuote(p.Spec.Schema),
		"CREATE TABLE " + pgQuote(p.Spec.Schema) + ".notes(id text PRIMARY KEY, owner_id text NOT NULL)",
		"INSERT INTO " + pgQuote(p.Spec.Schema) + ".notes VALUES ('alice-1','alice'),('bob-1','bob')",
	} {
		if _, err = conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	apply := func() error {
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if e = applyDataAPISQLTx(ctx, tx, p, verifier); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	t.Run("createrole_does_not_replace_database_delegation", func(t *testing.T) {
		provisioner := "setup_" + hex.EncodeToString(suffix[:])
		if _, e := conn.Exec(ctx, "CREATE ROLE "+pgQuote(provisioner)+" CREATEROLE CREATEDB"); e != nil {
			t.Fatal(e)
		}
		tx, e := conn.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, "SET LOCAL ROLE "+pgQuote(provisioner)); e != nil {
			t.Fatal(e)
		}
		var prerequisite dataAPIPrerequisiteError
		if e = applyDataAPISQLTx(ctx, tx, p, verifier); !errors.As(e, &prerequisite) || prerequisite.code != "data_api_database_grant_required" {
			t.Fatal("missing delegation was not reported before role creation")
		}
	})
	t.Run("rejects_schema_without_rls", func(t *testing.T) {
		if err = apply(); err == nil {
			t.Fatal("unprotected table enabled")
		}
	})
	for _, sql := range []string{
		"ALTER TABLE " + pgQuote(p.Spec.Schema) + ".notes ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + pgQuote(p.Spec.Schema) + ".notes FORCE ROW LEVEL SECURITY",
		"CREATE POLICY owner_rows ON " + pgQuote(p.Spec.Schema) + ".notes USING(owner_id=current_setting('request.jwt.claims',true)::jsonb->>'sub') WITH CHECK(owner_id=current_setting('request.jwt.claims',true)::jsonb->>'sub')",
	} {
		if _, err = conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("replay_sql_and_verify_nonprivileged_membership", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			if err = apply(); err != nil {
				t.Fatal(err)
			}
		}
		var configured bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_db_role_setting WHERE setrole=(SELECT oid FROM pg_roles WHERE rolname=$1) AND setdatabase=0 AND 'idle_session_timeout=5s'=ANY(setconfig))`, dataAPILogin(p.BranchID)).Scan(&configured); err != nil || !configured {
			t.Fatal("owned service idle timeout not applied on replay")
		}
	})
	t.Run("actual_request_role_rls_read_and_write", func(t *testing.T) {
		tx, e := conn.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, "SET LOCAL ROLE "+pgQuote(dataAPIRole(p.BranchID))); e != nil {
			t.Fatal(e)
		}
		claims, _ := json.Marshal(map[string]string{"iss": "neon-control-data-api", "aud": p.Spec.Audience, "branch_id": p.BranchID, "sub": "alice"})
		if _, e = tx.Exec(ctx, `SELECT set_config('request.jwt.claims',$1,true)`, string(claims)); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, "SELECT "+pgQuote(dataAPIGuardSchema(p.BranchID))+".check_request()"); e != nil {
			t.Fatal(e)
		}
		var ids []string
		if e = tx.QueryRow(ctx, "SELECT array_agg(id ORDER BY id) FROM "+pgQuote(p.Spec.Schema)+".notes").Scan(&ids); e != nil {
			t.Fatal(e)
		}
		if len(ids) != 1 || ids[0] != "alice-1" {
			t.Fatal("subject crossed row boundary")
		}
		if _, e = tx.Exec(ctx, "INSERT INTO "+pgQuote(p.Spec.Schema)+".notes VALUES ('forged','bob')"); e == nil {
			t.Fatal("forged row accepted")
		}
	})
	t.Run("guard_rejects_missing_issuer", func(t *testing.T) {
		tx, e := conn.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, "SET LOCAL ROLE "+pgQuote(dataAPIRole(p.BranchID))); e != nil {
			t.Fatal(e)
		}
		claims, _ := json.Marshal(map[string]string{"aud": p.Spec.Audience, "branch_id": p.BranchID, "sub": "alice"})
		if _, e = tx.Exec(ctx, `SELECT set_config('request.jwt.claims',$1,true)`, string(claims)); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, "SELECT "+pgQuote(dataAPIGuardSchema(p.BranchID))+".check_request()"); e == nil {
			t.Fatal("missing issuer accepted")
		}
	})
	t.Run("guard_blocks_rls_drift_after_provisioning", func(t *testing.T) {
		tx, e := conn.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, "ALTER TABLE "+pgQuote(p.Spec.Schema)+".notes NO FORCE ROW LEVEL SECURITY"); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, "SET LOCAL ROLE "+pgQuote(dataAPIRole(p.BranchID))); e != nil {
			t.Fatal(e)
		}
		claims, _ := json.Marshal(map[string]string{"iss": "neon-control-data-api", "aud": p.Spec.Audience, "branch_id": p.BranchID, "sub": "alice"})
		if _, e = tx.Exec(ctx, `SELECT set_config('request.jwt.claims',$1,true)`, string(claims)); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, "SELECT "+pgQuote(dataAPIGuardSchema(p.BranchID))+".check_request()"); e == nil {
			t.Fatal("RLS drift silently exposed data")
		}
	})
	t.Run("rejects_unowned_role", func(t *testing.T) {
		if _, err = conn.Exec(ctx, "COMMENT ON ROLE "+pgQuote(dataAPIRole(p.BranchID))+" IS 'unowned'"); err != nil {
			t.Fatal(err)
		}
		if err = apply(); err == nil || !strings.Contains(err.Error(), "ownership") {
			t.Fatal("unowned role changed")
		}
	})
}
