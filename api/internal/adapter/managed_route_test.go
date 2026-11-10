package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmfake "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned/fake"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// 全部身份/verifier均为synthetic fixture；不读取生产Secret或宿主网络。
type managedRouteTestKube struct {
	objects map[string]map[string]any
	writes  int
	reads   []string
}

func (k *managedRouteTestKube) Request(ctx context.Context, method, path string, body any) (map[string]any, error) {
	if method != http.MethodGet {
		k.writes++
		return nil, APIError{Status: 403}
	}
	k.reads = append(k.reads, path)
	if v, ok := k.objects[path]; ok {
		return copyMap(v), nil
	}
	return nil, APIError{Status: 404}
}
func managedRouteMap(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		t.Fatal("invalid fixture")
	}
	return m
}
func managedRouteHash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }

func managedRouteFixture(t *testing.T) (*Server, *managedRouteTestKube, route, ManagedSQLBinding) {
	t.Helper()
	return managedRouteFixtureVersion(t, 2)
}

func managedRouteFixtureVersion(t *testing.T, version uint32) (*Server, *managedRouteTestKube, route, ManagedSQLBinding) {
	t.Helper()
	ctx := context.Background()
	controller, immutable := true, true
	vm := new(vmv1.VirtualMachine)
	vm.Name, vm.Namespace, vm.UID, vm.ResourceVersion = "cp-test", "testing", "vm-uid", "1"
	vm.Labels = map[string]string{"neon-control/project-id": "prj_test", "neon-control/endpoint-id": "ep_test"}
	vm.Spec.Guest.Ports = []vmv1.Port{{Name: "postgres", Port: 55432, Protocol: vmv1.ProtocolTCP}}
	vm.Spec.DSGABootstrap = &vmv1.DSGABootstrap{ProtocolVersion: 1, Incarnation: "1234567890abcdef1234567890abcdef", ComputeInstanceID: "compute-test"}
	clients := vmfake.NewSimpleClientset(vm)
	store, err := journal.NewStore(clients.NeonvmV1().VirtualMachines(vm.Namespace), journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: vm.Spec.DSGABootstrap.Incarnation})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Initialize(ctx, "1", "fixture-owner")
	if err != nil {
		t.Fatal(err)
	}
	snap, err = store.ReleaseBootstrap(ctx, snap.ResourceVersion(), "config-uid")
	if err != nil {
		t.Fatal(err)
	}
	vm = snap.VirtualMachine()
	vm.Status.Phase = "Running"
	encoded := base64.StdEncoding.EncodeToString(make([]byte, 32))
	verifier := "SCRAM-SHA-256$4096:" + base64.StdEncoding.EncodeToString(make([]byte, 16)) + "$" + encoded + ":" + encoded
	dynamic := map[string]any{"protocol_version": version, "vm_uid": "vm-uid", "compute_instance_id": "compute-test",
		"bootstrap_buffer_bytes": uint64(1 << 20), "maximum_buffer_bytes": uint64(2 << 20), "minimum_guest_memory_bytes": uint64(1 << 30), "non_buffer_reserve_bytes": uint64(1 << 28)}
	document := map[string]any{"spec": map[string]any{"project_id": "prj_test", "endpoint_id": "ep_test", "branch_id": "br_test",
		"tenant_id": strings.Repeat("1", 32), "timeline_id": strings.Repeat("2", 32), "operation_uuid": "fixture-operation",
		"dynamic_shared_buffers": dynamic, "cluster": map[string]any{"roles": []any{map[string]any{"name": "app_owner", "encrypted_password": verifier}, map[string]any{"name": "control_probe", "encrypted_password": verifier}}}}}
	raw, _ := json.Marshal(document)
	config := &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name + "-config", UID: "config-uid", ResourceVersion: "1", Labels: vm.Labels,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: vm.Name, UID: vm.UID, Controller: &controller}}},
		Immutable: &immutable, Data: map[string][]byte{"config.json": raw}}
	svc := &core.Service{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name, UID: "service-uid", ResourceVersion: "1"},
		Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, ClusterIP: "10.0.0.2", Selector: map[string]string{"vm.neon.tech/name": vm.Name},
			Ports: []core.ServicePort{{Name: "postgres", Port: 55432, Protocol: core.ProtocolTCP, TargetPort: intstr.FromString("postgres")}}}}
	b := ManagedSQLBinding{ProtocolVersion: 1, Namespace: vm.Namespace, ProjectID: "prj_test", EndpointID: "ep_test", BranchID: "br_test", TenantID: strings.Repeat("1", 32), TimelineID: strings.Repeat("2", 32),
		VMUID: "vm-uid", Incarnation: vm.Spec.DSGABootstrap.Incarnation, ComputeInstanceID: "compute-test", ConfigSecretUID: "config-uid", ConfigSHA256: managedRouteHash(raw), SecretDataSHA256: ManagedSQLSecretDataSHA256(config.Data),
		CredentialSecretUID: "credential-uid", CredentialDataSHA256: strings.Repeat("a", 64), ServiceUID: "service-uid", NativeInitializationSHA256: strings.Repeat("b", 64)}
	bindingRaw, _ := json.Marshal(b)
	item := route{ProjectID: b.ProjectID, BranchID: b.BranchID, TenantID: b.TenantID, Kind: "neonvm", Workload: vm.Name, Address: vm.Name + "." + vm.Namespace + ".svc.cluster.local:55432",
		Role: "app_owner", Verifier: verifier, Roles: map[string]string{"app_owner": verifier, "control_probe": verifier}, AllowedIPs: []string{"0.0.0.0/0"}, NodeID: 2, ManagedBinding: bindingRaw}
	k := &managedRouteTestKube{objects: map[string]map[string]any{}}
	s := &Server{config: Config{Namespace: vm.Namespace, RoutesSecret: "fixture-routes"}, kube: k}
	k.objects[s.path("vm", vm.Name)] = managedRouteMap(t, vm)
	k.objects[s.path("secrets", vm.Name+"-config")] = managedRouteMap(t, config)
	k.objects[s.path("services", vm.Name)] = managedRouteMap(t, svc)
	routeRaw, _ := json.Marshal(item)
	routesRaw, _ := json.Marshal(map[string]json.RawMessage{"ep-test": routeRaw})
	k.objects[s.path("secrets", "fixture-routes")] = map[string]any{"data": map[string]any{"routes.json": base64.StdEncoding.EncodeToString(routesRaw)}}
	return s, k, item, b
}

func TestManagedSQLRouteSealedSourceAndReadonlyWake(t *testing.T) {
	s, k, item, _ := managedRouteFixture(t)
	if err := s.validateRoute("ep-test", item); err != nil {
		t.Fatal(err)
	}
	if err := s.validateManagedSQLRoute(context.Background(), "ep-test", item); err != nil {
		t.Fatal(err)
	}
	if cold, err := s.wakeWorkload(context.Background(), "ep-test", item); err != nil || cold {
		t.Fatal("managed readonly wake failed", err)
	}
	for _, path := range k.reads {
		if strings.Contains(path, "credentials") {
			t.Fatal("adapter read signing credential source")
		}
	}
	if k.writes != 0 {
		t.Fatal("managed wake wrote Kubernetes")
	}
	delete(k.objects, s.path("vm", item.Workload))
	if _, err := s.wakeWorkload(context.Background(), "ep-test", item); err == nil || k.writes != 0 {
		t.Fatal("missing managed VM did not fail closed")
	}
}

func TestManagedSQLRouteRejectsSourceAndBindingDrift(t *testing.T) {
	cases := []string{"branch-empty", "binding-null", "unknown-version", "native-empty", "config-hash", "secret-data-hash", "config-branch", "vm-uid", "incarnation", "config-uid", "service-uid", "legacy-port", "wrong-target", "selector", "guest-port", "role-verifier", "template"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s, k, item, b := managedRouteFixture(t)
			vm := k.objects[s.path("vm", item.Workload)]
			cfg := k.objects[s.path("secrets", item.Workload+"-config")]
			svc := k.objects[s.path("services", item.Workload)]
			setBinding := true
			switch name {
			case "branch-empty":
				b.BranchID = ""
			case "binding-null":
				item.ManagedBinding = json.RawMessage("null")
				setBinding = false
			case "unknown-version":
				b.ProtocolVersion = 2
			case "native-empty":
				b.NativeInitializationSHA256 = ""
			case "config-hash":
				b.ConfigSHA256 = strings.Repeat("c", 64)
			case "secret-data-hash":
				b.SecretDataSHA256 = strings.Repeat("c", 64)
			case "config-branch":
				raw, _ := base64.StdEncoding.DecodeString(nested(cfg, "data", "config.json").(string))
				var d map[string]any
				json.Unmarshal(raw, &d)
				d["spec"].(map[string]any)["branch_id"] = "br_other"
				raw, _ = json.Marshal(d)
				cfg["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(raw)
				b.ConfigSHA256 = managedRouteHash(raw)
				var configData map[string][]byte
				encoded, _ := json.Marshal(cfg["data"])
				json.Unmarshal(encoded, &configData)
				b.SecretDataSHA256 = ManagedSQLSecretDataSHA256(configData)
			case "vm-uid":
				vm["metadata"].(map[string]any)["uid"] = "another-uid"
			case "incarnation":
				b.Incarnation = strings.Repeat("3", 32)
			case "config-uid":
				b.ConfigSecretUID = "another-config"
			case "service-uid":
				b.ServiceUID = "another-service"
			case "legacy-port":
				svc["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)["port"] = float64(55433)
			case "wrong-target":
				svc["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)["targetPort"] = "elsewhere"
			case "selector":
				svc["spec"].(map[string]any)["selector"].(map[string]any)["extra"] = "foreign"
			case "guest-port":
				vm["spec"].(map[string]any)["guest"].(map[string]any)["ports"].([]any)[0].(map[string]any)["port"] = float64(55433)
			case "role-verifier":
				item.Roles["control_probe"] = "not-a-verifier"
			case "template":
				item.Template = map[string]any{"kind": "VirtualMachine"}
			}
			if setBinding {
				item.ManagedBinding, _ = json.Marshal(b)
			}
			if s.validateRoute("ep-test", item) == nil && s.validateManagedSQLRoute(context.Background(), "ep-test", item) == nil {
				t.Fatal("accepted drift")
			}
			if k.writes != 0 {
				t.Fatal("rejection wrote Kubernetes")
			}
		})
	}
}

func TestManagedSQLRouteSameRouteIncludesFullBinding(t *testing.T) {
	s, k, item, b := managedRouteFixture(t)
	if s.sameRoute(context.Background(), "ep-test", item) != nil {
		t.Fatal("valid route rejected")
	}
	b.VMUID = "changed-vm"
	changed := item
	changed.ManagedBinding, _ = json.Marshal(b)
	raw, _ := json.Marshal(map[string]route{"ep-test": changed})
	k.objects[s.path("secrets", "fixture-routes")]["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(raw)
	if s.sameRoute(context.Background(), "ep-test", item) == nil {
		t.Fatal("changed managed binding was ignored")
	}
}

func TestManagedSQLRouteLegacyPortAndStrictSchema(t *testing.T) {
	s, _, item, _ := managedRouteFixture(t)
	item.ManagedBinding = nil
	item.Role = "cloud_admin"
	item.Address = item.Workload + ".testing.svc.cluster.local:55433"
	if s.validateRoute("ep-test", item) != nil {
		t.Fatal("legacy55433 changed")
	}
	item.Address = item.Workload + ".testing.svc.cluster.local:55432"
	if s.validateRoute("ep-test", item) == nil {
		t.Fatal("legacy rule widened")
	}
	s, k, item, _ := managedRouteFixture(t)
	raw, _ := json.Marshal(item)
	var d map[string]any
	json.Unmarshal(raw, &d)
	d["managed_binding"].(map[string]any)["unknown_contract"] = true
	raw, _ = json.Marshal(d)
	readsBefore := len(k.reads)
	if ValidateManagedSQLRoute(context.Background(), k, "testing", "ep-test", raw) == nil {
		t.Fatal("unknown managed field accepted")
	}
	if len(k.reads) != readsBefore {
		t.Fatal("unknown member reached live source; negative was masked")
	}
	if k.writes != 0 {
		t.Fatal("strict schema wrote")
	}
	t.Run("actual_registry_reader", func(t *testing.T) {
		s, k, item, _ := managedRouteFixture(t)
		record, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		wrap := func(value []byte) []byte {
			return []byte("{\"ep-test\":" + string(value) + "}")
		}
		registry := func(raw []byte) {
			k.objects[s.path("secrets", "fixture-routes")] = map[string]any{"data": map[string]any{"routes.json": base64.StdEncoding.EncodeToString(raw)}}
		}
		unknown := strings.Replace(string(record), "\"protocol_version\":1", "\"unknown_contract\":true,\"protocol_version\":1", 1)
		if unknown == string(record) {
			t.Fatal("managed fixture marker missing")
		}
		cases := map[string][]byte{
			"duplicate_endpoint":      []byte("{\"ep-test\":" + string(record) + ",\"ep-test\":" + string(record) + "}"),
			"duplicate_binding":       wrap([]byte(strings.TrimSuffix(string(record), "}") + ",\"managed_binding\":" + string(item.ManagedBinding) + "}")),
			"unknown_binding":         wrap([]byte(unknown)),
			"null_binding":            wrap([]byte(strings.Replace(string(record), string(item.ManagedBinding), "null", 1))),
			"duplicate_nested_policy": wrap([]byte(strings.TrimSuffix(string(record), "}") + ",\"opaque\":{\"policy\":1,\"policy\":2}}")),
			"trailing":                append(wrap(record), []byte("{}")...),
			"oversized":               []byte(strings.Repeat(" ", (2<<20)+1)),
		}
		for name, raw := range cases {
			t.Run(name, func(t *testing.T) {
				registry(raw)
				readsBefore := len(k.reads)
				if _, err := s.routes(context.Background()); err == nil {
					t.Fatal("invalid registry accepted by actual reader")
				}
				if len(k.reads) != readsBefore+1 || k.writes != 0 {
					t.Fatal("static reader probed live source or wrote")
				}
			})
		}
		registry(wrap(record))
		if routes, err := s.routes(context.Background()); err != nil || len(routes) != 1 {
			t.Fatal("canonical managed registry rejected")
		}
		// 1MiB普通写协议限额不能误套到原2MiB只读registry。
		large := []byte(strings.TrimSuffix(string(record), "}") + ",\"opaque_policy\":\"" + strings.Repeat("x", (1<<20)+1) + "\"}")
		if len(large) <= 1<<20 || len(wrap(large)) > 2<<20 {
			t.Fatal("large registry fixture outside intended bounds")
		}
		registry(wrap(large))
		if routes, err := s.routes(context.Background()); err != nil || len(routes) != 1 {
			t.Fatal("valid registry above 1MiB rejected")
		}
		// reader只校Binding静态合同；route一致性仍由原selected路径拒绝。
		deferred := item
		deferred.Address = "invalid-unselected-address"
		deferredRaw, _ := json.Marshal(deferred)
		registry(wrap(deferredRaw))
		routes, err := s.routes(context.Background())
		if err != nil || len(routes) != 1 {
			t.Fatal("reader prematurely checked selected route fields")
		}
		if s.validateRoute("ep-test", routes["ep-test"]) == nil {
			t.Fatal("selected route consistency was weakened")
		}
		legacy := item
		legacy.ManagedBinding, legacy.Role = nil, "cloud_admin"
		legacy.Address = legacy.Workload + ".testing.svc.cluster.local:55433"
		legacyRaw, _ := json.Marshal(legacy)
		// 原合法legacy opaque字段可深于managed递归限制，不能被新门缩窄。
		deep := strings.Repeat("{\"opaque\":", 48) + "true" + strings.Repeat("}", 48)
		legacyRaw = []byte(strings.TrimSuffix(string(legacyRaw), "}") + ",\"unknown_legacy\":" + deep + "}")
		registry(wrap(legacyRaw))
		routes, err = s.routes(context.Background())
		if err != nil || len(routes) != 1 || s.validateRoute("ep-test", routes["ep-test"]) != nil {
			t.Fatal("valid legacy semantics changed")
		}
		if k.writes != 0 {
			t.Fatal("registry reader wrote")
		}
	})
}

func TestManagedSQLRouteBudgetBootstrapSealedSource(t *testing.T) {
	s, _, item, _ := managedRouteFixtureVersion(t, 3)
	if err := s.validateManagedSQLRoute(context.Background(), "ep-test", item); err != nil {
		t.Fatal(err)
	}
	s, _, item, _ = managedRouteFixtureVersion(t, 4)
	if err := s.validateManagedSQLRoute(context.Background(), "ep-test", item); err == nil {
		t.Fatal("unknown bootstrap accepted by SQL route")
	}
}
