package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 使用真实PG创建绑定和清理lease；Kube仅模拟UID/RV条件删除。
// 特别覆盖首次清理之前就出现的替身，不能只测清理回执丢失后的替身。
func TestFailedDSGACleanupRequiresOriginalCreationResourceUIDs(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		bindVM, bindService   bool
		vmUID, serviceUID     string
		wantIdentityRejection bool
		wantDeletes           int
	}{
		{"original", true, true, "vm-uid", "original-service", false, 2},
		{"replacement_vm_before_first_delete", true, true, "replacement-vm", "original-service", true, 0},
		{"replacement_service_before_first_delete", true, true, "vm-uid", "replacement-service", true, 0},
		{"present_vm_without_creation_binding", false, false, "vm-uid", "", true, 0},
		{"present_service_without_creation_binding", true, false, "vm-uid", "original-service", true, 0},
		{"absent_without_creation_binding", false, false, "", "", false, 0},
		{"absent_vm_original_service", true, true, "", "original-service", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, s, origin, _, input := dsgaBindingFixture(t)
			binding, err := s.loadDSGACreationBinding(ctx, origin, "worker_a")
			if err != nil {
				t.Fatal(err)
			}
			if tc.bindVM {
				traceVM, traceConfig := input.Bind, input.BindConfig
				input.Bind = func(ctx context.Context, vm *vmv1.VirtualMachine) error {
					if err := binding.bindVM(ctx, vm); err != nil {
						return err
					}
					return traceVM(ctx, vm)
				}
				input.BindConfig = func(ctx context.Context, secret *core.Secret) error {
					if err := binding.bindConfig(ctx, secret); err != nil {
						return err
					}
					return traceConfig(ctx, secret)
				}
				if _, err := s.kube.prepareDSGACreation(ctx, input); err != nil {
					t.Fatal(err)
				}
			}
			if tc.bindService {
				service := &core.Service{ObjectMeta: metav1.ObjectMeta{Namespace: s.kube.namespace, Name: kubeName(input.Payload.EndpointID), UID: "original-service"}}
				if _, err := binding.serviceIdentity(ctx, "postgres", service); err != nil {
					t.Fatal(err)
				}
			}
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := s.db.Exec(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			p := endpointDeletionPayload{ProjectID: input.Payload.ProjectID, BranchID: input.Payload.BranchID, EndpointID: input.Payload.EndpointID, FailedCreateOperationID: origin}
			scope := deletionPayload{ProjectID: p.ProjectID, BranchIDs: []string{p.BranchID}, EndpointIDs: []string{p.EndpointID}}
			exec(`UPDATE operations SET state='failed',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, origin)
			exec(`UPDATE endpoints SET state='error' WHERE id=$1`, p.EndpointID)
			cleanup, worker := newID("op_cleanup_"), "cleanup-worker"
			raw, _ := json.Marshal(p)
			exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at)
 VALUES($1,$2,'endpoint',$3,'cleanup_failed_endpoint','running','test','test',$4,$5,clock_timestamp()+interval '1 hour')`, cleanup, p.ProjectID, p.EndpointID, raw, worker)
			exec(`UPDATE endpoints SET state='deleting',deletion_operation_id=$2 WHERE id=$1`, p.EndpointID, cleanup)

			var mu sync.Mutex
			deletes := 0
			objects := map[string]string{s.kube.path("vm", kubeName(p.EndpointID)): tc.vmUID, s.kube.path("service", kubeName(p.EndpointID)): tc.serviceUID}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				uid, known := objects[r.URL.Path]
				if !known {
					t.Errorf("unexpected cleanup request %s %s", r.Method, r.URL.Path)
					jsonResponse(w, 500, record{})
					return
				}
				if uid == "" {
					jsonResponse(w, 404, record{})
					return
				}
				if r.Method == http.MethodDelete {
					var options record
					if json.NewDecoder(r.Body).Decode(&options) != nil || nested(options, "preconditions", "uid") != uid || nested(options, "preconditions", "resourceVersion") != "1" {
						t.Error("cleanup must retain UID/RV delete preconditions")
					}
					deletes++
					objects[r.URL.Path] = ""
					jsonResponse(w, 200, record{})
					return
				}
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation %s", r.Method)
				}
				jsonResponse(w, 200, record{"metadata": record{"uid": uid, "resourceVersion": "1", "labels": credentialLabels(p.ProjectID, p.EndpointID)}, "spec": record{"type": "ClusterIP", "selector": record{"vm.neon.tech/name": kubeName(p.EndpointID)}}})
			}))
			defer peer.Close()
			s.kube.base, s.kube.http = peer.URL, peer.Client()
			err = s.retireFailedEndpointCompute(ctx, cleanup, worker, p, scope)
			if tc.wantIdentityRejection {
				if !errors.Is(err, errDSGACreationIdentity) {
					t.Fatal("unbound or replacement resource was not rejected", err)
				}
			} else {
				if err != nil {
					t.Fatal("original or absent VM rejected", err)
				}
				if err := s.retireFailedEndpointService(ctx, cleanup, worker, p); err != nil {
					t.Fatal("original or absent Service rejected", err)
				}
				// 原Operation再次读回不存在，不重复删除，也不改原创建UID。
				if err := s.retireFailedEndpointCompute(ctx, cleanup, worker, p, scope); err != nil {
					t.Fatal("original cleanup retry failed", err)
				}
				if err := s.retireFailedEndpointService(ctx, cleanup, worker, p); err != nil {
					t.Fatal("original Service cleanup retry failed", err)
				}
			}
			mu.Lock()
			actualDeletes := deletes
			mu.Unlock()
			if actualDeletes != tc.wantDeletes {
				t.Fatalf("destructive calls=%d, want %d", actualDeletes, tc.wantDeletes)
			}
			if tc.bindVM {
				var uid string
				if err := s.db.QueryRow(ctx, `SELECT vm_uid FROM dsga_creation_bindings WHERE operation_id=$1`, origin).Scan(&uid); err != nil || uid != "vm-uid" {
					t.Fatal("original VM binding changed", uid, err)
				}
			}
			if tc.bindService {
				var uid string
				if err := s.db.QueryRow(ctx, `SELECT service_uid FROM dsga_creation_services WHERE operation_id=$1 AND service_kind='postgres'`, origin).Scan(&uid); err != nil || uid != "original-service" {
					t.Fatal("original Service binding changed", uid, err)
				}
			}
		})
	}
}
