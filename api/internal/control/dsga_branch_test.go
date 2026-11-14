package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDSGAStorageBranchLeasesAndReplaysExactAncestor(t *testing.T) {
	for _, major := range []uint32{17, 18} {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			setDSGACatalog(t, dsgaRetainedReleaseCatalogFixture(t))
			f.body.PostgresVersion = int(major)
			if got := f.request("creation_admin", "branch-parent-original", f.body); got.Code != http.StatusAccepted {
				t.Fatal("parent create not accepted", got.Code, got.Body.String())
			}
			op, _, _ := f.reservation("branch-parent-original")
			var raw []byte
			if err := f.s.db.QueryRow(f.ctx, "SELECT payload FROM operations WHERE id=$1", op).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var root struct {
				createPayload
				Plan dsgaCreationPlan `json:"dsga_creation"`
			}
			if err := json.Unmarshal(raw, &root); err != nil {
				t.Fatal(err)
			}
			f.exec("UPDATE operations SET state='running',lease_owner='parent-test',lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1", op)
			original, err := f.s.loadDSGACreationBinding(f.ctx, op, "parent-test")
			if err != nil {
				t.Fatal(err)
			}
			if err := original.bind(f.ctx, "fixture-fork-parent-vm", ""); err != nil {
				t.Fatal(err)
			}
			if err := original.bind(f.ctx, "fixture-fork-parent-vm", "fixture-fork-parent-config"); err != nil {
				t.Fatal(err)
			}
			f.exec("UPDATE projects SET state='ready' WHERE id=$1", root.ProjectID)
			f.exec("UPDATE branches SET state='ready' WHERE id=$1", root.BranchID)
			f.exec("UPDATE endpoints SET state='active' WHERE id=$1", root.EndpointID)
			f.exec("UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", op)
			auth := dsgaCreationAuth{Actor: "creation_admin", SessionHash: digest("creation_admin-session"), CSRFHash: digest("creation_admin-csrf")}
			ctx := context.WithValue(f.ctx, dsgaCreationAuthKey{}, auth)
			body := branchCreateRequest{Name: "Retained historical branch", ParentBranchID: root.BranchID}
			p, branchOp, err := f.s.admitDSGAStorageBranch(ctx, root.ProjectID, "org_creation", auth.Actor, root.TenantID, root.TimelineID, "0/200", "lsn",
				body, &root.Plan, "branch-from-exact-lsn", "request-digest", "request-branch")
			if err != nil {
				t.Fatal(err)
			}
			if p.TimelineID == root.TimelineID || p.ParentLSN != "0/200" || p.EndpointID != "" {
				t.Fatal("wrong branch scope", p)
			}
			f.exec("UPDATE operations SET state='running',lease_owner='branch-test',lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1", branchOp)
			binding, err := f.s.loadDSGACreationBinding(ctx, branchOp, "branch-test")
			if err != nil {
				t.Fatal(err)
			}
			var creates, leases int
			var mu sync.Mutex
			timelinePath := "/control/v1/tenant/" + root.TenantID + "/timeline/"
			leasePath := "/v1/tenant/" + root.TenantID + "/timeline/" + root.TimelineID + "/lsn_lease"
			createPath := "/v1/tenant/" + root.TenantID + "/timeline"
			var client *dsgaStorageClient
			client = dsgaStorageTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				scope, allowed := storageRequestScope(r.Method, strings.TrimPrefix(r.URL.Path, "/"))
				if !allowed || !validDSGAStorageToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), client.publicKey, scope) {
					creationStatus(w, 403)
					return
				}
				switch r.Method + " " + r.URL.Path {
				case "GET " + "/control/v1/tenant/" + root.TenantID:
					_ = json.NewEncoder(w).Encode(map[string]any{"tenant_id": root.TenantID, "shards": []any{map[string]any{"tenant_shard_id": root.TenantID}}})
				case "GET " + timelinePath + root.TimelineID:
					_ = json.NewEncoder(w).Encode(map[string]any{"shards": []any{map[string]any{"tenant_id": root.TenantID, "timeline_id": root.TimelineID,
						"pg_version": major, "state": "Active", "min_readable_lsn": "0/100", "initdb_lsn": "0/100", "last_record_lsn": "0/300"}}})
				case "GET " + timelinePath + p.TimelineID:
					if creates == 0 {
						creationStatus(w, 404)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"shards": []any{map[string]any{"tenant_id": root.TenantID, "timeline_id": p.TimelineID,
						"pg_version": major, "state": "Active", "ancestor_timeline_id": root.TimelineID, "ancestor_lsn": p.ParentLSN}}})
				case "POST " + leasePath:
					var request map[string]string
					if json.NewDecoder(r.Body).Decode(&request) != nil || len(request) != 1 || request["lsn"] != p.ParentLSN {
						t.Error("wrong LSN leased", request)
					}
					leases++
					_, _ = w.Write([]byte("null"))
				case "POST " + createPath:
					var request dsgaTimelineRequest
					if json.NewDecoder(r.Body).Decode(&request) != nil || request != (dsgaTimelineRequest{p.TimelineID, major, root.TimelineID, p.ParentLSN}) || leases != 1 {
						t.Error("branch POST differs from the leased ancestor")
					}
					creates++
					creationStatus(w, 504)
				default:
					t.Errorf("unexpected storage request %s %s", r.Method, r.URL.Path)
					creationStatus(w, 400)
				}
			}))
			client.profile, client.namespace = root.Plan.Storage, root.Plan.Namespace
			if err := binding.prepareTimelineWithStorage(ctx, client); err == nil {
				t.Fatal("a lost storage reply was reported as success")
			}
			for range 2 {
				if err := f.s.createStep(ctx, branchOp, "branch-test", 0, "create_timeline", func(ctx context.Context) error {
					return binding.prepareTimelineWithStorage(ctx, client)
				}); err != nil {
					t.Fatal(err)
				}
				if err := binding.persistStorageBranchReady(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var state, observed string
			var endpointCount int
			if err := f.s.db.QueryRow(ctx, `SELECT b.state,v.observed_state,(SELECT count(*) FROM endpoints WHERE branch_id=b.id)
 FROM branches b JOIN branch_service_instances v ON v.branch_id=b.id WHERE b.id=$1`, p.BranchID).Scan(&state, &observed, &endpointCount); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			createCount, leaseCount := creates, leases
			mu.Unlock()
			if state != "ready" || observed != "suspended" || endpointCount != 0 || createCount != 1 || leaseCount != 1 {
				t.Fatalf("branch completion differs %s %s endpoints=%d creates=%d leases=%d", state, observed, endpointCount, createCount, leaseCount)
			}
			f.exec("UPDATE operations SET lease_expires_at=$2 WHERE id=$1", branchOp, time.Now().Add(-time.Second))
			if err := binding.persistStorageBranchReady(ctx); err == nil {
				t.Fatal("expired lease settled original branch")
			}
			f.exec("UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", branchOp)
			bodyEndpoint := endpointCreateRequest{BranchID: p.BranchID, Type: "read_write", Password: f.body.Password}
			request := func(key string, body endpointCreateRequest) *httptest.ResponseRecorder {
				t.Helper()
				data, _ := json.Marshal(body)
				r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+root.ProjectID+"/endpoints", bytes.NewReader(data))
				r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "creation_admin-session"})
				r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "creation_admin-csrf"})
				r.Header.Set("X-CSRF-Token", "creation_admin-csrf")
				r.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				f.s.routes().ServeHTTP(w, r)
				return w
			}
			wrong := bodyEndpoint
			wrong.Password = "Incorrect application password"
			if resp := request("first-fork-writer-wrong", wrong); resp.Code != 422 || !strings.Contains(resp.Body.String(), "branch_password_mismatch") {
				t.Fatal("incorrect historical password accepted", resp.Code, resp.Body.String())
			}
			for range 2 {
				if resp := request("first-fork-writer-accepted", bodyEndpoint); resp.Code != 202 {
					t.Fatal("fork writer not accepted", resp.Code, resp.Body.String())
				}
			}
			var frozenRaw, credentialRaw []byte
			if err := f.s.db.QueryRow(ctx, `SELECT r.draft->'credential_source',o.payload#>'{dsga_creation,credentials}' FROM dsga_creation_reservations r
 JOIN operations o ON o.id=r.accepted_id AND o.action='create_endpoint_dsga_v1' WHERE r.actor_id=$1 AND r.key_hash=$2
 AND o.payload->>'timeline_id'=$3`, auth.Actor, keyHash("first-fork-writer-accepted"), p.TimelineID).Scan(&frozenRaw, &credentialRaw); err != nil {
				t.Fatal(err)
			}
			var frozen dsgaEndpointCredentialSource
			var credential dsgaCredentialReference
			if decodeDSGAObject(frozenRaw, &frozen) != nil || decodeDSGAObject(credentialRaw, &credential) != nil ||
				frozen != (dsgaEndpointCredentialSource{Endpoint: root.EndpointID, Operation: op, Reference: root.Plan.Credentials,
					Fork: dsgaRetainedFork{Operation: branchOp, ParentBranch: root.BranchID, ParentTimeline: root.TimelineID, LSN: p.ParentLSN}}) ||
				credential.validate() != nil || credential.UID == frozen.Reference.UID {
				t.Fatal("first writer lost fork ancestry or reused original Secret identity")
			}
		})
	}
}
