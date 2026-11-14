package control

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var dsgaSuspensionSteps = []string{"select_original_compute_for_suspension", "drain_and_settle_original_compute"}

func (s *server) reconcileDSGASuspension(ctx context.Context, id, worker string, p dsgaSuspensionPayload) error {
	e, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.EndpointID)
	if err != nil {
		return err
	}
	if !p.matches(e) {
		return errDSGACreationIdentity
	}
	var selected bool
	err = s.db.QueryRow(ctx, `SELECT state='succeeded' FROM operation_steps WHERE operation_id=$1 AND ordinal=0 AND name=$2`,
		id, dsgaSuspensionSteps[0]).Scan(&selected)
	if err != nil {
		return err
	}
	var in dsgaSuspensionIntent
	if selected {
		in, err = s.dsgaSuspensionIntent(ctx, id, worker, p, e.binding(), nil)
	} else {
		err = s.createStep(ctx, id, worker, 0, dsgaSuspensionSteps[0], func(ctx context.Context) error {
			in, err = s.selectDSGASuspension(ctx, id, worker, p, e)
			return err
		})
	}
	if err != nil {
		return err
	}
	return s.createStep(ctx, id, worker, 1, dsgaSuspensionSteps[1], func(ctx context.Context) error {
		// An Owner can still be settling a preceding resource operation or
		// waiting for current clients. Keep reconciling the original intent.
		for {
			err := s.retireDSGASuspendedCompute(ctx, id, worker, e, in)
			if !errors.Is(err, errDSGAInitializationPending) {
				return err
			}
			if err = waitDSGAStartupPoll(ctx); err != nil {
				return err
			}
		}
	})
}

// Called in the transaction that completes the Operation. A failed or
// abandoned drain leaves the public state unchanged and remains recoverable.
func commitDSGASuspension(ctx context.Context, tx pgx.Tx, id, project, endpoint string) error {
	tag, err := tx.Exec(ctx, `UPDATE endpoints e SET state='suspended',version=e.version+1,updated_at=now()
 FROM operations o JOIN dsga_suspension_retirements r ON r.operation_id=o.id
 WHERE o.id=$1 AND o.action=$4 AND o.project_id=$2 AND o.resource_id=$3 AND o.state='succeeded'
 AND e.id=o.resource_id AND e.project_id=o.project_id AND e.branch_id=o.payload->>'branch_id'
 AND e.state='active' AND e.deleted_at IS NULL AND r.vm_uid=o.payload->>'vm_uid'
 AND EXISTS(SELECT 1 FROM operation_steps st WHERE st.operation_id=o.id AND st.ordinal=0 AND st.name=$5 AND st.state='succeeded')
 AND EXISTS(SELECT 1 FROM operation_steps st WHERE st.operation_id=o.id AND st.ordinal=1 AND st.name=$6 AND st.state='succeeded')`,
		id, project, endpoint, dsgaSuspendAction, dsgaSuspensionSteps[0], dsgaSuspensionSteps[1])
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errDSGACreationIdentity
	}
	return nil
}

// Persist the authorization before DELETE so retries can prove what to
// reconcile even if the API response and the original VM are both gone.
func (s *server) recordDSGASuspensionRetirement(ctx context.Context, id, worker string, proof *vmv1.VirtualMachine) error {
	if proof == nil || proof.UID == "" || proof.ResourceVersion == "" || proof.DeletionTimestamp != nil {
		return errDSGACreationIdentity
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO dsga_suspension_retirements(operation_id,vm_uid,drain_resource_version)
 VALUES($1,$2,$3) ON CONFLICT(operation_id) DO NOTHING`, id, string(proof.UID), proof.ResourceVersion); err != nil {
		return err
	}
	var uid string
	if err = tx.QueryRow(ctx, "SELECT vm_uid FROM dsga_suspension_retirements WHERE operation_id=$1", id).Scan(&uid); err != nil {
		return err
	}
	if uid != string(proof.UID) {
		return errDSGACreationIdentity
	}
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *server) retireDSGASuspendedCompute(ctx context.Context, id, worker string, e *dsgaEndpointRuntime, in dsgaSuspensionIntent) error {
	binding := e.binding()
	if in.validate(id, binding) != nil {
		return errDSGACreationIdentity
	}
	clients, vms, err := newDSGAKubeClients(s.kube)
	if err != nil {
		return err
	}
	api := vms.NeonvmV1().VirtualMachines(binding.Namespace)
	var permitUID string
	err = s.db.QueryRow(ctx, "SELECT vm_uid FROM dsga_suspension_retirements WHERE operation_id=$1", id).Scan(&permitUID)
	authorized := err == nil
	if !authorized && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if authorized && permitUID != string(binding.VMUID) {
		return errDSGACreationIdentity
	}

	vm, err := api.Get(ctx, binding.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		if vm.UID != binding.VMUID || vm.ResourceVersion == "" {
			return errDSGACreationIdentity
		}
		if vm.DeletionTimestamp == nil {
			if err = s.deleteDSGADrainedVM(ctx, id, worker, e, in); err != nil {
				return err
			}
			authorized = true
		}
	}
	if !authorized {
		return errDSGACreationIdentity
	}
	// Reuse the existing physical observers, without another DELETE attempt
	// or any change to the storage or credentials of the logical endpoint.
	for {
		vm, err = api.Get(ctx, binding.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			return err
		}
		if vm.UID != binding.VMUID || vm.DeletionTimestamp == nil {
			return errDSGACreationIdentity
		}
		if err = waitDSGAStartupPoll(ctx); err != nil {
			return err
		}
	}
	p := endpointDeletionPayload{ProjectID: e.Payload.ProjectID, BranchID: e.Payload.BranchID, EndpointID: e.Payload.EndpointID}
	if err = s.observeDeletedEndpointRunners(ctx, p); err != nil {
		return err
	}
	// Also look up the frozen Runner by name so it cannot evade the list by
	// losing a label. Treat a same-name replacement as a changed identity.
	_, err = clients.CoreV1().Pods(binding.Namespace).Get(ctx, in.RunnerName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return errDSGACreationIdentity
	}
	for {
		node, err := clients.CoreV1().Nodes().Get(ctx, in.Host.Binding.NodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		hostLease, err := clients.CoordinationV1().Leases(in.Host.Binding.Namespace).Get(ctx, in.Host.Binding.LeaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if hostLease.UID != in.Host.Binding.LeaseUID || hostLease.DeletionTimestamp != nil {
			return errDSGACreationIdentity
		}
		err = verifyDSGASuspensionHostSettlement(in.Host, node, string(binding.VMUID), string(in.RunnerUID))
		if err == nil {
			_, err = api.Get(ctx, binding.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return s.assertLease(ctx, id, worker)
			}
			if err == nil {
				return errDSGACreationIdentity
			}
			return err
		}
		if !errors.Is(err, errDSGAInitializationPending) {
			return err
		}
		if err = waitDSGAStartupPoll(ctx); err != nil {
			return err
		}
	}
}

func (s *server) deleteDSGADrainedVM(ctx context.Context, id, worker string, e *dsgaEndpointRuntime, in dsgaSuspensionIntent) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	proof, err := s.readDSGASuspensionDrain(ctx, e, in)
	if err != nil {
		return err
	}
	if err = s.recordDSGASuspensionRetirement(ctx, id, worker, proof); err != nil {
		return err
	}
	_, vms, err := newDSGAKubeClients(s.kube)
	if err != nil {
		return err
	}
	if err = s.assertLease(ctx, id, worker); err != nil {
		return err
	}
	// Keep the resourceVersion from the drain observation. If the VM or its
	// journal changed, the API server must reject the stale DELETE.
	options := &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &proof.UID, ResourceVersion: &proof.ResourceVersion}}
	err = vms.NeonvmV1().RESTClient().Delete().Namespace(proof.Namespace).Resource("virtualmachines").
		Name(proof.Name).Body(options).MaxRetries(0).Do(ctx).Error()
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
