package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestBranchResetPostgresPrivateGrantLeaseAdminScopeAndAudit(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	fixture, config, stored, request, _ := resetGrantFixture(t)
	operation := fixture.Operation
	endpoint := branchResetProbeEndpoint(operation)
	originalEndpoint := "ep_0123456789abcdef"
	fixture.Endpoint = endpoint
	config["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/endpoint-id"] = endpoint
	const worker = "reset-grant-original-worker"
	backup := "br_" + strings.Repeat("3", 16)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,parent_lsn,state,created_at)
 VALUES($1,$2,'before-reset',$3,$4,'0/4000','creating',now())`, backup, fixture.Project, strings.Repeat("d", 32), fixture.Branch)
	exec(`UPDATE branches SET state='restoring' WHERE id=$1`, fixture.Branch)
	exec(`UPDATE endpoints SET state='suspended' WHERE id=$1`, originalEndpoint)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,lease_owner,lease_expires_at)
 VALUES($1,$2,'branch',$3,'reset_branch','running','binding_admin','owned-reset-grant',$4,now()+interval '5 minutes')`, operation, fixture.Project, fixture.Branch, worker)
	exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,0,'probe_restored_native_catalog','running')`, operation)
	exec(`INSERT INTO branch_reset_checkpoints(operation_id,project_id,branch_id,source_branch_id,tenant_id,
 original_timeline_id,source_timeline_id,target_timeline_id,backup_branch_id,backup_timeline_id,
 backup_lsn,restore_lsn,original_branch_version,payload_sha256,metadata_sha256)
 VALUES($1,$2,$3,$3,$4,$5,$5,$6,$7,$8,'0/4000','0/2000',1,$9,$10)`,
		operation, fixture.Project, fixture.Branch, fixture.Scope.TenantID, strings.Repeat("b", 32), fixture.Scope.TimelineID, backup, strings.Repeat("d", 32), strings.Repeat("a", 64), strings.Repeat("b", 64))
	snapshot := catalogNativeSnapshot{Scope: fixture.Scope, Roles: []catalogNativeRole{{OID: 10, Name: "cloud_admin", Flags: catalogRoleFlags{Login: true}}, {OID: 16387, Name: "control_probe", Flags: catalogRoleFlags{Login: true, Superuser: true}}}, Databases: []catalogNativeDatabase{{OID: 1, Name: "template1", OwnerOID: 10}}}
	roots := nested(request, "spec", "cluster", "roles").([]any)
	var mu sync.Mutex
	var permission map[string]any
	posts, puts := 0, 0
	lostPost, lostPut, conflict := true, true, true
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/virtualmachines/"):
			jsonResponse(w, 200, map[string]any{"metadata": map[string]any{"labels": resetDataAny(credentialLabels(fixture.Project, endpoint))}, "status": map[string]any{"phase": "Running"}})
		case strings.HasSuffix(r.URL.Path, "-config"):
			if r.Method == http.MethodPut {
				puts++
				if conflict {
					conflict = false
					w.WriteHeader(409)
					return
				}
				if json.NewDecoder(r.Body).Decode(&config) != nil {
					w.WriteHeader(422)
					return
				}
				if lostPut {
					lostPut = false
					w.WriteHeader(503)
					return
				}
			}
			jsonResponse(w, 200, config)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/secrets"):
			posts++
			if json.NewDecoder(r.Body).Decode(&permission) != nil {
				w.WriteHeader(422)
				return
			}
			permission["metadata"].(map[string]any)["uid"] = "44444444-4444-4444-8444-444444444444"
			if lostPost {
				lostPost = false
				w.WriteHeader(503)
				return
			}
			jsonResponse(w, 201, permission)
		case permission != nil && strings.HasSuffix(r.URL.Path, "/"+stringVal(nested(permission, "metadata", "name"))):
			jsonResponse(w, 200, permission)
		default:
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	s.kube = &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	issue := func(lease string, observation catalogNativeSnapshot) (string, error) {
		return s.issueBranchResetPlatformGrant(ctx, operation, lease, endpoint, observation, roots, 10, 16387)
	}
	if _, err := issue("other-worker", snapshot); err == nil {
		t.Fatal("foreign lease created private grant")
	}
	wrong := snapshot
	wrong.Scope.TimelineID = strings.Repeat("e", 32)
	if _, err := issue(worker, wrong); err == nil {
		t.Fatal("foreign restored scope created private grant")
	}
	exec(`UPDATE organization_members SET role='viewer' WHERE user_id='binding_admin'`)
	if _, err := issue(worker, snapshot); err == nil {
		t.Fatal("revoked Admin created private grant")
	}
	exec(`UPDATE organization_members SET role='admin' WHERE user_id='binding_admin'`)
	exec(`UPDATE operation_steps SET state='queued' WHERE operation_id=$1`, operation)
	if _, err := issue(worker, snapshot); err == nil {
		t.Fatal("unprobed target created private grant")
	}
	exec(`UPDATE operation_steps SET state='running' WHERE operation_id=$1`, operation)
	exec(`UPDATE endpoints SET state='active' WHERE id=$1`, originalEndpoint)
	if _, err := issue(worker, snapshot); err == nil {
		t.Fatal("public active Endpoint admitted private grant")
	}
	exec(`UPDATE endpoints SET state='suspended' WHERE id=$1`, originalEndpoint)
	exec(`CREATE FUNCTION reject_reset_grant_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned reset audit rejection';END $$;
 CREATE TRIGGER reject_reset_grant_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_reset_grant_audit()`)
	if _, err := issue(worker, snapshot); err == nil {
		t.Fatal("audit failure authorized private grant")
	}
	mu.Lock()
	writes := posts + puts
	mu.Unlock()
	if writes != 0 {
		t.Fatal("denied or unaudited authority wrote an external grant", writes)
	}
	exec(`DROP TRIGGER reject_reset_grant_audit ON audit_events`)
	ref, err := issue(worker, snapshot)
	if err != nil {
		t.Fatal("original grant did not recover unknown writes", err)
	}
	mu.Lock()
	postCount, putCount := posts, puts
	permissionCopy, configCopy := resetJSONCopy(permission), resetJSONCopy(config)
	mu.Unlock()
	if postCount != 1 || putCount != 2 || nested(configCopy, "metadata", "annotations", resetGrantAnnotation) != ref {
		t.Fatal("original grant was replaced after response loss", postCount, putCount)
	}
	raw, err := secretText(permissionCopy, "grant.json")
	var value branchResetPlatformGrant
	if err != nil || json.Unmarshal([]byte(raw), &value) != nil || value.Endpoint != endpoint {
		t.Fatal("private grant scope changed", err)
	}
	if !validBranchResetPlatformGrant(value, configCopy, stored, request, value.ExpiresAt.Add(-1)) {
		t.Fatal("retained grant does not authorize its exact original projection")
	}
	var count int
	if err = s.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='authorize_isolated_reset_platform_credentials' AND resource_id=$1`, fixture.Branch).Scan(&count); err != nil || count != 1 {
		t.Fatal("missing original grant audit", count, err)
	}
	exec(`UPDATE users SET disabled=true WHERE id='binding_admin'`)
	if s.checkBranchResetGrantAuthority(ctx, operation, worker, value) == nil {
		t.Fatal("disabled current actor retained live metadata authority")
	}
	if _, err := issue(worker, snapshot); err == nil {
		t.Fatal("revoked actor obtained another grant")
	}
	mu.Lock()
	after := posts + puts
	mu.Unlock()
	if after != postCount+putCount {
		t.Fatal("revoked actor changed external authority")
	}
}
