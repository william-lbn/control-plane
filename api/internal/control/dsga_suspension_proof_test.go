package control

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/hostguard"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDSGASuspensionHostRequiresOriginalNodeAndRetiredPlacement(t *testing.T) {
	binding := hostguard.Binding{NodeName: "worker-fixture", NodeUID: "node-1", Namespace: "platform",
		LeaseName: "host-lease", LeaseUID: "lease-1"}
	base := hostguard.Model{ProtocolVersion: 5, Binding: binding, Origin: strings.Repeat("b", 32),
		LeaseFloor: journal.Owner{Epoch: 1, HolderID: "owner-1"}, Sequence: 10, OutsideBytes: 1 << 30, Holds: []hostguard.Hold{},
		Profile: &hostguard.Profile{ID: "fixture", SchedulerName: "autoscale-scheduler", VMOverheadBytes: 64 << 20, PodOverheadBytes: 32 << 20}}
	node := func(sequence dsgapi.Uint64, placements []hostguard.Placement) *core.Node {
		t.Helper()
		m := base
		m.Sequence, m.Placements = sequence, &placements
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return &core.Node{ObjectMeta: metav1.ObjectMeta{Name: binding.NodeName, UID: binding.NodeUID, ResourceVersion: "1",
			Annotations: map[string]string{hostguard.LedgerAnnotation: string(wire), hostguard.OriginAnnotation: "1:" + base.Origin},
			Finalizers:  []string{hostguard.ProtectionFinalizer}}}
	}
	placement := hostguard.Placement{Namespace: "neon", PodName: "runner", PodUID: "runner-1",
		VMName: "compute", VMUID: "vm-1", Incarnation: strings.Repeat("a", 32), Managed: true,
		MemoryBytes: 4 << 30, HostOverheadBytes: 64 << 20}
	first := node(10, []hostguard.Placement{placement})
	original, _, err := readDSGASuspendedHost(first)
	if err != nil {
		t.Fatal(err)
	}
	stillHeld := node(11, []hostguard.Placement{placement})
	released := node(11, []hostguard.Placement{})
	if err := verifyDSGASuspensionHostSettlement(original, released, "vm-1", "runner-1"); err != nil {
		t.Fatal(err)
	}
	wrongNode := released.DeepCopy()
	wrongNode.UID = "node-2"
	badOrigin := released.DeepCopy()
	badOrigin.Annotations[hostguard.OriginAnnotation] = "1:" + strings.Repeat("c", 32)
	for _, rejected := range []*core.Node{first, stillHeld, wrongNode, badOrigin} {
		if err := verifyDSGASuspensionHostSettlement(original, rejected, "vm-1", "runner-1"); err == nil {
			t.Fatal("unconfirmed Host settlement was accepted")
		}
	}
}

func TestDSGASuspensionIntentPinsBootAndHostAcrossWorkerRetry(t *testing.T) {
	ctx, b, _, _, _, _, _, fact := initializationBindingFixture(t)
	p := dsgaSuspensionPayload{ProjectID: b.intent.ProjectID, BranchID: b.intent.BranchID, EndpointID: b.intent.EndpointID,
		CreationOperation: b.operationID, VMUID: fact.Observed.Instance.VMUID}
	binding := journal.Binding{Namespace: b.intent.Creation.Namespace, Name: kubeName(p.EndpointID),
		VMUID: types.UID(p.VMUID), Incarnation: b.intent.Creation.Incarnation}
	if _, err := b.server.db.Exec(ctx, "UPDATE operations SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", b.operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.server.db.Exec(ctx, "UPDATE endpoints SET state='active' WHERE id=$1", p.EndpointID); err != nil {
		t.Fatal(err)
	}
	id := "suspension-intent-test"
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.server.db.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at)
 VALUES($1,$2,'endpoint',$3,$4,'running','local-admin','suspension-test',$5,$6,clock_timestamp()+interval '5 minutes')`,
		id, p.ProjectID, p.EndpointID, dsgaSuspendAction, payload, b.workerID)
	if err != nil {
		t.Fatal(err)
	}
	selected := selectionFixture(t)
	selected.Namespace, selected.Name, selected.Incarnation = binding.Namespace, binding.Name, binding.Incarnation
	selected.Instance.VMUID, selected.Instance.ComputeInstanceID = p.VMUID, b.intent.Creation.ComputeInstanceID
	vm := &vmv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: binding.Namespace, Name: binding.Name, UID: binding.VMUID}}
	vm.Status.Node, vm.Status.PodName = "worker-test", "runner-original"
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: binding.Namespace, Name: vm.Status.PodName, UID: "runner-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(vm, vmv1.SchemeGroupVersion.WithKind("VirtualMachine"))}},
		Spec: core.PodSpec{NodeName: vm.Status.Node, SchedulerName: "autoscale-scheduler"}, Status: core.PodStatus{Phase: core.PodRunning}}
	placements := []hostguard.Placement{{Namespace: vm.Namespace, PodName: pod.Name, PodUID: string(pod.UID), VMName: vm.Name,
		VMUID: string(vm.UID), Incarnation: binding.Incarnation, Managed: true, MemoryBytes: 4 << 30, HostOverheadBytes: 64 << 20}}
	host := hostguard.Model{ProtocolVersion: 5, Binding: hostguard.Binding{NodeName: vm.Status.Node, NodeUID: "node-uid",
		Namespace: "platform", LeaseName: "host-lease", LeaseUID: "lease-uid"}, Origin: strings.Repeat("b", 32),
		LeaseFloor: journal.Owner{Epoch: 1, HolderID: "owner-1"}, Sequence: 10, OutsideBytes: 1 << 30,
		Holds: []hostguard.Hold{}, Placements: &placements,
		Profile: &hostguard.Profile{ID: "fixture", SchedulerName: pod.Spec.SchedulerName, VMOverheadBytes: 64 << 20, PodOverheadBytes: 32 << 20}}
	wire, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: host.Binding.NodeName, UID: host.Binding.NodeUID, ResourceVersion: "1",
		Annotations: map[string]string{hostguard.LedgerAnnotation: string(wire), hostguard.OriginAnnotation: "1:" + host.Origin},
		Finalizers:  []string{hostguard.ProtectionFinalizer}}}
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "selection-original"}}
	proposed, err := newDSGASuspensionIntent(id, vm, cm, selected, pod, node, binding)
	if err != nil || proposed.Selection.Next.Generation != selected.Generation+1 || proposed.Selection.Next.Automatic != nil ||
		proposed.Selection.Next.SuspensionOperation != id || proposed.Selection.PreviousSHA256 != selected.Digest() {
		t.Fatalf("original selection was not frozen: %v", err)
	}
	foreign := pod.DeepCopy()
	foreign.UID = "replacement-runner"
	if _, err := newDSGASuspensionIntent(id, vm, cm, selected, foreign, node, binding); err == nil {
		t.Fatal("Runner without the original Host charge was accepted")
	}
	first, err := b.server.dsgaSuspensionIntent(ctx, id, b.workerID, p, binding, &proposed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.server.db.Exec(ctx, "UPDATE dsga_suspension_intents SET intent='{}' WHERE operation_id=$1", id); err == nil {
		t.Fatal("durable suspension intent could be changed")
	}
	if _, err := b.server.db.Exec(ctx, "UPDATE operations SET lease_owner='replacement-worker' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	proposed.RunnerUID = "replacement-runner"
	proposed.Selection.Next.Instance.PGBootID++
	replay, err := b.server.dsgaSuspensionIntent(ctx, id, "replacement-worker", p, binding, &proposed)
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("takeover changed the original frozen intent: %v", err)
	}
	if _, err := b.server.dsgaSuspensionIntent(context.Background(), id, b.workerID, p, binding, nil); !errors.Is(err, errLeaseLost) {
		t.Fatalf("old Worker accepted: %v", err)
	}
	vm.ResourceVersion = "42"
	if err := b.server.recordDSGASuspensionRetirement(ctx, id, b.workerID, vm); !errors.Is(err, errLeaseLost) {
		t.Fatalf("old Worker could authorize retirement: %v", err)
	}
	if err := b.server.recordDSGASuspensionRetirement(ctx, id, "replacement-worker", vm); err != nil {
		t.Fatal(err)
	}
	if _, err := b.server.db.Exec(ctx, "DELETE FROM dsga_suspension_retirements WHERE operation_id=$1", id); err == nil {
		t.Fatal("retirement authorization could be removed")
	}
	if _, err := b.server.db.Exec(ctx, `UPDATE operations SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL
 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	tx, err := b.server.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := commitDSGASuspension(ctx, tx, id, p.ProjectID, p.EndpointID); !errors.Is(err, errDSGACreationIdentity) {
		t.Fatalf("endpoint suspended before step receipts: %v", err)
	}
	for i, name := range dsgaSuspensionSteps {
		if _, err := tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'succeeded')`,
			id, i, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := commitDSGASuspension(ctx, tx, id, p.ProjectID, p.EndpointID); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := tx.QueryRow(ctx, "SELECT state FROM endpoints WHERE id=$1", p.EndpointID).Scan(&state); err != nil || state != "suspended" {
		t.Fatalf("operation did not finalize the original endpoint: %v", err)
	}
}
