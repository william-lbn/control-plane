package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

func isDSGAUnopenedBoot(observed dsgapi.Snapshot, instance dsgapi.Instance) bool {
	return observed.Instance == instance && observed.IssuerFloorEpoch == 0 &&
		observed.CapacityGeneration == 1 && !observed.AdmissionOpen &&
		observed.AdmissionRevision == 0 && observed.Operation == nil
}

// A failed qualification can leave a native boot running before any runtime
// selection has been published. Retire only the exact original boot while its
// initial gate remains closed and the original qualifier has stopped.
func (s *server) verifyDSGAUnopenedBoot(ctx context.Context, clients kubernetes.Interface,
	vm *vmv1.VirtualMachine, p createPayload, plan dsgaCreationPlan, operation, configUID, serviceUID string,
	instance dsgapi.Instance,
) error {
	profile := plan.VM.supply()
	if profile == nil || instance.VMUID != string(vm.UID) || instance.ComputeInstanceID != plan.ComputeInstanceID {
		return errDSGACreationIdentity
	}
	var receipt, originalJob []byte
	err := s.db.QueryRow(ctx, `SELECT i.receipt,j.value FROM operations o
 JOIN dsga_creation_initialization_observations i ON i.operation_id=o.id
 JOIN dsga_creation_supply_facts j ON j.operation_id=o.id AND j.kind='qualification_job'
 WHERE o.id=$1 AND o.state='failed' AND o.error_code='qualification_unconfirmed'
 AND o.lease_owner IS NULL AND o.lease_expires_at IS NULL
 AND i.vm_uid=$2 AND i.config_uid=$3 AND i.management_service_uid=$4 AND i.credentials_uid=$5
 AND NOT EXISTS (SELECT 1 FROM dsga_creation_supply_facts f WHERE f.operation_id=o.id
  AND (f.kind IN ('qualification_result','runtime_selection')
   OR starts_with(f.kind,'qualification_retry_') OR starts_with(f.kind,'qualification_job_retry_')))`,
		operation, string(vm.UID), configUID, serviceUID, plan.Credentials.UID).Scan(&receipt, &originalJob)
	if err != nil {
		return err
	}
	var preserved dsgaInitializationResponse
	var jobRef dsgaSupplyObject
	if dsgapi.UnmarshalObject(receipt, &preserved) != nil ||
		preserved.validate(p, plan, string(vm.UID)) != nil ||
		!isDSGAUnopenedBoot(preserved.Observed, instance) ||
		dsgapi.UnmarshalObject(originalJob, &jobRef) != nil || jobRef.Name == "" || jobRef.UID == "" {
		return errDSGACreationIdentity
	}

	node, err := clients.CoreV1().Nodes().Get(ctx, vm.Status.Node, metav1.GetOptions{})
	if err != nil {
		return err
	}
	_, want, err := dsgaQualificationJob(vm, plan, string(node.UID), instance.PGBootID)
	if err != nil || want.Name != jobRef.Name {
		return errDSGACreationIdentity
	}
	job, err := clients.BatchV1().Jobs(want.Namespace).Get(ctx, jobRef.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	matched, same := matchDSGAQualificationJob(job, want)
	if !same || string(job.UID) != jobRef.UID || job.Status.Active != 0 ||
		job.Status.Succeeded != 0 || job.Status.Failed != 1 ||
		(job.Status.Terminating != nil && *job.Status.Terminating != 0) ||
		!isDSGAExpiredQualificationJob(job) {
		return errDSGAQualificationUnconfirmed
	}
	spec, err := json.Marshal(matched.Spec.Template.Spec)
	if err != nil || managedSQLDigest(spec) != jobRef.SHA256 {
		return errDSGACreationIdentity
	}
	pods, err := clients.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{dsgaSupplyVMLabel: string(vm.UID)}.String(),
	})
	if err != nil {
		return err
	}
	if len(pods.Items) != 0 || pods.Continue != "" {
		return errDSGAQualificationUnconfirmed
	}
	if _, err := clients.CoreV1().ConfigMaps(vm.Namespace).Get(
		ctx, runtimeselection.Name(string(vm.UID)), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return errDSGAQualificationUnconfirmed
	}
	if err := verifyNoDSGAQualificationClaim(ctx, clients, vm.Namespace, vm.UID); err != nil {
		return err
	}

	credential, err := clients.CoreV1().Secrets(vm.Namespace).Get(ctx, vm.Name+"-credentials", metav1.GetOptions{})
	if err != nil {
		return err
	}
	frozen, err := snapshotDSGACredentials(credential, vm.Namespace, vm.Name+"-credentials", p.ProjectID, p.EndpointID)
	if err != nil || frozen.reference() != plan.Credentials || len(credential.Data["controlSigningSeed"]) != ed25519.SeedSize {
		return errDSGACredentialSource
	}
	service, err := clients.CoreV1().Services(vm.Namespace).Get(ctx, vm.Name+"-management", metav1.GetOptions{})
	if err != nil {
		return err
	}
	wantedService, err := renderDSGAService(vm, p, plan, operation, "management")
	if err != nil || !sameDSGAService(service, wantedService, serviceUID) {
		return errDSGACreationIdentity
	}
	client := newDSGAProxy(nil).observer
	client.Timeout = 35 * time.Second
	defer client.CloseIdleConnections()
	key := ed25519.NewKeyFromSeed(credential.Data["controlSigningSeed"])
	fresh, err := fetchDSGAInitialization(ctx, client, p, plan, string(vm.UID), key, service.Spec.ClusterIP)
	if err != nil {
		return err
	}
	if !isDSGAUnopenedBoot(fresh.Observed, instance) {
		return errDSGACreationIdentity
	}
	return nil
}

func isDSGAExpiredQualificationJob(job *batch.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batch.JobFailed && condition.Status == core.ConditionTrue &&
			condition.Reason == batch.JobReasonDeadlineExceeded {
			return true
		}
	}
	return false
}
