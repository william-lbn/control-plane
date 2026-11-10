package control

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/resourceguard"
)

func TestDSGAProxyOriginalProcessBirthUsesFixedReadOnlyRoute(t *testing.T) {
	_, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/dsg/v1/resource/process" {
			t.Error("process discovery changed method or target")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root_process_fixture":true}`))
	})
	code, data := proxyRequest(t, client, http.MethodGet, base+"/dsg/v1/resource/process", token, nil)
	if code != 200 || string(data) != `{"root_process_fixture":true}` {
		t.Fatal("original process route not forwarded", code)
	}
	code, _ = proxyRequest(t, client, http.MethodPost, base+"/dsg/v1/resource/process", token, []byte(`{}`))
	if code == 200 {
		t.Fatal("caller supplied a process discovery body")
	}
}

func TestDSGAProxyResourceStateUsesFixedReadOnlyRoute(t *testing.T) {
	f, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/dsg/v1/resource/state" {
			t.Error("resource state changed method or target")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource_state_fixture":true}`))
	})
	// 实际生产Store写入新增资源字段，确保网关/签发器使用准确AS版本读取
	// 已绑定账本，而不只在没有新字段的旧夹具上验证路由。
	f.mu.Lock()
	vm := f.vm.DeepCopy()
	f.mu.Unlock()
	store, err := journal.NewStore(f.proxy.broker.vms.NeonvmV1().VirtualMachines(vm.Namespace), journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: vm.Spec.DSGABootstrap.Incarnation})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := resourceguard.Identity{VMUID: string(vm.UID), ComputeInstanceID: vm.Spec.DSGABootstrap.ComputeInstanceID, GuestBootID: "12345678-1234-1234-1234-123456789abc", CgroupDevice: 2, CgroupInode: 3}
	if _, err := store.BindResource(context.Background(), snapshot, snapshot.State().Owner, id); err != nil {
		t.Fatal(err)
	}
	code, data := proxyRequest(t, client, http.MethodGet, base+"/dsg/v1/resource/state", token, nil)
	if code != 200 || string(data) != `{"resource_state_fixture":true}` {
		t.Fatal("resource state not forwarded", code)
	}
	code, _ = proxyRequest(t, client, http.MethodPost, base+"/dsg/v1/resource/state", token, []byte(`{}`))
	if code == 200 {
		t.Fatal("state mutation accepted")
	}
}

// 只验证本批新增固定资源路由，复用实际TLS网关夹具；不重复原PG路由全套，
// 不把客户端/Lease API夹具称作真实K8s或root执行器验收。
func TestDSGAProxyResourceRoutesPreserveOriginalAndRejectAliases(t *testing.T) {
	var calls atomic.Int32
	_, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && string(body) != `{"issuer_epoch":"9007199254740993"}` {
			t.Error("resource original bytes changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fixture":true}`))
	})
	for _, path := range []string{"identity", "fence", "apply", "observe", "replay-original"} {
		method, body := http.MethodPost, []byte(`{"issuer_epoch":"9007199254740993"}`)
		if path == "identity" {
			method, body = http.MethodGet, nil
		}
		code, data := proxyRequest(t, client, method, base+"/dsg/v1/resource/"+path, token, body)
		if code != 200 || string(data) != `{"fixture":true}` {
			t.Fatalf("resource %s: %d", path, code)
		}
	}
	for _, path := range []string{"initialize", "bootstrap", "apply?override=1", "%61pply"} {
		code, _ := proxyRequest(t, client, http.MethodPost, base+"/dsg/v1/resource/"+path, token, []byte(`{}`))
		if code == 200 {
			t.Fatal("unsafe resource path accepted", path)
		}
	}
	if calls.Load() != 5 {
		t.Fatal("unexpected resource native calls", calls.Load())
	}
}
