package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDSGARetainedReplicaUsesOriginalCredentialsAndIntent(t *testing.T) {
	for _, version := range []uint32{17, 18} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			catalog := dsgaReleaseCatalogFixture(t)
			for i := range catalog.Releases {
				c := &catalog.Releases[i].Creation
				c.VM.ProtocolVersion = 5
				c.VM.TLSClusterIssuer = ""
				c.VM.ResourceLimits.SystemMemoryMaxBytes = 64 << 20
				c.VM.GenericGuest = genericGuestFixture(c.PostgresVersion, c.Budget)
				c.RetainedCatalog = true
			}
			setDSGACatalog(t, catalog)
			f.body.PostgresVersion = int(version)
			if response := f.request("creation_admin", "retained-source-project", f.body); response.Code != 202 {
				t.Fatal("original project not accepted", response.Code, response.Body.String())
			}
			original, _, _ := f.reservation("retained-source-project")
			var p createPayload
			var raw []byte
			if err := f.s.db.QueryRow(f.ctx, `SELECT payload FROM operations WHERE id=$1`, original).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			f.exec(`UPDATE operations SET state='running',lease_owner='fixture-worker',lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, original)
			binding, err := f.s.loadDSGACreationBinding(f.ctx, original, "fixture-worker")
			if err != nil {
				t.Fatal(err)
			}
			if err := binding.bind(f.ctx, "fixture-retained-writer-vm", ""); err != nil {
				t.Fatal(err)
			}
			if err := binding.bind(f.ctx, "fixture-retained-writer-vm", "fixture-retained-writer-config"); err != nil {
				t.Fatal(err)
			}
			f.exec(`UPDATE projects SET state='ready' WHERE id=$1`, p.ProjectID)
			f.exec(`UPDATE branches SET state='ready' WHERE id=$1`, p.BranchID)
			f.exec(`UPDATE endpoints SET state='active' WHERE id=$1`, p.EndpointID)
			f.exec(`UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, original)
			body := endpointCreateRequest{BranchID: p.BranchID, Type: "read_only"}
			request := func(key string, body endpointCreateRequest) *httptest.ResponseRecorder {
				t.Helper()
				data, _ := json.Marshal(body)
				r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+p.ProjectID+"/endpoints", bytes.NewReader(data))
				r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "creation_admin-session"})
				r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "creation_admin-csrf"})
				r.Header.Set("X-CSRF-Token", "creation_admin-csrf")
				r.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				f.s.routes().ServeHTTP(w, r)
				return w
			}
			for range 2 {
				if response := request("retained-replica-key", body); response.Code != 202 {
					t.Fatal("same retained endpoint not accepted", response.Code, response.Body.String())
				}
			}
			var endpoint, action, tenant, timeline string
			var retained bool
			if err := f.s.db.QueryRow(f.ctx, `SELECT o.action,e.id,o.payload->>'tenant_id',o.payload->>'timeline_id',o.payload#>'{dsga_creation,retain_catalog}'='true'::jsonb
 FROM idempotency_keys i JOIN operations o ON o.id=i.operation_id JOIN endpoints e ON e.id=o.resource_id WHERE i.actor_id='creation_admin' AND i.key_hash=$1`, keyHash("retained-replica-key")).
				Scan(&action, &endpoint, &tenant, &timeline, &retained); err != nil {
				t.Fatal(err)
			}
			if endpoint == p.EndpointID || action != "create_endpoint_dsga_v1" || tenant != p.TenantID || timeline != p.TimelineID || !retained {
				t.Fatal("retained compute lost original data identity")
			}
			f.credentials.mu.Lock()
			old := f.credentials.objects[kubeName(p.EndpointID)+"-credentials"]
			new := f.credentials.objects[kubeName(endpoint)+"-credentials"]
			matched := f.credentials.posts == 2 && old != nil && new != nil && old.UID != new.UID &&
				bytes.Equal(old.Data["applicationVerifier"], new.Data["applicationVerifier"]) && bytes.Equal(old.Data["probeVerifier"], new.Data["probeVerifier"]) &&
				!bytes.Equal(old.Data["controlSigningSeed"], new.Data["controlSigningSeed"])
			f.credentials.mu.Unlock()
			if !matched {
				t.Fatal("retained credentials changed or were reissued")
			}
			body.Type, body.Password = "read_write", f.body.Password
			if response := request("retained-writer-conflict", body); response.Code != 409 {
				t.Fatalf("writer collision returned %d", response.Code)
			}

			// The API's credential-only Kubernetes fixture denies Pod listing, just
			// like the standalone API service account. A retired Writer can still
			// be accepted; the Worker checks actual VM and Runner absence before birth.
			retirement := "op_retained_writer_retirement"
			f.exec(`INSERT INTO operations(id,action,state,project_id,resource_type,resource_id,actor_id,request_id,payload,finished_at)
 VALUES($1,'delete_endpoint','succeeded',$2,'endpoint',$3,'creation_admin','req_retained_writer_retirement','{}',clock_timestamp())`, retirement, p.ProjectID, p.EndpointID)
			f.exec(`UPDATE endpoints SET state='deleted',deleted_at=clock_timestamp(),deletion_operation_id=$2 WHERE id=$1`, p.EndpointID, retirement)
			for range 2 {
				if response := request("retained-replace-writer", body); response.Code != 202 {
					t.Fatal("replacement Writer was not accepted by standalone API", response.Code, response.Body.String())
				}
			}
		})
	}
}
