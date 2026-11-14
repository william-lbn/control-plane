package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDSGAStorageBranchLeasesAndReplaysExactAncestor(t *testing.T) {
	for _, major := range []uint32{17, 18} {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			setDSGACatalog(t, dsgaReleaseCatalogFixture(t))
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
			f.exec("UPDATE projects SET state='ready' WHERE id=$1", root.ProjectID)
			f.exec("UPDATE branches SET state='ready' WHERE id=$1", root.BranchID)
			f.exec("UPDATE operations SET state='succeeded',finished_at=clock_timestamp() WHERE id=$1", op)
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
		})
	}
}
