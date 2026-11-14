package control

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s *server) reconcileDSGAResume(ctx context.Context, binding *dsgaCreationBinding) error {
	if binding.action != dsgaResumeAction {
		return errDSGACreationIdentity
	}
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		return err
	}
	err = s.createStep(ctx, binding.operationID, binding.workerID, 0, dsgaEndpointCreateSteps()[0], func(ctx context.Context) error {
		return binding.verifyResumeTimeline(ctx, plan)
	})
	if err != nil {
		return err
	}
	return s.reconcileDSGACompute(ctx, binding, 1)
}

func (b *dsgaCreationBinding) verifyResumeTimeline(ctx context.Context, plan dsgaCreationPlan) error {
	p := b.intent.createPayload
	source, err := readDSGAResumeSource(ctx, b.server.db, p, b.intent.ResumeFrom)
	if err != nil {
		return err
	}
	previous, err := readDSGAResumePlan(ctx, b.server.db, source, p)
	if err != nil {
		return err
	}
	if plan.ComputeInstanceID != previous.ComputeInstanceID || plan.Credentials != previous.Credentials ||
		plan.Namespace != previous.Namespace || plan.PostgresVersion != previous.PostgresVersion || plan.Storage != previous.Storage ||
		plan.Incarnation == previous.Incarnation || plan.OperationUUID == previous.OperationUUID {
		return errDSGACreationIdentity
	}
	k := b.server.kube
	_, vms, err := newDSGAKubeClients(k)
	if err != nil {
		return err
	}
	vm, err := vms.NeonvmV1().VirtualMachines(plan.Namespace).Get(ctx, kubeName(p.EndpointID), metav1.GetOptions{})
	if err == nil {
		if string(vm.UID) == source.VMUID || b.validateVM(vm) != nil {
			return errDSGACreationIdentity
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if err := managedSQLReplacementRetired(ctx, k, adapter.ManagedSQLBinding{
		Namespace: plan.Namespace, EndpointID: p.EndpointID, VMUID: source.VMUID, Incarnation: source.Incarnation,
	}); err != nil {
		return err
	}
	// The branch row fixes the ancestry, including for a branch restored at a
	// point in time. Compare every shard to the immutable fork receipt.
	var parent, lsn string
	err = b.server.db.QueryRow(ctx, `SELECT COALESCE(o.payload->>'parent_timeline_id',''), COALESCE(br.parent_lsn,'')
 FROM branches br LEFT JOIN operations o ON o.project_id=br.project_id AND o.resource_type='branch' AND o.resource_id=br.id
   AND o.action='create_branch_dsga_v1' AND o.state='succeeded' AND o.payload->>'timeline_id'=br.timeline_id
   AND o.payload->>'parent_branch_id'=br.parent_branch_id AND o.payload->>'parent_lsn'=br.parent_lsn
 WHERE br.id=$1 AND br.project_id=$2 AND br.timeline_id=$3 AND br.state='ready' AND br.deleted_at IS NULL AND br.expires_at IS NULL
 AND (br.parent_branch_id IS NULL OR EXISTS(SELECT 1 FROM dsga_creation_timelines t WHERE t.operation_id=o.id AND t.confirmed_at IS NOT NULL))`,
		p.BranchID, p.ProjectID, p.TimelineID).Scan(&parent, &lsn)
	if err != nil {
		return err
	}
	storage, err := newDSGAStorageClientAccess(ctx, k, plan.Storage, false)
	if err != nil {
		return err
	}
	defer storage.CloseIdleConnections()
	fact, err := storage.request(ctx, http.MethodGet, "control/v1/tenant/"+p.TenantID+"/timeline/"+p.TimelineID, nil)
	if err != nil {
		return err
	}
	return validateDSGATimelineAggregate(fact, p.TenantID, p.TimelineID, plan.PostgresVersion, parent, lsn, true)
}

// Atomically publish the new active runtime and the terminal Operation after
// the UID-bound native probe, the route probe and all seven Worker steps pass.
func commitDSGAResume(ctx context.Context, tx pgx.Tx, id, project, endpoint string) error {
	tag, err := tx.Exec(ctx, `UPDATE endpoints e SET state='active',dsga_runtime_operation_id=o.id,version=e.version+1,updated_at=now()
 FROM operations o JOIN operations suspended ON suspended.id=o.payload->>'resume_from'
 JOIN dsga_creation_bindings b ON b.operation_id=o.id
 JOIN dsga_creation_bindings old ON old.operation_id=suspended.payload->>'creation_operation_id'
 JOIN dsga_creation_initialization_observations i ON i.operation_id=o.id AND i.vm_uid=b.vm_uid AND i.config_uid=b.config_uid
 JOIN dsga_creation_services v ON v.operation_id=o.id AND v.vm_uid=b.vm_uid AND v.service_kind='postgres'
 JOIN dsga_creation_supply_facts f ON f.operation_id=o.id AND f.kind='runtime_selection'
 WHERE o.id=$1 AND o.action=$4 AND o.project_id=$2 AND o.resource_id=$3 AND o.state='succeeded'
 AND suspended.action='suspend_endpoint_dsga_v1' AND suspended.state='succeeded'
 AND e.id=o.resource_id AND e.project_id=o.project_id AND e.branch_id=o.payload->>'branch_id'
 AND e.state='suspended' AND e.deleted_at IS NULL AND b.vm_uid<>old.vm_uid AND b.config_uid<>old.config_uid
 AND (e.dsga_runtime_operation_id=old.operation_id OR (e.dsga_runtime_operation_id IS NULL AND EXISTS (
   SELECT 1 FROM operations original WHERE original.id=old.operation_id AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1'))))
 AND (SELECT count(*) FROM operation_steps st JOIN unnest($5::text[]) WITH ORDINALITY d(name,position)
   ON st.ordinal=d.position-1 AND st.name=d.name WHERE st.operation_id=o.id AND st.state='succeeded')=cardinality($5::text[])`,
		id, project, endpoint, dsgaResumeAction, dsgaEndpointCreateSteps())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errDSGACreationIdentity
	}
	return nil
}
