package control

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestBranchResetPostgresDataOnlySourceKeepsOriginalParentEpoch(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	value, config, credential := resetBackingFixture(t)
	proof := value.Original
	proof.Operation = "op_" + strings.Repeat("1", 24)
	// Legacy bootstrap stored this verifier only in its original config.
	delete(credential["data"].(map[string]any), "probeVerifier")
	raw, _ := json.Marshal(proof)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id)
 VALUES($1,$2,'branch',$3,'create_project','succeeded','binding_admin','original-source-root')`, proof.Operation, proof.Project, proof.Branch)
	exec(`INSERT INTO compute_catalog_bootstrap_receipts(endpoint_id,operation_id,proof) VALUES($1,$2,$3)`, proof.Endpoint, proof.Operation, raw)
	exec(`INSERT INTO compute_catalog_bootstrap_publications(endpoint_id) VALUES($1)`, proof.Endpoint)
	child := "br_" + strings.Repeat("2", 16)
	childScope := catalogNativeScope{proof.Tenant, strings.Repeat("c", 32)}
	childOperation := "op_" + strings.Repeat("2", 24)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,parent_lsn,state,created_at)
 VALUES($1,$2,'data-only-history',$3,$4,'0/2000','ready',now())`, child, proof.Project, childScope.TimelineID, proof.Branch)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id)
 VALUES($1,$2,'branch',$3,'create_branch','succeeded','binding_admin','original-data-only-source')`, childOperation, proof.Project, child)
	original := catalogInheritanceSource{Operation: childOperation, PayloadSHA256: strings.Repeat("d", 64), Project: proof.Project, Branch: child, SourceBranch: proof.Branch, Scope: childScope, SourceScope: catalogNativeScope{proof.Tenant, proof.Timeline}, ParentLSN: "0/2000", Historical: true, Items: []catalogInheritanceItem{}}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = storeCatalogInheritanceSourceTx(ctx, tx, original); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var rollback map[string]any
	writes := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet {
			writes++
			w.WriteHeader(403)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "-config"):
			jsonResponse(w, 200, config)
		case strings.HasSuffix(r.URL.Path, "-credentials"):
			jsonResponse(w, 200, credential)
		case rollback != nil && strings.HasSuffix(r.URL.Path, "/"+stringVal(nested(rollback, "metadata", "name"))):
			jsonResponse(w, 200, rollback)
		default:
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	result, err := k.resolveBranchResetSourceRoot(ctx, s.db, proof.Project, child, childScope)
	if err != nil || result.StorageBranch != child || result.StorageScope != childScope || result.WitnessBranch != proof.Branch || result.WitnessScope != original.SourceScope || result.Bootstrap != proof {
		t.Fatal("data-only fork borrowed another native scope", err)
	}
	if _, invented := result.CredentialData["probeVerifier"]; invented {
		t.Fatal("source observation invented a legacy private file")
	}
	exposed, _ := json.Marshal(result)
	for _, private := range []string{"synthetic-reset-probe-password", "synthetic-original-key", "config_data", "CredentialData", "ConfigData"} {
		if strings.Contains(string(exposed), private) {
			t.Fatal("private source files were exportable")
		}
	}
	reset := "op_" + strings.Repeat("3", 24)
	next := catalogNativeScope{proof.Tenant, strings.Repeat("e", 32)}
	snapshot, err := prepareBranchResetBacking(reset, proof, next, config, credential)
	if err != nil {
		t.Fatal(err)
	}
	// The live parent has advanced. Its old data-only child's immutable source
	// still names the old native epoch and cannot use the parent's current one.
	mu.Lock()
	config["data"] = resetDataAny(snapshot.PreparedData)
	mu.Unlock()
	exec(`UPDATE branches SET timeline_id=$2 WHERE id=$1`, proof.Branch, next.TimelineID)
	if _, err = k.resolveBranchResetSourceRoot(ctx, s.db, proof.Project, child, childScope); err == nil {
		t.Fatal("advanced parent adopted without original retained files")
	}
	backup := "br_" + strings.Repeat("4", 16)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,parent_lsn,state,created_at)
 VALUES($1,$2,'retained-source-backup',$3,$4,'0/4000','ready',now())`, backup, proof.Project, strings.Repeat("f", 32), proof.Branch)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id)
 VALUES($1,$2,'branch',$3,'reset_branch','succeeded','binding_admin','retained-parent-epoch')`, reset, proof.Project, proof.Branch)
	exec(`INSERT INTO branch_reset_checkpoints(operation_id,project_id,branch_id,source_branch_id,tenant_id,
 original_timeline_id,source_timeline_id,target_timeline_id,backup_branch_id,backup_timeline_id,
 backup_lsn,restore_lsn,original_branch_version,payload_sha256,metadata_sha256)
 VALUES($1,$2,$3,$3,$4,$5,$5,$6,$7,$8,'0/4000','0/2000',1,$9,$10)`,
		reset, proof.Project, proof.Branch, proof.Tenant, proof.Timeline, next.TimelineID, backup, strings.Repeat("f", 32), strings.Repeat("a", 64), strings.Repeat("b", 64))
	snapshotRaw, _ := json.Marshal(snapshot)
	uid := "44444444-4444-4444-8444-444444444444"
	labels := credentialLabels(proof.Project, proof.Endpoint)
	labels["neon-control/reset-operation"] = reset
	mu.Lock()
	rollback = map[string]any{"immutable": true, "metadata": map[string]any{"name": snapshot.secretName(), "uid": uid, "labels": resetDataAny(labels)}, "data": map[string]any{"snapshot.json": base64.StdEncoding.EncodeToString(snapshotRaw)}}
	mu.Unlock()
	exec(`INSERT INTO branch_reset_endpoint_backings(operation_id,endpoint_id,endpoint_type,original_config_uid,
 original_credential_uid,rollback_secret_ref,rollback_secret_uid,rollback_sha256,prepared_config_sha256,original_endpoint_version)
 VALUES($1,$2,'read_write',$3,$4,$5,$6,$7,$8,1)`, reset, proof.Endpoint, proof.ConfigUID, proof.CredentialUID, snapshot.secretName(), uid, snapshot.digest(), snapshot.preparedDigest())
	result, err = k.resolveBranchResetSourceRoot(ctx, s.db, proof.Project, child, childScope)
	if err != nil || result.WitnessScope.TimelineID != proof.Timeline || result.WitnessScope == next || result.Bootstrap != proof {
		t.Fatal("immutable original parent epoch was not recovered", err)
	}
	if result.ConfigData["config.json"] != snapshot.ConfigData["config.json"] || result.CredentialData["probeVerifier"] != "" {
		t.Fatal("retained source files were changed")
	}
	foreign := childScope
	foreign.TenantID = strings.Repeat("d", 32)
	if _, err = k.resolveBranchResetSourceRoot(ctx, s.db, proof.Project, child, foreign); err == nil {
		t.Fatal("cross-tenant inheritance accepted")
	}
	mu.Lock()
	rollback["metadata"].(map[string]any)["uid"] = "55555555-5555-4555-8555-555555555555"
	mu.Unlock()
	if _, err = k.resolveBranchResetSourceRoot(ctx, s.db, proof.Project, child, childScope); err == nil {
		t.Fatal("replacement retained source snapshot accepted")
	}
	mu.Lock()
	count := writes
	mu.Unlock()
	if count != 0 {
		t.Fatal("source observation mutated external state", count)
	}
}
