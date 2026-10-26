package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogPlatformPostgresOriginalReceiptAndAtomicAudit(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	source, snapshot := platformCatalogFixture()
	source.Scope = bindingPayload("platform", 1).NativeIdentity.Scope
	snapshot.Scope = source.Scope
	observation, err := captureCatalogPlatform(source, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	key, hash := keyHash("original-platform-observation"), strings.Repeat("a", 64)
	commit := func(actor, receiptKey, digest string) (record, error) {
		t.Helper()
		tx, e := s.db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		value, e := commitCatalogPlatformBaseline(ctx, tx, actor, "platform-original-request", receiptKey, digest, observation)
		if e != nil {
			return nil, e
		}
		return value, tx.Commit(ctx)
	}
	exec(`CREATE FUNCTION reject_platform_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned platform audit failure'; END $$; CREATE TRIGGER reject_platform_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_platform_audit()`)
	if _, err = commit("binding_admin", key, hash); err == nil {
		t.Fatal("audit failure accepted")
	}
	var rows, receipts int
	if err = s.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_platform_identity_baselines),(SELECT count(*) FROM catalog_identity_requests)`).Scan(&rows, &receipts); err != nil || rows != 0 || receipts != 0 {
		t.Fatal("incomplete identity or receipt survived rollback", rows, receipts, err)
	}
	exec(`DROP TRIGGER reject_platform_audit ON audit_events; DROP FUNCTION reject_platform_audit()`)
	value, err := commit("binding_admin", key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if stringVal(value["baseline_id"]) == "" || value["credentials"] != "not_observed" || value["protected_object_mutation"] != false {
		t.Fatal("unexpected platform receipt", value)
	}
	if _, err = commit("binding_admin", key, hash); err != nil {
		t.Fatal("original receipt replay", err)
	}
	if _, err = commit("binding_admin", key, strings.Repeat("b", 64)); err == nil {
		t.Fatal("same key changed observation")
	}
	if _, err = commit("binding_admin", keyHash("different-first-platform-key"), hash); err == nil {
		t.Fatal("system identity rebaselined")
	}
	id, stored, _, err := readCatalogPlatformBaseline(ctx, s.db, source)
	if err != nil || id != stringVal(value["baseline_id"]) {
		t.Fatal("original baseline lookup", err)
	}
	items, err := compareCatalogPlatform(stored, snapshot)
	if err != nil || len(items) != 4 {
		t.Fatal("stored platform identity proof", err)
	}
	for _, item := range items {
		if item.Status != "catalog_matches" {
			t.Fatal(item)
		}
	}
	exec(`UPDATE organization_members SET role='viewer' WHERE org_id='binding_org' AND user_id='binding_admin'`)
	if _, err = commit("binding_admin", key, hash); err == nil {
		t.Fatal("revoked current permission replayed system receipt")
	}
	if err = s.db.QueryRow(ctx, `SELECT count(*) FROM catalog_platform_identity_baselines`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal(rows, err)
	}
}

func TestCatalogPlatformPostgresImmutableHistoryAndTimelineIsolation(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	source, snapshot := platformCatalogFixture()
	source.Scope = bindingPayload("platform", 1).NativeIdentity.Scope
	snapshot.Scope = source.Scope
	observation, err := captureCatalogPlatform(source, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	value, err := commitCatalogPlatformBaseline(ctx, tx, "binding_admin", "immutable-platform-request", keyHash("platform-immutable-original"), strings.Repeat("a", 64), observation)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	id := stringVal(value["baseline_id"])
	for _, sql := range []string{`DELETE FROM catalog_platform_identity_baselines WHERE id=$1`, `UPDATE catalog_platform_identity_baselines SET observation='{}' WHERE id=$1`, `UPDATE catalog_platform_identity_baselines SET captured_by='binding_admin' WHERE id=$1`} {
		if _, err = s.db.Exec(ctx, sql, id); err == nil {
			t.Fatal("original system baseline mutable")
		}
	}
	different := source
	different.Scope.TimelineID = strings.Repeat("c", 32)
	if _, _, _, err = readCatalogPlatformBaseline(ctx, s.db, different); !isNoRows(err) {
		t.Fatal("prior timeline accepted as current system identity", err)
	}
	encoded, _ := json.Marshal(observation)
	if _, err = s.db.Exec(ctx, `INSERT INTO catalog_platform_identity_baselines(id,project_id,branch_id,tenant_id,timeline_id,endpoint_id,credential_ref,credential_uid,config_uid,observation,observation_sha256,captured_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'binding_admin')`, newID("cps_"), source.Project, source.Branch, source.Scope.TenantID, source.Scope.TimelineID, source.Endpoint, source.CredentialRef, source.CredentialUID, source.ConfigUID, encoded, strings.Repeat("b", 64)); err == nil {
		t.Fatal("second identity silently replaced original scope")
	}
	exec(`UPDATE branches SET timeline_id=$2 WHERE id=$1`, source.Branch, different.Scope.TimelineID)
	if _, _, _, err = readCatalogPlatformBaseline(ctx, s.db, different); !isNoRows(err) {
		t.Fatal("timeline change invented original identity", err)
	}
	_, stored, _, err := readCatalogPlatformBaseline(ctx, s.db, source)
	if err != nil {
		t.Fatal("historical proof lost", err)
	}
	witness, _ := stored.witness()
	original, _ := observation.witness()
	if witness != original {
		t.Fatal("historical proof changed with the branch head")
	}
}
