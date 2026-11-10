package control

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/resourceguard"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 真实元数据库与HTTP协议边界；Root内容为合成样本，不能作为Linux硬限验收。
// 覆盖原Service类型、原签名、只读请求和跨层读回，防止把元数据或RSS当实际完成。
func TestDSGAMemoryObservationUsesOriginalServicesAndConfirmedHardLimit(t *testing.T) {
	ctx, b, f, credentials, service, _, _, initial := initializationBindingFixture(t)
	current, err := b.server.loadDSGACreationBinding(ctx, b.operationID, b.workerID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.vm.Status.PodName = "original-runner"
	f.vm.Status.MemorySize = resource.NewQuantity(8<<30, resource.BinarySI)
	vm := f.vm.DeepCopy()
	f.mu.Unlock()
	log, err := journal.Inspect(vm, journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: plan.Incarnation})
	if err != nil {
		t.Fatal(err)
	}
	epoch := log.State().Owner.Epoch
	initial.Observed.IssuerFloorEpoch = epoch
	endpoint := &dsgaEndpointRuntime{Operation: b.operationID, Payload: b.intent.createPayload, Plan: plan, VMUID: string(vm.UID), ConfigUID: current.configUID}
	selected := runtimeselection.Selection{Instance: initial.Observed.Instance}
	id := resourceguard.Identity{VMUID: string(vm.UID), ComputeInstanceID: plan.ComputeInstanceID, GuestBootID: "12345678-1234-1234-1234-123456789abc", CgroupDevice: 2, CgroupInode: 100}
	limits := resourceguard.Limits{MemoryMaxBytes: 128 << 20, OutsideUpperBytes: 32 << 20, HugePools: []resourceguard.HugePool{{PageSizeKB: 2048, Pages: 0}}}
	root := resourceguard.View{ProtocolVersion: 1, Identity: id, IssuerFloorEpoch: epoch, Bootstrap: resourceguard.BootstrapCommand{ProtocolVersion: 1, Identity: id, IssuerEpoch: epoch, BudgetCommitmentID: "synthetic-budget", Limits: limits}, BootstrapApplied: true,
		Actual: resourceguard.Evidence{ProtocolVersion: 1, Identity: id, MemoryMaxBytes: limits.MemoryMaxBytes, MemoryCurrentBytes: 1 << 20, GuestMemTotalBytes: 8 << 30, HugePools: limits.HugePools, HugeOvercommitDisabled: true}}
	if _, err := root.ConfirmedLimits(); err != nil {
		t.Fatal("invalid synthetic Root", err)
	}
	mode := "current"
	initReads, rootReads := 0, 0
	key := ed25519.NewKeyFromSeed((*credentials).Data["controlSigningSeed"])
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Error("observation attempted a mutation or arbitrary query")
			w.WriteHeader(405)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Error("missing native token")
			w.WriteHeader(401)
			return
		}
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims dsgaProxyClaims
		if !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), signature) || dsgaTokenObject(raw, &claims) != nil || claims.VMUID != string(vm.UID) || claims.ComputeInstanceID != plan.ComputeInstanceID {
			t.Error("foreign native identity")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/dsg/v1/initialization":
			initReads++
			fact := initial
			if claims.Scope != "compute_ctl:dsga_initialization" {
				t.Error("unexpected initialization authority")
			}
			if initReads == 2 && mode == "pg-changed" {
				fact.Observed.CapacityGeneration++
			}
			_ = json.NewEncoder(w).Encode(fact)
		case "/dsg/v1/resource/state":
			rootReads++
			view := root.Clone()
			if claims.Scope != "compute_ctl:dsga" || claims.IssuerEpoch != epoch {
				t.Error("wrong original resource reader")
			}
			switch mode {
			case "unknown":
				view.BootstrapApplied = false
			case "drift":
				view.Actual.MemoryMaxBytes++
			case "old-owner":
				view.IssuerFloorEpoch++
			case "cache":
				w.Header().Del("Cache-Control")
			case "media":
				w.Header().Set("Content-Type", "text/plain")
			case "vm-changed":
				f.mu.Lock()
				f.vm.Status.MemorySize = resource.NewQuantity(9<<30, resource.BinarySI)
				f.mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(view)
		default:
			t.Error("unexpected native path")
			w.WriteHeader(404)
		}
	}))
	defer native.Close()
	client := newDSGAProxy(nil).observer
	defer client.CloseIdleConnections()
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != net.JoinHostPort(service.Spec.ClusterIP, "3080") {
			t.Error("caller-controlled native address")
			return nil, net.InvalidAddrError("foreign address")
		}
		return new(net.Dialer).DialContext(ctx, network, native.Listener.Addr().String())
	}
	for _, scenario := range []string{"current", "unknown", "drift", "old-owner", "cache", "media", "pg-changed", "vm-changed"} {
		t.Run(scenario, func(t *testing.T) {
			mode = scenario
			initReads, rootReads = 0, 0
			f.mu.Lock()
			f.vm = vm.DeepCopy()
			f.mu.Unlock()
			actual, err := b.server.observeDSGAEndpointMemoryWithHTTP(ctx, endpoint, vm, selected, client)
			if scenario == "current" {
				if err != nil || actual.WorkMemoryMaxBytes != limits.MemoryMaxBytes || actual.WorkMemoryMaxBytes == root.Actual.MemoryCurrentBytes || actual.VMMemoryBytes != 8<<30 || actual.SharedBufferBytes != dsgapi.Uint64(initial.Observed.LowBuffers)*8192 || initReads != 2 || rootReads != 1 {
					t.Fatalf("not a coherent actual read: %#v %v reads=%d/%d", actual, err, initReads, rootReads)
				}
			} else if err == nil {
				t.Fatal("unconfirmed state became current")
			}
		})
	}
}

// 上游dump-state的GET仍必须有{} JSON正文；通过真实client-go传输固定该合同，
// 不能只测试合成decoded状态而遗漏实际HTTP接线。
func TestDSGAMemoryPolicyReadsOriginalAgentWithUpstreamJSONContract(t *testing.T) {
	_, plan := supplyFixture(t, 17)
	selected := selectionFixture(t)
	p := plan.VM.supply()
	vm := &vmv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: selected.Namespace}}
	vm.Status.Node = "worker-119"
	vm.Status.PodName = "runner"
	yes := true
	ds := &apps.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: p.AgentDaemonSet, Namespace: p.AgentNamespace, UID: "original-agent-ds"}}
	pod := core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-119", Namespace: p.AgentNamespace, UID: "agent-pod", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: &yes}}}, Spec: core.PodSpec{NodeName: vm.Status.Node, ServiceAccountName: p.AgentServiceAccount}, Status: core.PodStatus{Phase: core.PodRunning}}
	status := runtimeselection.Status{DesiredGeneration: 1, AppliedGeneration: 1, DesiredSHA256: selected.Digest(), AppliedSHA256: selected.Digest(), State: "applied"}
	calls := 0
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("policy read attempted write")
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/daemonsets/") {
			_ = json.NewEncoder(w).Encode(ds)
			return
		}
		if strings.Contains(r.URL.Path, "/proxy") {
			calls++
			raw, _ := io.ReadAll(r.Body)
			if strings.TrimSpace(string(raw)) != "{}" {
				t.Error("upstream JSON body missing")
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"stopped": false, "pods": []any{map[string]any{"podName": map[string]string{"namespace": vm.Namespace, "name": vm.Status.PodName}, "runner": map[string]any{"dsga": map[string]any{"instance": selected.Instance, "observedAt": time.Now().UTC(), "runtimeSelection": status}}}}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pods") {
			if r.URL.Query().Get("fieldSelector") != "spec.nodeName=worker-119" {
				t.Error("unbounded Agent lookup")
			}
			_ = json.NewEncoder(w).Encode(core.PodList{Items: []core.Pod{pod}})
			return
		}
		t.Error("unexpected Kube path", r.URL.Path)
		w.WriteHeader(404)
	}))
	defer api.Close()
	clients, _, err := newDSGAKubeClients(&kubeClient{base: api.URL, http: api.Client(), token: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readDSGAOwnerPolicy(context.Background(), clients, vm, p, selected)
	if err != nil || got != status || calls != 1 {
		t.Fatal("original Agent observation failed", err, calls)
	}
}
