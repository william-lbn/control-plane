package control

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const dsgaSuspendAction = "suspend_endpoint_dsga_v1"

type dsgaSuspensionPayload struct {
	ProjectID         string `json:"project_id"`
	BranchID          string `json:"branch_id"`
	EndpointID        string `json:"endpoint_id"`
	CreationOperation string `json:"creation_operation_id"`
	VMUID             string `json:"vm_uid"`
}

type dsgaSuspensionIntent struct {
	Selection  dsgaPolicyIntent  `json:"selection"`
	RunnerName string            `json:"runner_name"`
	RunnerUID  types.UID         `json:"runner_uid"`
	Host       dsgaSuspendedHost `json:"host"`
}

func (p dsgaSuspensionPayload) matches(e *dsgaEndpointRuntime) bool {
	return e != nil && p.CreationOperation != "" && p.VMUID != "" && e.ConfigUID != "" &&
		p.CreationOperation == e.Operation && p.VMUID == e.VMUID &&
		p.ProjectID == e.Payload.ProjectID && p.BranchID == e.Payload.BranchID && p.EndpointID == e.Payload.EndpointID
}

func (in dsgaSuspensionIntent) validate(id string, binding journal.Binding) error {
	next := in.Selection.Next
	previousSHA, err := hex.DecodeString(in.Selection.PreviousSHA256)
	if next.Validate() != nil || next.SuspensionOperation != id || next.Automatic != nil || next.Generation < 2 ||
		next.Namespace != binding.Namespace || next.Name != binding.Name || next.Incarnation != binding.Incarnation ||
		next.Instance.VMUID != string(binding.VMUID) || in.Selection.SelectionUID == "" || err != nil || len(previousSHA) != 32 ||
		dsgapi.ValidateID(in.RunnerName, "original Runner name") != nil || dsgapi.ValidateID(string(in.RunnerUID), "original Runner UID") != nil ||
		in.Host.Binding.Validate() != nil || in.Host.Origin == "" || in.Host.Sequence == 0 {
		return errDSGACreationIdentity
	}
	return nil
}

func newDSGASuspensionIntent(id string, vm *vmv1.VirtualMachine, cm *core.ConfigMap, current runtimeselection.Selection,
	pod *core.Pod, node *core.Node, binding journal.Binding) (dsgaSuspensionIntent, error) {
	var empty dsgaSuspensionIntent
	if current.Automatic == nil || current.SuspensionOperation != "" || current.Generation == ^dsgapi.Uint64(0) || cm == nil ||
		!isDSGASuspensionRunner(vm, pod) {
		return empty, errDSGACreationIdentity
	}
	host, ledger, err := readDSGASuspendedHost(node)
	if err != nil || host.Binding.NodeName != vm.Status.Node || ledger.Profile == nil || ledger.Profile.SchedulerName != pod.Spec.SchedulerName {
		return empty, errDSGACreationIdentity
	}
	for _, placement := range *ledger.Placements {
		if placement.Managed && placement.Namespace == vm.Namespace && placement.VMName == vm.Name &&
			placement.VMUID == string(vm.UID) && placement.Incarnation == binding.Incarnation &&
			placement.PodName == pod.Name && placement.PodUID == string(pod.UID) {
			next := current
			next.Generation++
			next.Automatic = nil
			next.SuspensionOperation = id
			intent := dsgaSuspensionIntent{
				Selection:  dsgaPolicyIntent{string(cm.UID), current.Digest(), next},
				RunnerName: pod.Name, RunnerUID: pod.UID, Host: host,
			}
			return intent, intent.validate(id, binding)
		}
	}
	return empty, errDSGAInitializationPending
}

func isDSGASuspensionRunner(vm *vmv1.VirtualMachine, pod *core.Pod) bool {
	if vm == nil || pod == nil || pod.UID == "" || pod.DeletionTimestamp != nil || pod.Status.Phase != core.PodRunning ||
		pod.Namespace != vm.Namespace || pod.Name != vm.Status.PodName || pod.Spec.NodeName != vm.Status.Node {
		return false
	}
	ref := metav1.GetControllerOf(pod)
	return ref != nil && ref.APIVersion == vmv1.SchemeGroupVersion.String() && ref.Kind == "VirtualMachine" && ref.Name == vm.Name && ref.UID == vm.UID
}

func (s *server) dsgaSuspensionIntent(ctx context.Context, id, worker string, p dsgaSuspensionPayload, binding journal.Binding, proposed *dsgaSuspensionIntent) (dsgaSuspensionIntent, error) {
	var intent dsgaSuspensionIntent
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return intent, err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return intent, err
	}
	var project, endpoint, action, resourceType string
	var payload []byte
	err = tx.QueryRow(ctx, "SELECT project_id,resource_id,action,resource_type,payload FROM operations WHERE id=$1", id).
		Scan(&project, &endpoint, &action, &resourceType, &payload)
	if err != nil {
		return intent, err
	}
	var durable dsgaSuspensionPayload
	if decodeDSGAObject(payload, &durable) != nil || durable != p || action != dsgaSuspendAction ||
		project != p.ProjectID || endpoint != p.EndpointID || resourceType != "endpoint" || string(binding.VMUID) != p.VMUID {
		return intent, errDSGACreationIdentity
	}
	var raw []byte
	err = tx.QueryRow(ctx, "SELECT intent FROM dsga_suspension_intents WHERE operation_id=$1", id).Scan(&raw)
	if proposed != nil && errors.Is(err, pgx.ErrNoRows) {
		intent = *proposed
		if err = intent.validate(id, binding); err != nil {
			return intent, err
		}
		raw, err = json.Marshal(intent)
		if err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO dsga_suspension_intents(operation_id,intent) VALUES($1,$2)", id, raw)
		}
	} else if err == nil {
		err = decodeDSGAObject(raw, &intent)
	}
	if err != nil {
		return intent, err
	}
	if err = intent.validate(id, binding); err != nil {
		return intent, err
	}
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return intent, err
	}
	return intent, tx.Commit(ctx)
}

// A retry after a lost PUT reply reads the immutable intent first and uses the
// original selection generation. It cannot capture a subsequent PG boot.
func (s *server) selectDSGASuspension(ctx context.Context, id, worker string, p dsgaSuspensionPayload, e *dsgaEndpointRuntime) (dsgaSuspensionIntent, error) {
	var empty dsgaSuspensionIntent
	if !p.matches(e) {
		return empty, errDSGACreationIdentity
	}
	binding := e.binding()
	intent, err := s.dsgaSuspensionIntent(ctx, id, worker, p, binding, nil)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	freeze := errors.Is(err, pgx.ErrNoRows)
	clients, vm, cm, current, err := s.readDSGABoundSelection(ctx, e)
	if err != nil {
		return empty, err
	}
	snapshot, err := journal.Inspect(vm, binding)
	if err != nil {
		return empty, err
	}
	state := snapshot.State()
	if state.PGInstance == nil || *state.PGInstance != current.Instance || vm.Status.Node == "" || vm.Status.PodName == "" {
		return empty, errDSGACreationIdentity
	}
	pod, err := clients.CoreV1().Pods(vm.Namespace).Get(ctx, vm.Status.PodName, metav1.GetOptions{})
	if err != nil {
		return empty, err
	}
	if freeze {
		node, err := clients.CoreV1().Nodes().Get(ctx, vm.Status.Node, metav1.GetOptions{})
		if err != nil {
			return empty, err
		}
		proposed, err := newDSGASuspensionIntent(id, vm, cm, current, pod, node, binding)
		if err != nil {
			return empty, err
		}
		intent, err = s.dsgaSuspensionIntent(ctx, id, worker, p, binding, &proposed)
		if err != nil {
			return empty, err
		}
	}
	if vm.Status.Node != intent.Host.Binding.NodeName || vm.Status.PodName != intent.RunnerName ||
		!isDSGASuspensionRunner(vm, pod) || pod.UID != intent.RunnerUID || current.Instance != intent.Selection.Next.Instance {
		return empty, errDSGACreationIdentity
	}
	if err = s.assertLease(ctx, id, worker); err != nil {
		return empty, err
	}
	return intent, writeDSGASelection(ctx, clients, cm, current, intent.Selection)
}
