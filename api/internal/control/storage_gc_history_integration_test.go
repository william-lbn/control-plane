package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
)

func storageGCHistoryNativePGFixture(t *testing.T) (*settingsPGFixture, storageGCHistoryScope) {
	t.Helper()
	f := newSettingsPGFixture(t)
	scope := storageGCHistoryScope{TenantID: strings.Repeat("1", 32), TimelineID: strings.Repeat("2", 32), Database: f.db}
	if _, err := f.conn.Exec(f.ctx, `SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, scope.TenantID, scope.TimelineID); err != nil {
		t.Fatal(err)
	}
	if err := f.conn.QueryRow(f.ctx, `SELECT oid FROM pg_database WHERE datname=current_database()`).Scan(&scope.DatabaseOID); err != nil {
		t.Fatal(err)
	}
	return f, scope
}
func storageGCHistoryInsert(ctx context.Context, conn *pgx.Conn, key, blob string) error {
	_, err := conn.Exec(ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('settings-bucket',$1,$2,repeat('a',64),4,'text/plain')`, key, blob)
	return err
}

func TestStorageGCHistoryPostgresEpochTransitionsAndDurableQuarantine(t *testing.T) {
	f, scope := storageGCHistoryNativePGFixture(t)
	key := f.p.ProjectID + "/blob_" + strings.Repeat("a", 24)
	id, err := installStorageGCHistoryEpochs(f.ctx, f.conn, f.p.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if replay, e := installStorageGCHistoryEpochs(f.ctx, f.conn, f.p.ProjectID); e != nil || replay != id {
		t.Fatal("explicit installation did not attest/reuse original authority", e)
	}
	var format int
	if err = f.conn.QueryRow(f.ctx, `SELECT max(format_version) FROM neon_storage.installations`).Scan(&format); err != nil || format != 7 {
		t.Fatal("explicit GC authority changed native format 7", format, err)
	}
	observe := func() storageGCHistoryDirectory {
		t.Helper()
		lease, e := openStorageGCHistoryDirectory(f.ctx, f.conn, scope, f.p.ProjectID, key)
		if e != nil {
			t.Fatal(e)
		}
		defer lease.Close(context.Background())
		return lease.Observation()
	}
	before := observe()
	if before.Epoch != 0 || before.ObjectReferences != 0 {
		t.Fatal(before)
	}
	if err = storageGCHistoryInsert(f.ctx, f.conn, "first-reference", key); err != nil {
		t.Fatal(err)
	}
	if _, err = f.conn.Exec(f.ctx, `INSERT INTO neon_storage.objects SELECT bucket,'copied-reference',blob_key,sha256,size,content_type,updated_at,metadata,tags,response_headers,part_layout,checksums,checksum_policy FROM neon_storage.objects WHERE key='first-reference'`); err != nil {
		t.Fatal("COPY-style pointer sharing bypassed epoch", err)
	}
	shared := observe()
	if shared.ObjectReferences != 2 || shared.Epoch != 2 {
		t.Fatal("shared refs were not observed", shared)
	}
	if _, err = f.conn.Exec(f.ctx, `DELETE FROM neon_storage.objects WHERE key IN ('first-reference','copied-reference')`); err != nil {
		t.Fatal(err)
	}
	after := observe()
	if after.ObjectReferences != 0 || after.Epoch != 4 {
		t.Fatal("transient publish/delete was invisible", after)
	}
	// Aborted transactions do not create readable historical references and
	// their epoch write must roll back atomically with their pointer write.
	tx, err := f.conn.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `INSERT INTO neon_storage.objects(bucket,key,blob_key,sha256,size,content_type) VALUES('settings-bucket','rolled-back',$1,repeat('b',64),4,'text/plain')`, key); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if observed := observe(); observed.Epoch != 4 || observed.ObjectReferences != 0 {
		t.Fatal("rollback split pointer and epoch", observed)
	}
	lease, err := openStorageGCHistoryDirectory(f.ctx, f.conn, scope, f.p.ProjectID, key)
	if err != nil {
		t.Fatal(err)
	}
	retirement := "sgh_" + strings.Repeat("c", 24)
	if err = lease.Quarantine(f.ctx, retirement); err != nil {
		t.Fatal(err)
	}
	if err = storageGCHistoryInsert(f.ctx, f.conn, "after-lock-disconnect", key); err == nil {
		t.Fatal("committed quarantine allowed pointer resurrection after lock release")
	}
	if _, err = f.conn.Exec(f.ctx, `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	if err = storageGCHistoryInsert(f.ctx, f.conn, "replica-bypass", key); err == nil {
		t.Fatal("ALWAYS trigger was bypassed by replica mode")
	}
	if _, err = f.conn.Exec(f.ctx, `SET session_replication_role=origin`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`DELETE FROM neon_storage.gc_history_retirements`, `UPDATE neon_storage.gc_history_epochs SET epoch=1`, `TRUNCATE neon_storage.objects`, `TRUNCATE neon_storage.gc_history_epochs`} {
		if _, e := f.conn.Exec(f.ctx, query); e == nil {
			t.Fatal("epoch/retirement evidence bypassed", query)
		}
	}
	if observed := observe(); observed.RetirementID != retirement || observed.Epoch != 4 {
		t.Fatal("durable retirement did not survive lock transaction", observed)
	}
}

func TestStorageGCHistoryPostgresNativeLocksPartsPrivilegesAndAttestation(t *testing.T) {
	f, scope := storageGCHistoryNativePGFixture(t)
	if _, err := f.conn.Exec(f.ctx, `CREATE TABLE neon_storage.object_versions(blob_key text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := installStorageGCHistoryEpochs(f.ctx, f.conn, f.p.ProjectID); err == nil {
		t.Fatal("unknown version/history physical reference table was silently adopted")
	}
	if _, err := f.conn.Exec(f.ctx, `DROP TABLE neon_storage.object_versions`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.Exec(f.ctx, `ALTER TABLE neon_storage.objects ENABLE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	if _, err := installStorageGCHistoryEpochs(f.ctx, f.conn, f.p.ProjectID); err == nil {
		t.Fatal("RLS-filtered reference tables were silently adopted")
	}
	if _, err := f.conn.Exec(f.ctx, `ALTER TABLE neon_storage.objects DISABLE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	role := "gc_history_writer_" + newID("")[:16]
	if _, err := f.conn.Exec(f.ctx, "CREATE ROLE "+pgQuote(role)+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.conn.Exec(context.Background(), `RESET ROLE`)
		_, _ = f.conn.Exec(context.Background(), "DROP OWNED BY "+pgQuote(role))
		if _, err := f.conn.Exec(context.Background(), "DROP ROLE "+pgQuote(role)); err != nil {
			t.Error(err)
		}
	})
	if _, err := f.conn.Exec(f.ctx, "GRANT USAGE ON SCHEMA neon_storage TO "+pgQuote(role)+";GRANT SELECT,INSERT,UPDATE,DELETE ON neon_storage.objects,neon_storage.multipart_parts,neon_storage.multipart_uploads TO "+pgQuote(role), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	if _, err := installStorageGCHistoryEpochs(f.ctx, f.conn, f.p.ProjectID); err != nil {
		t.Fatal(err)
	}
	key := f.p.ProjectID + "/part_" + strings.Repeat("d", 24)
	if _, err := f.conn.Exec(f.ctx, `INSERT INTO neon_storage.multipart_uploads(id,branch_id,principal,bucket,key,content_type,state) VALUES('history-upload',$1,'disposable-owner','settings-bucket','pending','text/plain','aborted')`, f.p.BranchID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.Exec(f.ctx, "SET ROLE "+pgQuote(role)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.Exec(f.ctx, `INSERT INTO neon_storage.multipart_parts(upload_id,part_number,blob_key,sha256,etag,size) VALUES('history-upload',1,$1,repeat('d',64),'synthetic-etag',4)`, key); err != nil {
		t.Fatal("restricted writer could not atomically record a staging epoch", err)
	}
	if _, err := f.conn.Exec(f.ctx, `UPDATE neon_storage.gc_history_epochs SET epoch=1`); err == nil {
		t.Fatal("restricted writer gained GC epoch mutation")
	}
	if _, err := f.conn.Exec(f.ctx, `ALTER TABLE neon_storage.multipart_parts DISABLE TRIGGER gc_history_parts_changed`); err == nil {
		t.Fatal("restricted writer disabled instrumentation")
	}
	if _, err := f.conn.Exec(f.ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	lease, err := openStorageGCHistoryDirectory(f.ctx, f.conn, scope, f.p.ProjectID, key)
	if err != nil {
		t.Fatal(err)
	}
	if value := lease.Observation(); value.MultipartReferences != 1 || value.Epoch != 1 {
		t.Fatal("aborted staging part was dropped from references", value)
	}
	other, err := pgx.ConnectConfig(f.ctx, f.config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if _, err = other.Exec(f.ctx, `SET lock_timeout='100ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err = other.Exec(f.ctx, `DELETE FROM neon_storage.multipart_parts WHERE upload_id='history-upload'`); err == nil {
		t.Fatal("native SHARE lease did not block an independent pointer writer")
	}
	if err = lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = other.Exec(f.ctx, `DELETE FROM neon_storage.multipart_parts WHERE upload_id='history-upload'`); err != nil {
		t.Fatal("closing native lease left writer blocked", err)
	}
	if _, err = f.conn.Exec(f.ctx, `ALTER TABLE neon_storage.objects DISABLE TRIGGER gc_history_objects_changed`); err != nil {
		t.Fatal(err)
	}
	if _, err = openStorageGCHistoryDirectory(f.ctx, f.conn, scope, f.p.ProjectID, key); err == nil {
		t.Fatal("disabled reference trigger remained an attested authority")
	}
	if _, err = installStorageGCHistoryEpochs(f.ctx, f.conn, f.p.ProjectID); err == nil {
		t.Fatal("reinstall silently adopted altered instrumentation")
	}
}

func TestStorageGCHistoryPostgresFencedPipelineEvidenceAndFailureHolds(t *testing.T) {
	f := newStorageGCPGFixture(t)
	f.exec(`UPDATE projects SET tenant_id=$1 WHERE id=$2`, strings.Repeat("1", 32), f.project)
	key, id := f.allocation("a", "may_reference", 3600)
	f.backing.info = minio.ObjectInfo{Size: 4, ETag: "disposable-history-etag", UserMetadata: map[string]string{storageGCMetadata: id}}
	// This is a protocol model authority, NOT the unavailable production native
	// API. PostgreSQL here verifies the durable metadata/evidence state machine.
	native := &storageGCHistoryFakeNative{snapshot: storageGCHistoryModel()}
	source := &storageGCHistoryFakeSource{native: native}
	blocked, e := f.j.prepareHistory(f.ctx, f.project, key, f.actor, nil, nil)
	if !errors.Is(e, errStorageGCHistoryHeld) || blocked == "" || f.backing.deletes != 0 {
		t.Fatal("missing native authority permitted work", blocked, e)
	}
	baseline, e := f.j.prepareHistory(f.ctx, f.project, key, f.actor, native, source)
	if e != nil {
		t.Fatal(e)
	}
	native.snapshot.Directories[0].ObservationLSN = "0/3000"
	native.snapshot.Directories[0].Epoch += 2
	if state, e := f.j.executeHistory(f.ctx, baseline, f.actor, native, source); !errors.Is(e, errStorageGCHistoryHeld) || state != "held" || f.backing.deletes != 0 {
		t.Fatal("transient ref reappearance allowed DELETE", state, e)
	}
	native.snapshot.Directories[0].Epoch -= 2
	native.failRetirement = true
	if _, e = f.j.executeHistory(f.ctx, baseline, f.actor, native, source); e == nil || f.backing.deletes != 0 || source.quarantined != 0 {
		t.Fatal("unconfirmed global retirement permitted local deletion", e)
	}
	native.failRetirement = false
	source.failQuarantine = true
	if _, e = f.j.executeHistory(f.ctx, baseline, f.actor, native, source); e == nil || f.backing.deletes != 0 {
		t.Fatal("incomplete durable quarantine permitted DELETE", e)
	}
	source.failQuarantine = false
	f.backing.onRemove = func() {
		if !native.active || native.retirements == 0 || source.quarantined == 0 {
			t.Error("DELETE ran outside durable native retirement and scope quarantine")
		}
	}
	if state, e := f.j.executeHistory(f.ctx, baseline, f.actor, native, source); e != nil || state != "deleted" || f.backing.deletes != 1 {
		t.Fatal("fenced pipeline failed", state, e)
	}
	if state, e := f.j.executeHistory(f.ctx, baseline, f.actor, native, source); e != nil || state != "absent" || f.backing.deletes != 1 {
		t.Fatal("original history baseline replay repeated DELETE", state, e)
	}
	var retained, results int
	if e = f.s.db.QueryRow(f.ctx, `SELECT count(*),count(*) FILTER(WHERE kind='result') FROM storage_gc_history_evidence WHERE store_id=$1 AND blob_key=$2`, f.j.storeID, key).Scan(&retained, &results); e != nil || retained < 8 || results != 2 {
		t.Fatal("history evidence not append-only/complete", retained, results, e)
	}
	for _, query := range []string{`DELETE FROM storage_gc_history_evidence`, `UPDATE storage_gc_history_evidence SET outcome='deleted'`} {
		if _, e = f.s.db.Exec(f.ctx, query); e == nil {
			t.Fatal("history evidence was mutable", query)
		}
	}
	var state string
	if e = f.s.db.QueryRow(f.ctx, `SELECT state FROM storage_gc_allocations WHERE store_id=$1 AND blob_key=$2`, f.j.storeID, key).Scan(&state); e != nil || state != "may_reference" {
		t.Fatal("history collection demoted immutable prospective allocation", state, e)
	}
}

func TestStorageGCHistoryPostgresInterruptedDeleteKeepsIntentAndQuarantine(t *testing.T) {
	f := newStorageGCPGFixture(t)
	f.exec(`UPDATE projects SET tenant_id=$1 WHERE id=$2`, strings.Repeat("1", 32), f.project)
	key, id := f.allocation("b", "may_reference", 3600)
	f.backing.info = minio.ObjectInfo{Size: 4, ETag: "disposable-history-etag", UserMetadata: map[string]string{storageGCMetadata: id}}
	native := &storageGCHistoryFakeNative{snapshot: storageGCHistoryModel()}
	source := &storageGCHistoryFakeSource{native: native}
	baseline, e := f.j.prepareHistory(f.ctx, f.project, key, f.actor, native, source)
	if e != nil {
		t.Fatal(e)
	}
	native.snapshot.Directories[0].ObservationLSN = "0/3000"
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.backing.onRemove = cancel
	if _, e = f.j.executeHistory(ctx, baseline, f.actor, native, source); e == nil {
		t.Fatal("interrupted result was reported complete")
	}
	f.backing.onRemove = nil
	var intents, results int
	check, stop := context.WithTimeout(f.ctx, 5*time.Second)
	defer stop()
	if e = f.s.db.QueryRow(check, `SELECT count(*) FILTER(WHERE kind='intent'),count(*) FILTER(WHERE kind='result') FROM storage_gc_history_evidence WHERE baseline_id=$1`, baseline).Scan(&intents, &results); e != nil || intents != 1 || results != 0 || source.quarantined != 1 {
		t.Fatal("unknown DELETE lost its precommitted intent or quarantine", intents, results, e)
	}
}
