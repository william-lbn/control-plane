package control

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBranchResetPostgresEpochReceiptAndAtomicCurrentPublication(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	source, original, target := resetConfigFixture(t, "read_write")
	original.Operation = "op_" + strings.Repeat("1", 24)
	reset := "op_" + strings.Repeat("2", 24)
	backup := "br_" + strings.Repeat("3", 16)
	oldPayload := original.payload()
	newPayload := oldPayload
	newPayload.TimelineID = target.TimelineID
	originalJSON, _ := json.Marshal(original)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id)
 VALUES($1,$2,'branch',$3,'create_project','succeeded','binding_admin','initial-reset-fixture')`, original.Operation, original.Project, original.Branch)
	exec(`INSERT INTO compute_catalog_bootstrap_receipts(endpoint_id,operation_id,proof) VALUES($1,$2,$3::jsonb)`, original.Endpoint, original.Operation, originalJSON)
	exec(`INSERT INTO compute_catalog_bootstrap_publications(endpoint_id) VALUES($1)`, original.Endpoint)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,parent_lsn,state,created_at)
 VALUES($1,$2,'before-reset',$3,$4,'0/4000','creating',now())`, backup, original.Project, strings.Repeat("d", 32), original.Branch)
	exec(`UPDATE branches SET state='restoring' WHERE id=$1`, original.Branch)
	exec(`UPDATE endpoints SET state='suspended' WHERE id=$1`, original.Endpoint)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,lease_owner,lease_expires_at)
 VALUES($1,$2,'branch',$3,'reset_branch','running','binding_admin','owned-reset-request','reset-worker',now()+interval '5 minutes')`, reset, original.Project, original.Branch)
	exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,0,'probe_restored_native_catalog','running')`, reset)
	exec(`INSERT INTO branch_reset_checkpoints(operation_id,project_id,branch_id,source_branch_id,tenant_id,
 original_timeline_id,source_timeline_id,target_timeline_id,backup_branch_id,backup_timeline_id,
 backup_lsn,restore_lsn,original_branch_version,payload_sha256,metadata_sha256)
 VALUES($1,$2,$3,$3,$4,$5,$5,$6,$7,$8,'0/4000','0/2000',1,$9,$10)`, reset, original.Project, original.Branch, original.Tenant, original.Timeline, target.TimelineID, backup, strings.Repeat("d", 32), strings.Repeat("a", 64), strings.Repeat("b", 64))
	exec(`INSERT INTO branch_reset_endpoint_backings(operation_id,endpoint_id,endpoint_type,
 original_config_uid,original_credential_uid,rollback_secret_ref,rollback_secret_uid,rollback_sha256,
 prepared_config_sha256,original_endpoint_version) VALUES($1,$2,'read_write',$3,$4,
 'owned-reset-rollback','rollback-secret-uid',$5,$6,1)`, reset, original.Endpoint, original.ConfigUID, original.CredentialUID, strings.Repeat("c", 64), strings.Repeat("d", 64))
	prepared := original
	prepared.Timeline = target.TimelineID
	prepared.Operation, prepared.Witness = "", ""
	store := func(p createPayload, value computeCatalogBootstrap, worker string) error {
		t.Helper()
		tx, err := s.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = storeBranchResetBootstrap(ctx, tx, reset, worker, p, value); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	prior, err := currentComputeBootstrap(ctx, s.db, oldPayload)
	if err != nil || prior != original {
		t.Fatal("original bootstrap unavailable before cutover", prior, err)
	}
	bad := prepared
	bad.ConfigUID = "replacement-config"
	if err = store(newPayload, bad, "reset-worker"); err == nil {
		t.Fatal("replacement configuration accepted")
	}
	exec(`UPDATE operation_steps SET state='queued' WHERE operation_id=$1`, reset)
	if err = store(newPayload, prepared, "reset-worker"); err == nil {
		t.Fatal("missing actual probe stage accepted")
	}
	exec(`UPDATE operation_steps SET state='running' WHERE operation_id=$1`, reset)
	if err = store(newPayload, prepared, "stale-worker"); err == nil {
		t.Fatal("foreign worker lease accepted")
	}
	if err = store(newPayload, prepared, "reset-worker"); err != nil {
		t.Fatal(err)
	}
	if err = store(newPayload, prepared, "reset-worker"); err != nil {
		t.Fatal("original probe receipt replay", err)
	}
	if _, err = readBranchResetBootstrap(ctx, s.db, newPayload); !isNoRows(err) {
		t.Fatal("unpublished epoch became current", err)
	}
	if _, err = currentComputeBootstrap(ctx, s.db, newPayload); err == nil {
		t.Fatal("new timeline borrowed original bootstrap")
	}
	prior, err = currentComputeBootstrap(ctx, s.db, oldPayload)
	if err != nil || prior != original {
		t.Fatal("preparation displaced the old epoch", err)
	}
	publication := func(cutover bool) error {
		t.Helper()
		tx, err := s.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `INSERT INTO compute_catalog_reset_publications(operation_id,endpoint_id) VALUES($1,$2)`, reset, original.Endpoint); err != nil {
			return err
		}
		if cutover {
			if _, err = tx.Exec(ctx, `UPDATE branches SET timeline_id=$2,state='ready' WHERE id=$1`, original.Branch, target.TimelineID); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	if err = publication(false); err == nil {
		t.Fatal("publication committed separately from native metadata cutover")
	}
	var count int
	if err = s.db.QueryRow(ctx, `SELECT count(*) FROM compute_catalog_reset_publications`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial publication survived", count, err)
	}
	if err = publication(true); err != nil {
		t.Fatal("atomic publication rejected", err)
	}
	current, err := currentComputeBootstrap(ctx, s.db, newPayload)
	if err != nil || current.Timeline != target.TimelineID || current.Operation != reset || current.Witness != branchResetBootstrapWitness || current.ConfigUID != original.ConfigUID || current.CredentialUID != original.CredentialUID {
		t.Fatal("wrong restored epoch", current, err)
	}
	if _, err = currentComputeBootstrap(ctx, s.db, oldPayload); err == nil {
		t.Fatal("stale scope reused old bootstrap after cutover")
	}
	document, err := branchResetComputeConfig(source, original, target)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(document)
	b64 := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	roles := document["spec"].(map[string]any)["cluster"].(map[string]any)["roles"].([]any)
	var replacement atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("cold resume attempted a backing mutation")
			w.WriteHeader(500)
			return
		}
		uid := original.ConfigUID
		if replacement.Load() {
			uid = "replacement-config"
		}
		var value record
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+kubeName(original.Endpoint)+"-config"):
			value = record{"metadata": record{"uid": uid, "resourceVersion": "1", "labels": credentialLabels(original.Project, original.Endpoint)}, "data": record{"config.json": b64(string(encoded)), "probePassword": b64("synthetic-reset-probe-password")}}
		case strings.HasSuffix(r.URL.Path, "/"+kubeName(original.Endpoint)+"-credentials"):
			value = record{"metadata": record{"uid": original.CredentialUID, "labels": credentialLabels(original.Project, original.Endpoint)}, "data": record{"adminVerifier": b64(stringVal(roles[0].(map[string]any)["encrypted_password"])), "probeVerifier": b64(stringVal(roles[1].(map[string]any)["encrypted_password"])), "probePassword": b64("synthetic-reset-probe-password")}}
		default:
			t.Error("unexpected backing read")
			w.WriteHeader(404)
			return
		}
		jsonResponse(w, 200, value)
	}))
	defer peer.Close()
	s.kube = &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	if err = s.resumeComputeCatalogBootstrap(ctx, newPayload); err != nil {
		t.Fatal("restored original Endpoint could not resume its sealed catalog", err)
	}
	replacement.Store(true)
	if err = s.resumeComputeCatalogBootstrap(ctx, newPayload); err == nil {
		t.Fatal("replacement config UID adopted during restored cold resume")
	}
	if err = store(newPayload, prepared, "reset-worker"); err != nil {
		t.Fatal("committed original probe replay", err)
	}
	exec(`UPDATE organization_members SET role='viewer' WHERE org_id='binding_org' AND user_id='binding_admin'`)
	if err = store(newPayload, prepared, "reset-worker"); err == nil {
		t.Fatal("revoked actor replayed privileged probe receipt")
	}
	exec(`INSERT INTO branch_reset_results(operation_id,result) VALUES($1,'{"result":"owned-fixture"}')`, reset)
	for _, table := range []string{"branch_reset_checkpoints", "branch_reset_endpoint_backings", "compute_catalog_reset_receipts", "compute_catalog_reset_publications", "branch_reset_results"} {
		for _, sql := range []string{"UPDATE " + table + " SET operation_id=operation_id", "DELETE FROM " + table} {
			if _, err = s.db.Exec(ctx, sql); err == nil {
				t.Fatal("immutable restore witness changed", table)
			}
		}
	}
	var retained []byte
	if err = s.db.QueryRow(ctx, `SELECT proof FROM compute_catalog_bootstrap_receipts WHERE endpoint_id=$1`, original.Endpoint).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	var old computeCatalogBootstrap
	if json.Unmarshal(retained, &old) != nil || old != original {
		t.Fatal("original bootstrap was rewritten")
	}
}
