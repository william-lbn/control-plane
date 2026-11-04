package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmfake "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned/fake"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type managedSQLTestFixture struct {
	mu                                 sync.Mutex
	objects                            map[string]map[string]any
	puts, reads, conflicts, putFailure int
	k                                  *kubeClient
	in                                 managedSQLRouteInput
	fact                               dsgaInitializationResponse
}

func managedSQLTestMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		t.Fatal("invalid fixture")
	}
	return m
}
func (f *managedSQLTestFixture) mutate(fn func(map[string]map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.objects)
}
func (f *managedSQLTestFixture) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.reads
}
func (f *managedSQLTestFixture) observe(ctx context.Context, in managedSQLRouteInput, s managedSQLRouteSource) (dsgaInitializationResponse, error) {
	return f.fact, nil
}

func newManagedSQLFixture(t *testing.T) *managedSQLTestFixture {
	t.Helper()
	ctx := context.Background()
	truth := true
	vm := new(vmv1.VirtualMachine)
	vm.Name, vm.Namespace, vm.UID, vm.ResourceVersion = "cp-test", "testing", "vm-uid", "1"
	vm.Labels = credentialLabels("prj_test", "ep_test")
	vm.Spec.Guest.Ports = []vmv1.Port{{Name: "postgres", Port: 55432, Protocol: vmv1.ProtocolTCP}, {Name: "compute-control", Port: 3080, Protocol: vmv1.ProtocolTCP}}
	vm.Spec.DSGABootstrap = &vmv1.DSGABootstrap{ProtocolVersion: 1, Incarnation: "1234567890abcdef1234567890abcdef", ComputeInstanceID: "compute-test"}
	clients := vmfake.NewSimpleClientset(vm)
	store, err := journal.NewStore(clients.NeonvmV1().VirtualMachines("testing"), journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: vm.Spec.DSGABootstrap.Incarnation})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Initialize(ctx, "1", "fixture-owner")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.ReleaseBootstrap(ctx, snapshot.ResourceVersion(), "config-uid")
	if err != nil {
		t.Fatal(err)
	}
	vm = snapshot.VirtualMachine()
	vm.Status.Phase = "Running"
	seed := bytes.Repeat([]byte{0x37}, 32)
	probe, err := scramVerifier(base64.RawURLEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatal(err)
	}
	app, err := scramVerifier("synthetic-fixture-application")
	if err != nil {
		t.Fatal(err)
	}
	credentials := &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name + "-credentials", UID: "credentials-uid", ResourceVersion: "1", Labels: vm.Labels},
		Type: core.SecretTypeOpaque, Immutable: &truth, Data: map[string][]byte{"applicationVerifier": []byte(app), "probeVerifier": []byte(probe), "controlSigningSeed": bytes.Repeat([]byte{0x19}, 32), "probeCredentialSeed": seed}}
	startup := dsgaNativeBootstrap{ProtocolVersion: 2, VMUID: "vm-uid", ComputeInstanceID: "compute-test", dsgaBootstrapBudget: dsgaBootstrapBudget{
		BootstrapBufferBytes: 1 << 20, MaximumBufferBytes: 2 << 20, MinimumGuestMemoryBytes: 1 << 30, NonBufferReserveBytes: 1 << 28}}
	body := map[string]any{"spec": map[string]any{"project_id": "prj_test", "endpoint_id": "ep_test", "branch_id": "br_test", "tenant_id": strings.Repeat("1", 32), "timeline_id": strings.Repeat("2", 32),
		"operation_uuid": "fixture-operation", "dynamic_shared_buffers": startup, "cluster": map[string]any{"roles": []any{
			map[string]any{"name": "app_owner", "encrypted_password": app}, map[string]any{"name": "control_probe", "encrypted_password": probe}}}}}
	configRaw, _ := json.Marshal(body)
	config := &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name + "-config", UID: "config-uid", ResourceVersion: "1", Labels: vm.Labels,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: vm.Name, UID: vm.UID, Controller: &truth}}},
		Type: core.SecretTypeOpaque, Immutable: &truth, Data: map[string][]byte{"config.json": configRaw}}
	sqlService := &core.Service{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name, UID: "sql-service-uid", ResourceVersion: "1"},
		Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, ClusterIP: "10.0.0.2", Selector: map[string]string{"vm.neon.tech/name": vm.Name},
			Ports: []core.ServicePort{{Name: "postgres", Port: 55432, Protocol: core.ProtocolTCP, TargetPort: intstr.FromString("postgres")}}}}
	management := &core.Service{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name + "-management", UID: "management-uid", ResourceVersion: "1", Labels: vm.Labels},
		Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, ClusterIP: "10.0.0.3", Selector: sqlService.Spec.Selector,
			Ports: []core.ServicePort{{Name: "compute-control", Port: 3080, Protocol: core.ProtocolTCP, TargetPort: intstr.FromInt32(3080)}}}}
	registry := &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: routesSecret, UID: "registry-uid", ResourceVersion: "1"},
		Type: core.SecretTypeOpaque, Data: map[string][]byte{"routes.json": []byte("{\"ep-unrelated\":{\"keep\":\"unchanged\",\"extra\":true}}")}}
	f := &managedSQLTestFixture{objects: map[string]map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			f.reads++
			if value, ok := f.objects[r.URL.Path]; ok {
				json.NewEncoder(w).Encode(value)
				return
			}
			w.WriteHeader(404)
			io.WriteString(w, "{}")
			return
		}
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/namespaces/testing/secrets/"+routesSecret {
			w.WriteHeader(403)
			return
		}
		f.puts++
		if f.conflicts > 0 {
			f.conflicts--
			w.WriteHeader(409)
			io.WriteString(w, "{}")
			return
		}
		if f.putFailure != 0 {
			w.WriteHeader(f.putFailure)
			io.WriteString(w, "{}")
			return
		}
		var value map[string]any
		if json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&value) != nil {
			w.WriteHeader(400)
			return
		}
		previous := f.objects[r.URL.Path]
		if stringVal(nested(value, "metadata", "resourceVersion")) != stringVal(nested(previous, "metadata", "resourceVersion")) {
			w.WriteHeader(409)
			return
		}
		value["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(f.puts + 1)
		f.objects[r.URL.Path] = value
		json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(server.Close)
	f.k = &kubeClient{base: server.URL, namespace: "testing", http: server.Client()}
	f.objects[f.k.path("vm", vm.Name)] = managedSQLTestMap(t, vm)
	f.objects[f.k.path("secret", config.Name)] = managedSQLTestMap(t, config)
	f.objects[f.k.path("secret", credentials.Name)] = managedSQLTestMap(t, credentials)
	f.objects[f.k.path("service", sqlService.Name)] = managedSQLTestMap(t, sqlService)
	f.objects[f.k.path("service", management.Name)] = managedSQLTestMap(t, management)
	f.objects[f.k.path("secret", routesSecret)] = managedSQLTestMap(t, registry)
	f.in = managedSQLRouteInput{ProtocolVersion: 1, RegistryUID: "registry-uid", ManagementServiceUID: "management-uid", Binding: adapter.ManagedSQLBinding{
		ProtocolVersion: 1, Namespace: "testing", ProjectID: "prj_test", EndpointID: "ep_test", BranchID: "br_test", TenantID: strings.Repeat("1", 32), TimelineID: strings.Repeat("2", 32),
		VMUID: "vm-uid", Incarnation: vm.Spec.DSGABootstrap.Incarnation, ComputeInstanceID: "compute-test", ConfigSecretUID: "config-uid", ConfigSHA256: managedSQLDigest(configRaw), SecretDataSHA256: managedSQLDataDigest(config.Data),
		CredentialSecretUID: "credentials-uid", CredentialDataSHA256: managedSQLDataDigest(credentials.Data), ServiceUID: "sql-service-uid"}}
	f.fact = dsgaInitializationResponse{ProtocolVersion: 1, OperationUUID: "fixture-operation", TenantID: f.in.Binding.TenantID, TimelineID: f.in.Binding.TimelineID, Startup: startup, AuthenticationPolicyVerified: true, OrdinaryRolesVerified: true,
		Observed: dsgapi.Snapshot{Instance: dsgapi.Instance{VMUID: "vm-uid", ComputeInstanceID: "compute-test", PGBootID: 17}, CapacityGeneration: 1, LowBuffers: 128, HighBuffers: 128, MaximumBuffers: 256, StartupSharedUpperBytes: 128 << 20}}
	for _, kind := range []dsgapi.ArrayKind{dsgapi.Descriptors, dsgapi.Blocks, dsgapi.IOCV, dsgapi.Checkpoint} {
		f.fact.Observed.Arrays = append(f.fact.Observed.Arrays, dsgapi.ArrayFact{Array: kind, CommittedUpperBytes: 4096, ReleaseState: dsgapi.ReleaseNone, Provider: dsgapi.ProviderFact{Status: dsgapi.ProviderUnknown}})
	}
	return f
}

func TestManagedSQLRoutePublisherNativeThenCASPreservesRegistry(t *testing.T) {
	f := newManagedSQLFixture(t)
	f.conflicts = 1
	receipt, err := publishManagedSQLRoute(context.Background(), f.k, f.in, f.observe, nil)
	if err != nil || receipt.Selector != "ep-test" || receipt.NativeInitializationSHA256 == "" {
		t.Fatal("publication failed", err)
	}
	puts, _ := f.counts()
	if puts != 2 {
		t.Fatal("CAS409 did not use bounded reread")
	}
	f.mu.Lock()
	raw, _ := secretText(f.objects[f.k.path("secret", routesSecret)], "routes.json")
	f.mu.Unlock()
	var routes map[string]json.RawMessage
	json.Unmarshal([]byte(raw), &routes)
	var other map[string]any
	json.Unmarshal(routes["ep-unrelated"], &other)
	if other["keep"] != "unchanged" || other["extra"] != true {
		t.Fatal("unrelated route lost")
	}
	var own map[string]any
	json.Unmarshal(routes["ep-test"], &own)
	if _, ok := own["allowed_ips"]; ok {
		t.Fatal("initial publisher injected IP policy")
	}
	if _, ok := own["pageserver_node_id"]; ok {
		t.Fatal("initial publisher guessed PS node")
	}
	if own["vm_template"] != nil || own["role"] != "app_owner" {
		t.Fatal("legacy authority introduced")
	}
	// 同一Branch的既有保留服务角色不能在冷republish时丢失。
	// 既有空CIDR是deny-all，未知VPC/网络字段、状态和既有节点不能因刷新丢失或扩大。
	own["allowed_ips"] = []string{}
	own["allowed_vpc_endpoint_ids"] = []string{"vpce-fixture"}
	own["network_acl"] = map[string]any{"default_action": "deny", "source": "frozen-fixture-policy"}
	own["pageserver_node_id"] = float64(19)
	own["state"] = "suspended"
	roles := own["roles"].(map[string]any)
	roles[dataAPILogin("br_test")] = roles["app_owner"]
	ownRaw, _ := json.Marshal(own)
	routes["ep-test"] = ownRaw
	all, _ := json.Marshal(routes)
	f.mutate(func(objects map[string]map[string]any) {
		objects[f.k.path("secret", routesSecret)]["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(all)
	})
	if _, err = publishManagedSQLRoute(context.Background(), f.k, f.in, f.observe, nil); err != nil {
		t.Fatal("idempotent republish failed", err)
	}
	f.mu.Lock()
	raw, _ = secretText(f.objects[f.k.path("secret", routesSecret)], "routes.json")
	f.mu.Unlock()
	json.Unmarshal([]byte(raw), &routes)
	json.Unmarshal(routes["ep-test"], &own)
	if ips, ok := own["allowed_ips"].([]any); !ok || ips == nil || len(ips) != 0 {
		t.Fatal("empty CIDR policy widened")
	}
	if own["state"] != "suspended" || own["pageserver_node_id"] != float64(19) || own["network_acl"].(map[string]any)["default_action"] != "deny" || own["allowed_vpc_endpoint_ids"].([]any)[0] != "vpce-fixture" {
		t.Fatal("existing network policy or node was discarded")
	}
	if own["roles"].(map[string]any)[dataAPILogin("br_test")] == nil {
		t.Fatal("reserved service role dropped")
	}
}

func TestManagedSQLRouteMissingBranchBeforeAnyRead(t *testing.T) {
	f := newManagedSQLFixture(t)
	f.in.Binding.BranchID = ""
	if _, err := publishManagedSQLRoute(context.Background(), f.k, f.in, f.observe, nil); err == nil {
		t.Fatal("empty Branch accepted")
	}
	puts, reads := f.counts()
	if puts != 0 || reads != 0 {
		t.Fatal("empty Branch reached Kubernetes")
	}
	raw, _ := json.Marshal(f.in)
	if _, err := PublishManagedSQLRoute(context.Background(), "/must-not-be-opened", raw); err == nil {
		t.Fatal("CLI empty Branch accepted")
	}
}

func TestManagedSQLRouteNativeAndSourceFailuresNeverPublish(t *testing.T) {
	for _, name := range []string{"native503", "tenant", "startup", "roles", "permit", "branch", "credential-uid", "credential-data", "vm-after-native", "lease", "foreign-route", "invalid-policy", "put500"} {
		t.Run(name, func(t *testing.T) {
			f := newManagedSQLFixture(t)
			observe := f.observe
			var authorize func(context.Context) error
			switch name {
			case "native503":
				observe = func(context.Context, managedSQLRouteInput, managedSQLRouteSource) (dsgaInitializationResponse, error) {
					return dsgaInitializationResponse{}, errManagedSQLPublish
				}
			case "tenant":
				f.fact.TenantID = strings.Repeat("3", 32)
			case "startup":
				f.fact.Startup.MaximumBufferBytes++
			case "roles":
				f.fact.OrdinaryRolesVerified = false
			case "permit":
				f.fact.VMShrinkPermit = true
			case "branch":
				f.in.Binding.BranchID = "br_other"
			case "credential-uid":
				f.in.Binding.CredentialSecretUID = "changed-credentials"
			case "credential-data":
				f.in.Binding.CredentialDataSHA256 = strings.Repeat("a", 64)
			case "vm-after-native":
				observe = func(ctx context.Context, in managedSQLRouteInput, s managedSQLRouteSource) (dsgaInitializationResponse, error) {
					f.mutate(func(objects map[string]map[string]any) {
						objects[f.k.path("vm", "cp-test")]["metadata"].(map[string]any)["uid"] = "new-vm"
					})
					return f.fact, nil
				}
			case "lease":
				authorize = func(context.Context) error { return errors.New("fixture expired") }
			case "foreign-route":
				raw := []byte("{\"ep-test\":{\"project_id\":\"prj_foreign\",\"branch_id\":\"br_foreign\"}}")
				f.mutate(func(objects map[string]map[string]any) {
					objects[f.k.path("secret", routesSecret)]["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(raw)
				})
			case "invalid-policy":
				// 先真实发布同一绑定，再给既有策略注入非法CIDR；拒绝下一次写而不修复它。
				if _, err := publishManagedSQLRoute(context.Background(), f.k, f.in, f.observe, nil); err != nil {
					t.Fatal("policy setup failed")
				}
				f.mu.Lock()
				raw, _ := secretText(f.objects[f.k.path("secret", routesSecret)], "routes.json")
				var routes map[string]map[string]any
				json.Unmarshal([]byte(raw), &routes)
				routes["ep-test"]["allowed_ips"] = []string{"not-a-network"}
				encoded, _ := json.Marshal(routes)
				f.objects[f.k.path("secret", routesSecret)]["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
				f.puts = 0
				f.mu.Unlock()
			case "put500":
				f.putFailure = 500
			}
			if _, err := publishManagedSQLRoute(context.Background(), f.k, f.in, observe, authorize); err == nil {
				t.Fatal("unsafe publication accepted")
			}
			puts, _ := f.counts()
			if name == "put500" {
				if puts != 1 {
					t.Fatal("unknown PUT was retried")
				}
			} else if puts != 0 {
				t.Fatal("failure published route")
			}
		})
	}
}

func TestManagedSQLRouteActualBoundNativeGETAnd503(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newManagedSQLFixture(t)
			var nativeCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nativeCalls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/dsg/v1/initialization" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					t.Error("wrong native request")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(status)
				if status == 200 {
					json.NewEncoder(w).Encode(f.fact)
				} else {
					io.WriteString(w, "{}")
				}
			}))
			defer server.Close()
			transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}}
			client := &http.Client{Transport: transport}
			defer client.CloseIdleConnections()
			_, err := publishManagedSQLRoute(context.Background(), f.k, f.in, func(ctx context.Context, in managedSQLRouteInput, s managedSQLRouteSource) (dsgaInitializationResponse, error) {
				return observeManagedSQLNative(ctx, client, in, s)
			}, nil)
			puts, _ := f.counts()
			if nativeCalls.Load() != 1 || (status == 200 && (err != nil || puts != 1)) || (status == 503 && (err == nil || puts != 0)) {
				t.Fatal("native gate did not control publication")
			}
		})
	}
}
