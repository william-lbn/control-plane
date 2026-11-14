package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/dsga/hostguard"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
