package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func initializationHistoryFact(t *testing.T, ctx context.Context, binding *dsgaCreationBinding, boot string) (string, string) {
	t.Helper()
	var raw, xmin string
	if err := binding.server.db.QueryRow(ctx, `SELECT receipt::text,xmin::text FROM dsga_creation_initialization_observations WHERE operation_id=$1 AND pg_boot_id=$2`, binding.operationID, boot).Scan(&raw, &xmin); err != nil {
		t.Fatal(err)
	}
	return raw, xmin
}

func TestDSGAInitializationHistoryAlwaysObservesFreshAndPreservesFirstBootFact(t *testing.T) {
	ctx, binding, _, _, _, client, state, original := initializationBindingFixture(t)
	if _, err := binding.observeAndRecordInitializationWithHTTP(ctx, client); err != nil {
		t.Fatal(err)
	}
	before, xmin := initializationHistoryFact(t, ctx, binding, "17")
	// 合同夹具模拟稍后PG准入已变：不能覆盖首份审计，也不能把首份旧false
	// 回给调用方冒充本次HTTP结果。本测试不实际授予任何PG或VM许可。
	current := original
	current.Observed.AdmissionOpen = true
	current.Observed.AdmissionRevision = 1
	raw, _ := json.Marshal(current)
	state.mu.Lock()
	state.raw = raw
	state.mu.Unlock()
	fresh, err := binding.observeAndRecordInitializationWithHTTP(ctx, client)
	if err != nil || !fresh.Observed.AdmissionOpen || fresh.Observed.AdmissionRevision != 1 {
		t.Fatal("history was substituted for current observation")
	}
	after, afterXmin := initializationHistoryFact(t, ctx, binding, "17")
	if before != after || xmin != afterXmin {
		t.Fatal("repeat observation rewrote first boot fact")
	}
	// 新boot另记，不覆盖旧boot，也不将原生不可用恢复成数据库缓存成功。
	current.Observed.Instance.PGBootID = 18
	raw, _ = json.Marshal(current)
	state.mu.Lock()
	state.raw = raw
	state.mu.Unlock()
	if _, err := binding.observeAndRecordInitializationWithHTTP(ctx, client); err != nil {
		t.Fatal(err)
	}
	_, _ = initializationHistoryFact(t, ctx, binding, "18")
	state.mu.Lock()
	state.raw = []byte(`{"state":"unavailable"}`)
	state.mu.Unlock()
	if fact, err := binding.observeAndRecordInitializationWithHTTP(ctx, client); err == nil || fact.ProtocolVersion != 0 || fact.Observed.Instance.PGBootID != 0 {
		t.Fatal("native failure reused historical success")
	}

	var count int
	if err := binding.server.db.QueryRow(ctx, `SELECT count(*) FROM dsga_creation_initialization_observations WHERE operation_id=$1`, binding.operationID).Scan(&count); err != nil || count != 2 {
		t.Fatal("boot history lost or fabricated")
	}
}

func TestDSGAInitializationHistoryDatabaseGuardsAndForwardReplay(t *testing.T) {
	ctx, binding, _, _, _, client, _, original := initializationBindingFixture(t)
	if _, err := binding.observeAndRecordInitializationWithHTTP(ctx, client); err != nil {
		t.Fatal(err)
	}
	before, xmin := initializationHistoryFact(t, ctx, binding, "17")
	for range 2 {
		if err := migrate(ctx, binding.server.db); err != nil {
			t.Fatal(err)
		}
	}
	after, afterXmin := initializationHistoryFact(t, ctx, binding, "17")
	if before != after || xmin != afterXmin {
		t.Fatal("migration replay rewrote initialization fact")
	}
	for _, query := range []string{`UPDATE dsga_creation_initialization_observations SET pg_boot_id='19' WHERE operation_id=$1`, `UPDATE dsga_creation_initialization_observations SET observed_at=observed_at WHERE operation_id=$1`, `DELETE FROM dsga_creation_initialization_observations WHERE operation_id=$1`} {
		_, err := binding.server.db.Exec(ctx, query, binding.operationID)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatal("original initialization history mutable")
		}
	}
	for name, mutate := range map[string]func(*dsgaInitializationResponse){
		"permit":         func(f *dsgaInitializationResponse) { f.VMShrinkPermit = true },
		"operation":      func(f *dsgaInitializationResponse) { f.OperationUUID = "replacement" },
		"tenant":         func(f *dsgaInitializationResponse) { f.TenantID = "foreign" },
		"startup":        func(f *dsgaInitializationResponse) { f.Startup.NonBufferReserveBytes++ },
		"authentication": func(f *dsgaInitializationResponse) { f.AuthenticationPolicyVerified = false },
	} {
		t.Run(name, func(t *testing.T) {
			bad := original
			bad.Observed.Instance.PGBootID = 19
			mutate(&bad)
			raw, _ := json.Marshal(bad)
			_, err := binding.server.db.Exec(ctx, `INSERT INTO dsga_creation_initialization_observations(operation_id,pg_boot_id,vm_uid,config_uid,credentials_uid,management_service_uid,receipt)
 SELECT operation_id,'19',vm_uid,config_uid,credentials_uid,management_service_uid,$2 FROM dsga_creation_initialization_observations WHERE operation_id=$1 AND pg_boot_id='17'`, binding.operationID, raw)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
				t.Fatal("database accepted mismatched initialization proof")
			}
		})
	}
}

func TestDSGAInitializationHistoryUnsignedBootAndFailedCommitRecovery(t *testing.T) {
	ctx, binding, _, _, _, client, state, original := initializationBindingFixture(t)
	original.Observed.Instance.PGBootID = 18446744073709551615
	raw, _ := json.Marshal(original)
	state.mu.Lock()
	state.raw = raw
	state.mu.Unlock()
	if _, err := binding.server.db.Exec(ctx, `CREATE FUNCTION fail_initialization_history() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'test history failure'; END$$;
 CREATE TRIGGER fail_initialization_history BEFORE INSERT ON dsga_creation_initialization_observations FOR EACH ROW EXECUTE FUNCTION fail_initialization_history()`); err != nil {
		t.Fatal(err)
	}
	if fact, err := binding.observeAndRecordInitializationWithHTTP(ctx, client); err == nil || fact.ProtocolVersion != 0 {
		t.Fatal("failed SQL recording returned an accepted observation")
	}
	var count int
	if err := binding.server.db.QueryRow(ctx, `SELECT count(*) FROM dsga_creation_initialization_observations WHERE operation_id=$1`, binding.operationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed recording partially committed")
	}
	if _, err := binding.server.db.Exec(ctx, `DROP TRIGGER fail_initialization_history ON dsga_creation_initialization_observations`); err != nil {
		t.Fatal(err)
	}
	if _, err := binding.observeAndRecordInitializationWithHTTP(ctx, client); err != nil {
		t.Fatal(err)
	}
	if state.calls.Load() != 2 {
		t.Fatal("SQL recovery bypassed fresh native observation")
	}
	_, _ = initializationHistoryFact(t, ctx, binding, "18446744073709551615")
	_, err := binding.server.db.Exec(ctx, `INSERT INTO dsga_creation_initialization_observations(operation_id,pg_boot_id,vm_uid,config_uid,credentials_uid,management_service_uid,receipt)
 SELECT operation_id,$2,vm_uid,config_uid,credentials_uid,management_service_uid,jsonb_set(receipt,'{observed,instance,pg_boot_id}',to_jsonb($2::text))
 FROM dsga_creation_initialization_observations WHERE operation_id=$1`, binding.operationID, "18446744073709551616")
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
		t.Fatal("boot outside uint64 range was stored or truncated")
	}
}
