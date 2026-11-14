package control

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/hostguard"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/owner"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Return the VM resourceVersion from the same read as the current Owner
// journal. The retirement request must use it as its DELETE precondition.
func (s *server) readDSGASuspensionDrain(ctx context.Context, e *dsgaEndpointRuntime, in dsgaSuspensionIntent) (*vmv1.VirtualMachine, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	clients, vm, cm, selected, err := s.readDSGABoundSelection(ctx, e)
	if err != nil {
		return nil, err
	}
	id := in.Selection.Next.SuspensionOperation
	if string(cm.UID) != in.Selection.SelectionUID || in.validate(id, e.binding()) != nil || selected.Digest() != in.Selection.Next.Digest() ||
		vm.Status.Node != in.Host.Binding.NodeName || vm.Status.PodName != in.RunnerName {
		return nil, errDSGACreationIdentity
	}
	pod, err := clients.CoreV1().Pods(vm.Namespace).Get(ctx, in.RunnerName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !isDSGASuspensionRunner(vm, pod) || pod.UID != in.RunnerUID {
		return nil, errDSGACreationIdentity
	}
	raw, agentUID, err := readDSGAOwnerPodDump(ctx, clients, vm, e.Plan.VM.supply())
	if err != nil {
		return nil, err
	}
	observation, err := decodeDSGAOwnerObservation(raw, vm.Namespace, in.RunnerName, selected, time.Now())
	if err != nil {
		return nil, err
	}
	_, vms, err := newDSGAKubeClients(s.kube)
	if err != nil {
		return nil, err
	}
	binding := e.binding()
	store, err := journal.NewStore(vms.NeonvmV1().VirtualMachines(binding.Namespace), binding)
	if err != nil {
		return nil, err
	}
	coord := clients.CoordinationV1()
	lease, err := coord.Leases(binding.Namespace).Get(ctx, owner.LeaseName(binding), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if lease.Spec.HolderIdentity == nil {
		return nil, owner.ErrLeadershipLost
	}
	holder := *lease.Spec.HolderIdentity
	if podUID, ok := owner.HolderPodUID(holder); !ok || agentUID == "" || podUID != agentUID {
		return nil, owner.ErrLeadershipLost
	}
	proof := owner.Ownership{Namespace: binding.Namespace, VMName: binding.Name, VMUID: binding.VMUID, Incarnation: binding.Incarnation,
		LeaseUID: lease.UID, HolderID: holder, IssuerEpoch: observation.IssuerEpoch}
	snapshot, lease, err := owner.VerifyCurrent(ctx, coord, store, proof)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if !snapshot.Dispatchable() || !dsgaOwnerLeaseFresh(lease, now) {
		return nil, owner.ErrLeadershipLost
	}
	checked := snapshot.VirtualMachine()
	if checked.Status.Node != in.Host.Binding.NodeName || checked.Status.PodName != in.RunnerName {
		return nil, errDSGACreationIdentity
	}
	if err = validateDSGASuspensionObservation(observation, now, selected, snapshot.State(), id); err != nil {
		return nil, err
	}
	return checked, nil
}

// Match the fresh Agent projection to the exact journal and selection.
// Host capacity remains charged until retirement has settled separately.
func validateDSGASuspensionObservation(observation dsgaOwnerObservation, now time.Time, selected runtimeselection.Selection, state journal.Model, operation string) error {
	sel := observation.RuntimeSelection
	age := now.Sub(observation.ObservedAt)
	if age > 15*time.Second || age < -2*time.Second || sel == nil || observation.SuspensionDrain == nil ||
		operation == "" || selected.SuspensionOperation != operation ||
		observation.IssuerEpoch != state.Owner.Epoch || sel.AppliedGeneration != selected.Generation ||
		sel.AppliedSHA256 != selected.Digest() || state.PGInstance == nil || *state.PGInstance != selected.Instance ||
		state.ServiceClosure == nil || state.ServiceClosure.Pending() || state.Active != nil {
		return errDSGAInitializationPending
	}
	c := state.ServiceClosure
	if c.Reason != "compute_suspension" || c.Reopen != nil || c.Rearm != nil {
		return errDSGACreationIdentity
	}
	// The shared AS module owns the on-wire Operation binding and all native
	// zero-client, prepared-transaction, admission and current Owner checks.
	return state.CheckComputeSuspensionDrain(operation, *observation.SuspensionDrain)
}

type dsgaSuspendedHost struct {
	Binding  hostguard.Binding `json:"binding"`
	Origin   string            `json:"origin"`
	Sequence dsgapi.Uint64     `json:"sequence"`
}

func readDSGASuspendedHost(node *core.Node) (dsgaSuspendedHost, hostguard.Model, error) {
	var m hostguard.Model
	if node == nil || node.UID == "" || node.ResourceVersion == "" || node.DeletionTimestamp != nil ||
		!slices.Contains(node.Finalizers, hostguard.ProtectionFinalizer) {
		return dsgaSuspendedHost{}, m, errDSGACreationIdentity
	}
	raw := node.Annotations[hostguard.LedgerAnnotation]
	if len(raw) > hostguard.MaxRawBytes || json.Unmarshal([]byte(raw), &m) != nil || m.Validate() != nil ||
		m.Binding.NodeUID != node.UID || m.Binding.NodeName != node.Name || m.Placements == nil ||
		node.Annotations[hostguard.OriginAnnotation] != "1:"+m.Origin {
		return dsgaSuspendedHost{}, m, errDSGACreationIdentity
	}
	return dsgaSuspendedHost{Binding: m.Binding, Origin: m.Origin, Sequence: m.Sequence}, m, nil
}

func verifyDSGASuspensionHostSettlement(original dsgaSuspendedHost, node *core.Node, vmUID, runnerUID string) error {
	current, m, err := readDSGASuspendedHost(node)
	if err != nil {
		return err
	}
	if vmUID == "" || runnerUID == "" || current.Binding != original.Binding || current.Origin != original.Origin ||
		current.Sequence < original.Sequence {
		return errDSGACreationIdentity
	}
	if current.Sequence == original.Sequence {
		return errDSGAInitializationPending
	}
	for _, placement := range *m.Placements {
		if placement.VMUID == vmUID || placement.PodUID == runnerUID {
			return errDSGAInitializationPending
		}
	}
	for _, hold := range m.Holds {
		if hold.Plan.Request.Key.Instance.VMUID == vmUID || hold.Plan.RunnerPodUID == runnerUID {
			return errDSGAInitializationPending
		}
	}
	return nil
}
