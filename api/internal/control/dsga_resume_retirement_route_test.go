package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/dsga/budget"
	"github.com/neondatabase/autoscaling/pkg/dsga/hostguard"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/vmguard"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDSGAResumeRetirementUsesBothCredentialDigests(t *testing.T) {
	ctx := context.Background()
	f := newManagedSQLFixture(t)
	if _, err := publishManagedSQLRoute(ctx, f.k, f.in, f.observe, nil); err != nil {
		t.Fatal(err)
	}
	b := f.in.Binding
	p := createPayload{ProjectID: b.ProjectID, EndpointID: b.EndpointID, BranchID: b.BranchID,
		TenantID: b.TenantID, TimelineID: b.TimelineID}
	source := dsgaResumeSource{VMUID: b.VMUID, ConfigUID: b.ConfigSecretUID, ServiceUID: b.ServiceUID,
		Incarnation: b.Incarnation, ComputeInstanceID: b.ComputeInstanceID}
	var credential core.Secret
	name := kubeName(p.EndpointID) + "-credentials"
	if err := managedSQLRead(f.k, ctx, "secret", name, &credential); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotDSGACredentials(&credential, b.Namespace, name, p.ProjectID, p.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	plan := dsgaCreationPlan{dsgaCreationIdentity: dsgaCreationIdentity{Namespace: b.Namespace}, Credentials: snapshot.reference()}
	if plan.Credentials.ContentSHA256 == b.CredentialDataSHA256 {
		t.Fatal("fixture requires the actual distinct digest domains")
	}
	s := &server{kube: f.k}
	if err := s.verifyDSGAUnpublishedResumeRoute(ctx, p, plan, source); err != nil {
		t.Fatalf("valid unchanged credential rejected: %v", err)
	}
	changed := plan
	changed.Credentials.ContentSHA256 = b.CredentialDataSHA256
	if err := s.verifyDSGAUnpublishedResumeRoute(ctx, p, changed, source); !errors.Is(err, errDSGACredentialSource) {
		t.Fatalf("route digest improperly substituted into the creation plan: %v", err)
	}
	f.mutate(func(objects map[string]map[string]any) {
		registry := objects[f.k.path("secret", routesSecret)]
		raw, _ := secretText(registry, "routes.json")
		var routes map[string]map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &routes)
		b.CredentialDataSHA256 = strings.Repeat("9", 64)
		routes[selector(p.EndpointID)]["managed_binding"], _ = json.Marshal(b)
		next, _ := json.Marshal(routes)
		registry["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(next)
	})
	if err := s.verifyDSGAUnpublishedResumeRoute(ctx, p, plan, source); !errors.Is(err, errDSGACreationIdentity) {
		t.Fatalf("a different route certificate was accepted: %v", err)
	}
	if n, _ := f.counts(); n != 1 {
		t.Fatalf("retirement verification wrote routing state: %d PUTs", n)
	}
}

func TestDSGAResumeRetirementIdentifiesOriginalEvictedRunner(t *testing.T) {
	controller := true
	deleting := metav1.Now()
	vm := &vmv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "neon", Name: "cp-original", UID: "original-vm"},
		Spec:   vmv1.VirtualMachineSpec{RestartPolicy: vmv1.RestartPolicyAlways, DSGABootstrap: &vmv1.DSGABootstrap{}},
		Status: vmv1.VirtualMachineStatus{Phase: vmv1.VmFailed, PodName: "original-runner"}}
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "neon", Name: "original-runner", UID: "original-pod", ResourceVersion: "123", DeletionTimestamp: &deleting,
		Finalizers:      []string{hostguard.PodProtectionFinalizer},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: vm.Name, UID: vm.UID, Controller: &controller}}},
		Spec: core.PodSpec{RestartPolicy: core.RestartPolicyNever, NodeName: "worker-original"},
		Status: core.PodStatus{Phase: core.PodFailed, Reason: "Evicted",
			ContainerStatuses: []core.ContainerStatus{{Name: "neonvm-runner", RestartCount: 1,
				State: core.ContainerState{Terminated: &core.ContainerStateTerminated{Reason: "ContainerStatusUnknown"}}}}}}
	if !isDSGAFailedResumeTerminalRunner(vm, pod) {
		t.Fatal("original evicted Runner could not be bound for a deletion request")
	}
	for name, change := range map[string]func(*vmv1.VirtualMachine, *core.Pod){
		"live VM":              func(v *vmv1.VirtualMachine, p *core.Pod) { v.Status.Phase = vmv1.VmRunning },
		"wrong node":           func(v *vmv1.VirtualMachine, p *core.Pod) { v.Status.Node = "replacement-node" },
		"wrong owner":          func(v *vmv1.VirtualMachine, p *core.Pod) { p.OwnerReferences[0].UID = "replacement-vm" },
		"unprotected":          func(v *vmv1.VirtualMachine, p *core.Pod) { p.Finalizers = nil },
		"unknown non-eviction": func(v *vmv1.VirtualMachine, p *core.Pod) { p.Status.Reason = "Error" },
	} {
		t.Run(name, func(t *testing.T) {
			v, p := vm.DeepCopy(), pod.DeepCopy()
			change(v, p)
			if isDSGAFailedResumeTerminalRunner(v, p) {
				t.Fatal("incorrect terminal VM/Runner binding accepted")
			}
		})
	}
}

func TestDSGAResumeRetirementRequiresRealBirthSourceNotFound(t *testing.T) {
	binding := journal.Binding{Namespace: "neon", Name: "cp-original", VMUID: "original-vm",
		Incarnation: "0123456789abcdef0123456789abcdef"}
	names := vmguard.BirthSourceNames(binding)
	for _, status := range []int{http.StatusNotFound, http.StatusOK, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			paths := make(chan string, 2)
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths <- r.Method + " " + r.URL.Path
				w.WriteHeader(status)
				w.Write([]byte("{}"))
			}))
			defer h.Close()
			k := &kubeClient{http: h.Client(), base: h.URL, namespace: binding.Namespace}
			err := confirmDSGAFailedResumeUnpublished(context.Background(), k, nil, binding)
			if (err == nil) != (status == http.StatusNotFound) {
				t.Fatalf("HTTP %d produced incorrect absence result: %v", status, err)
			}
			want := "GET /api/v1/namespaces/neon/configmaps/"
			if got := <-paths; got != want+names[0] {
				t.Fatalf("wrong immutable birth source request: %s", got)
			}
			if status == http.StatusNotFound && len(paths) != 1 {
				t.Fatal("both independently named sources must be checked")
			}
			if len(paths) != 0 && <-paths != want+names[1] {
				t.Fatal("wrong immutable Runner state source")
			}
		})
	}
}

func TestDSGAResumeRetirementRetainsRegisteredEmptyRunnerState(t *testing.T) {
	binding := journal.Binding{Namespace: "neon", Name: "cp-original", VMUID: "original-vm",
		Incarnation: "0123456789abcdef0123456789abcdef"}
	vm := &vmv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: binding.Namespace, Name: binding.Name, UID: binding.VMUID}}
	names := vmguard.BirthSourceNames(binding)
	birthRef := budget.ObjectRef{Name: names[0], UID: "original-birth"}
	stateRef := budget.ObjectRef{Name: names[1], UID: "original-state"}
	id := vmguard.Identity{VMUID: string(vm.UID), Incarnation: binding.Incarnation, NodeUID: "original-node",
		RunnerPodUID: "original-pod", RunnerBootID: "1234567890abcdef1234567890abcdef",
		MinimumBytes: 8 << 20, MaximumBytes: 16 << 20, BlockBytes: 8 << 20}
	birth := vmguard.Birth{ProtocolVersion: 1, Identity: id, StateClaim: stateRef}
	controller := true
	for _, mode := range []string{"registered", "published", "missing-state", "replaced-state"} {
		t.Run(mode, func(t *testing.T) {
			state := vmguard.Publication{ProtocolVersion: 1, Identity: id}
			if mode == "published" {
				state.BindingClaim = &birthRef
			}
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var ref budget.ObjectRef
				var value any
				var kind string
				immutable := false
				switch r.URL.Path {
				case "/api/v1/namespaces/neon/configmaps/" + names[0]:
					ref, value, kind, immutable = birthRef, birth, "binding-v1", true
				case "/api/v1/namespaces/neon/configmaps/" + names[1]:
					if mode == "missing-state" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					ref, value, kind = stateRef, state, "state-v1"
					if mode == "replaced-state" {
						ref.UID = "replaced-state"
					}
				default:
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				raw, _ := json.Marshal(value)
				cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: binding.Namespace, Name: ref.Name, UID: types.UID(ref.UID), ResourceVersion: "1",
					Labels: map[string]string{vmguard.ClaimLabel: kind}, Finalizers: []string{vmguard.ProtectionFinalizer},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: vm.Name,
						UID: vm.UID, Controller: &controller, BlockOwnerDeletion: &controller}}},
					Immutable: &immutable, Data: map[string]string{vmguard.ClaimKey: string(raw)}}
				_ = json.NewEncoder(w).Encode(cm)
			}))
			defer h.Close()
			k := &kubeClient{http: h.Client(), base: h.URL, namespace: binding.Namespace}
			if err := confirmDSGAFailedResumeUnpublished(context.Background(), k, vm, binding); (err == nil) != (mode == "registered") {
				t.Fatalf("wrong disposition for original Runner: %v", err)
			}
		})
	}
}
