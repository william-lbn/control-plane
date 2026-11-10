package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// 本机TLS/RFC6902协议夹具：调用真实typed客户端/Store，不重写journal编码。
// 只模拟VM/Secret及UID/RV线性化点，不冒充apiserver/CRD/RBAC认证；Render配置
// 只供身份契约检查，并非可启动的完整Neon spec。PG/预算实际端到端另行验证。
type creationTestAPI struct {
	mu            sync.Mutex
	kube          *kubeClient
	vm            *vmv1.VirtualMachine
	secret        *core.Secret
	bound         types.UID
	boundConfig   types.UID
	version       int
	trace         []string
	lost          string
	beforePatch   func()
	afterRender   func()
	bindErr       error
	bindConfigErr error
}

func creationStatus(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: int32(code)})
}

func (f *creationTestAPI) next() string {
	f.version++
	return strconv.Itoa(f.version)
}

func (f *creationTestAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer creation-kube-identity" {
		creationStatus(w, http.StatusForbidden)
		return
	}
	var object any
	phase := ""
	switch {
	case r.Method == http.MethodGet && r.URL.Path == f.kube.path("vm", brokerTestWorkload):
		if f.vm == nil {
			creationStatus(w, http.StatusNotFound)
			return
		}
		object = f.vm
	case r.Method == http.MethodGet && r.URL.Path == f.kube.path("secret", brokerTestWorkload+"-config"):
		if f.secret == nil {
			creationStatus(w, http.StatusNotFound)
			return
		}
		object = f.secret
	case r.Method == http.MethodPost && r.URL.Path == f.kube.path("vm", ""):
		if f.vm != nil {
			creationStatus(w, http.StatusConflict)
			return
		}
		vm := new(vmv1.VirtualMachine)
		if json.NewDecoder(r.Body).Decode(vm) != nil || vm.Spec.DSGABootstrap == nil || vm.Spec.DSGABootstrap.Ready {
			creationStatus(w, http.StatusBadRequest)
			return
		}
		vm.UID, vm.ResourceVersion = "vm-uid", f.next()
		f.vm, phase, object = vm, "create", vm
	case r.Method == http.MethodPatch && r.URL.Path == f.kube.path("vm", brokerTestWorkload):
		if f.beforePatch != nil {
			hook := f.beforePatch
			f.beforePatch = nil
			hook()
		}
		if f.vm == nil || r.Header.Get("Content-Type") != "application/json-patch+json" {
			creationStatus(w, http.StatusConflict)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		patch, err := jsonpatch.DecodePatch(raw)
		if err != nil {
			creationStatus(w, http.StatusBadRequest)
			return
		}
		before, _ := json.Marshal(f.vm)
		after, err := patch.Apply(before)
		if err != nil {
			creationStatus(w, http.StatusConflict)
			return
		}
		vm := new(vmv1.VirtualMachine)
		if json.Unmarshal(after, vm) != nil {
			creationStatus(w, http.StatusBadRequest)
			return
		}
		phase = "initialize"
		if vm.Spec.DSGABootstrap.Ready {
			phase = "release"
		}
		vm.ResourceVersion = f.next()
		f.vm, object = vm, vm
	case r.Method == http.MethodPost && r.URL.Path == f.kube.path("secret", ""):
		if f.secret != nil {
			creationStatus(w, http.StatusConflict)
			return
		}
		secret := new(core.Secret)
		if json.NewDecoder(r.Body).Decode(secret) != nil {
			creationStatus(w, http.StatusBadRequest)
			return
		}
		secret.UID, secret.ResourceVersion = "config-uid", f.next()
		f.secret, phase, object = secret, "config", secret
	default:
		creationStatus(w, http.StatusBadRequest)
		return
	}
	if phase != "" {
		f.trace = append(f.trace, phase)
	}
	if phase != "" && f.lost == phase {
		f.lost = ""
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(object)
}

func newCreationTest(t *testing.T) (*creationTestAPI, dsgaCreationInput) {
	t.Helper()
	f := &creationTestAPI{kube: &kubeClient{namespace: "neon", token: "creation-kube-identity"}}
	server := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.kube.base, f.kube.http = server.URL, server.Client()
	p := createPayload{ProjectID: "prj_test", EndpointID: "ep_0123456789abcdef"}
	vm := &vmv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: brokerTestWorkload, Namespace: "neon", Labels: credentialLabels(p.ProjectID, p.EndpointID)}}
	vm.Spec.DSGABootstrap = &vmv1.DSGABootstrap{ProtocolVersion: 1, Incarnation: strings.Repeat("a", 32), ComputeInstanceID: "compute-1", VMUID: "", ConfigSecretUID: "", Ready: false}
	input := dsgaCreationInput{Payload: p, VM: vm, ExpectedVMUID: "", Bind: func(_ context.Context, actual *vmv1.VirtualMachine) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.bindErr != nil {
			return f.bindErr
		}
		if f.bound != "" && f.bound != actual.UID {
			return errDSGACreationIdentity
		}
		f.bound = actual.UID
		if len(f.trace) == 1 {
			f.trace = append(f.trace, "bind")
		}
		return nil
	}, ExpectedConfigUID: "", BindConfig: func(_ context.Context, actual *core.Secret) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.bindConfigErr != nil {
			return f.bindConfigErr
		}
		if f.boundConfig != "" && f.boundConfig != actual.UID {
			return errDSGACreationIdentity
		}
		if f.boundConfig == "" {
			f.trace = append(f.trace, "bind-config")
		}
		f.boundConfig = actual.UID
		return nil
	}, Render: func(_ context.Context, actual *vmv1.VirtualMachine) (map[string][]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.bound == "" || f.bound != actual.UID || !dsgapi.Managed(actual.Annotations) {
			return nil, errors.New("render before original UID or journal")
		}
		if f.afterRender != nil {
			hook := f.afterRender
			f.afterRender = nil
			hook()
		}
		return map[string][]byte{"config.json": []byte(fmt.Sprintf(`{"spec":{"dynamic_shared_buffers":{"protocol_version":2,"vm_uid":%q,"compute_instance_id":%q,"bootstrap_buffer_bytes":8388608,"maximum_buffer_bytes":16777216,"minimum_guest_memory_bytes":1073741824,"non_buffer_reserve_bytes":536870912}}}`, actual.UID, actual.Spec.DSGABootstrap.ComputeInstanceID)), "control.key": []byte("only-test-private-key")}, nil
	}}
	return f, input
}

func TestDSGACreationPersistedOrderAndReadyReadbackDoesNotRewrite(t *testing.T) {
	f, input := newCreationTest(t)
	vm, err := f.kube.prepareDSGACreation(context.Background(), input)
	if err != nil || vm == nil || !vm.Spec.DSGABootstrap.Ready {
		t.Fatalf("creation failed: %v", err)
	}
	f.mu.Lock()
	want := []string{"create", "bind", "initialize", "config", "bind-config", "release"}
	if !reflect.DeepEqual(f.trace, want) || f.secret.Immutable == nil || !*f.secret.Immutable || f.secret.OwnerReferences[0].UID != vm.UID {
		t.Fatalf("creation order or immutable ownership differs: %v", f.trace)
	}
	journalBefore := f.vm.Annotations[dsgapi.JournalAnnotation]
	f.vm.Status.Phase, f.vm.Status.PodName, f.vm.Status.Node = vmv1.VmRunning, "runner", "node"
	f.mu.Unlock()
	input.ExpectedVMUID = vm.UID
	input.ExpectedConfigUID = "config-uid"
	if _, err := f.kube.prepareDSGACreation(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.trace, want) || journalBefore != f.vm.Annotations[dsgapi.JournalAnnotation] {
		t.Fatal("ready readback reset history or wrote again")
	}
}

func TestDSGACreationLostWritesResumeOnlyOriginalFacts(t *testing.T) {
	for _, phase := range []string{"create", "initialize", "config", "release"} {
		t.Run(phase, func(t *testing.T) {
			f, input := newCreationTest(t)
			f.lost = phase
			if vm, err := f.kube.prepareDSGACreation(context.Background(), input); err == nil || vm != nil {
				t.Fatal("lost reply returned a successful creation")
			}
			f.mu.Lock()
			input.ExpectedVMUID = f.bound
			input.ExpectedConfigUID = f.boundConfig
			uid := f.vm.UID
			f.mu.Unlock()
			vm, err := f.kube.prepareDSGACreation(context.Background(), input)
			if err != nil || vm.UID != uid || !vm.Spec.DSGABootstrap.Ready {
				t.Fatalf("original creation did not resume: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, effect := range []string{"create", "initialize", "config", "release"} {
				count := 0
				for _, actual := range f.trace {
					if actual == effect {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("unknown %s was reset or repeated: %v", phase, f.trace)
				}
			}
		})
	}
}

func TestDSGACreationRejectsChangedObjectsWithoutResetOrRelease(t *testing.T) {
	for _, scenario := range []string{"vm_uid", "vm_missing", "config_uid", "config_missing", "config_owner", "config_data", "config_mutable", "journal_missing", "journal_bad", "incarnation", "compute_id", "deleting", "changed_after_render"} {
		t.Run(scenario, func(t *testing.T) {
			f, input := newCreationTest(t)
			vm, err := f.kube.prepareDSGACreation(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			input.ExpectedVMUID = vm.UID
			input.ExpectedConfigUID = "config-uid"
			f.mu.Lock()
			before := append([]string(nil), f.trace...)
			switch scenario {
			case "vm_uid":
				f.vm.UID = "same-name-new-vm"
			case "vm_missing":
				f.vm = nil
			case "config_uid":
				f.secret.UID = "same-name-new-config"
			case "config_missing":
				f.secret = nil
			case "config_owner":
				f.secret.OwnerReferences[0].UID = "foreign-vm"
			case "config_data":
				f.secret.Data["control.key"] = []byte("changed-key")
			case "config_mutable":
				f.secret.Immutable = nil
			case "journal_missing":
				delete(f.vm.Annotations, dsgapi.JournalAnnotation)
			case "journal_bad":
				f.vm.Annotations[dsgapi.JournalAnnotation] = "bad"
			case "incarnation":
				f.vm.Spec.DSGABootstrap.Incarnation = strings.Repeat("b", 32)
			case "compute_id":
				f.vm.Spec.DSGABootstrap.ComputeInstanceID = "new-compute"
			case "deleting":
				now := metav1.Now()
				f.vm.DeletionTimestamp = &now
			case "changed_after_render":
				f.afterRender = func() { f.vm.UID = "recreated-during-render" }
			}
			f.mu.Unlock()
			if result, err := f.kube.prepareDSGACreation(context.Background(), input); err == nil || result != nil {
				t.Fatalf("changed %s accepted", scenario)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !reflect.DeepEqual(f.trace, before) {
				t.Fatalf("changed %s caused reset or write: %v", scenario, f.trace)
			}
		})
	}
}

func TestDSGACreationConfigBindFailureNeverReleases(t *testing.T) {
	f, input := newCreationTest(t)
	f.bindConfigErr = errors.New("original config UID CAS failed")
	if vm, err := f.kube.prepareDSGACreation(context.Background(), input); err == nil || vm != nil {
		t.Fatal("config UID bind failure ignored")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vm == nil || f.vm.Spec.DSGABootstrap.Ready || !dsgapi.Managed(f.vm.Annotations) || f.secret == nil || !reflect.DeepEqual(f.trace, []string{"create", "bind", "initialize", "config"}) {
		t.Fatalf("config bind failure released or lost persisted facts: %v", f.trace)
	}
}

func TestDSGACreationMalformedIntentRejectedBeforeAPICreate(t *testing.T) {
	for _, scenario := range []string{"nil_vm", "uid", "rv", "name", "namespace", "labels", "already_managed", "ready", "zero_incarnation", "missing_compute", "no_bind", "no_config_bind", "no_render", "partial_binding"} {
		t.Run(scenario, func(t *testing.T) {
			f, input := newCreationTest(t)
			switch scenario {
			case "nil_vm":
				input.VM = nil
			case "uid":
				input.VM.UID = "forged"
			case "rv":
				input.VM.ResourceVersion = "forged"
			case "name":
				input.VM.Name = "other"
			case "namespace":
				input.VM.Namespace = "foreign"
			case "labels":
				input.VM.Labels["neon-control/project-id"] = "foreign"
			case "already_managed":
				input.VM.Annotations = map[string]string{dsgapi.JournalAnnotation: ""}
			case "ready":
				input.VM.Spec.DSGABootstrap.Ready = true
			case "zero_incarnation":
				input.VM.Spec.DSGABootstrap.Incarnation = strings.Repeat("0", 32)
			case "missing_compute":
				input.VM.Spec.DSGABootstrap.ComputeInstanceID = ""
			case "no_bind":
				input.Bind = nil
			case "no_config_bind":
				input.BindConfig = nil
			case "no_render":
				input.Render = nil
			case "partial_binding":
				input.ExpectedConfigUID = "config-uid"
			}
			if result, err := f.kube.prepareDSGACreation(context.Background(), input); !errors.Is(err, errDSGACreationIdentity) || result != nil {
				t.Fatalf("malformed %s accepted: %v", scenario, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.vm != nil || f.secret != nil || len(f.trace) != 0 {
				t.Fatal("malformed intent created resources")
			}
		})
	}
}

func TestDSGACreationHeldHistoryAndBadRenderNeverRelease(t *testing.T) {
	for _, scenario := range []string{"held_runner", "held_restart", "bad_json", "wrong_vm", "wrong_compute", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			f, input := newCreationTest(t)
			if strings.HasPrefix(scenario, "held_") {
				f.bindErr = errors.New("pause before initialization")
				if _, err := f.kube.prepareDSGACreation(context.Background(), input); err == nil {
					t.Fatal("pause unexpectedly succeeded")
				}
				f.mu.Lock()
				f.bindErr = nil
				input.ExpectedVMUID = f.vm.UID
				if scenario == "held_runner" {
					f.vm.Status.PodName = "old-runner"
				} else {
					f.vm.Status.RestartCount = 1
				}
				f.mu.Unlock()
			} else {
				render := input.Render
				input.Render = func(ctx context.Context, vm *vmv1.VirtualMachine) (map[string][]byte, error) {
					data, err := render(ctx, vm)
					if err != nil {
						return nil, err
					}
					switch scenario {
					case "bad_json":
						data["config.json"] = []byte("{")
					case "wrong_vm":
						data["config.json"] = []byte(strings.ReplaceAll(string(data["config.json"]), "vm-uid", "different-vm"))
					case "wrong_compute":
						data["config.json"] = []byte(strings.ReplaceAll(string(data["config.json"]), "compute-1", "different-compute"))
					case "oversized":
						data["too-large"] = make([]byte, 1<<20)
					}
					return data, nil
				}
			}
			if result, err := f.kube.prepareDSGACreation(context.Background(), input); err == nil || result != nil {
				t.Fatalf("unsafe %s accepted", scenario)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.vm == nil || f.vm.Spec.DSGABootstrap.Ready || f.secret != nil {
				t.Fatalf("unsafe %s released or wrote config: %v", scenario, f.trace)
			}
			if strings.HasPrefix(scenario, "held_") && dsgapi.Managed(f.vm.Annotations) {
				t.Fatal("held execution history was adopted into a new journal")
			}
		})
	}
}

func TestDSGACreationBindFailureLeavesHeldBeforeJournalAndConfig(t *testing.T) {
	f, input := newCreationTest(t)
	f.bindErr = errors.New("original operation lease lost")
	if vm, err := f.kube.prepareDSGACreation(context.Background(), input); err == nil || vm != nil {
		t.Fatal("bind failure ignored")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vm == nil || f.vm.Spec.DSGABootstrap.Ready || dsgapi.Managed(f.vm.Annotations) || f.secret != nil || !reflect.DeepEqual(f.trace, []string{"create"}) {
		t.Fatalf("bind failure progressed past held CREATE: %v", f.trace)
	}
}
