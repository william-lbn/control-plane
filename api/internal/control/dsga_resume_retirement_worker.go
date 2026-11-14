package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	"github.com/neondatabase/autoscaling/pkg/dsga/hostguard"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/vmguard"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

var dsgaResumeRetirementSteps = []string{"settle_unpublished_resume"}

type dsgaFailedResumeIntent struct {
	VMUID      string            `json:"vm_uid"`
	ConfigUID  string            `json:"config_uid"`
	RunnerUID  types.UID         `json:"runner_uid"`
	RunnerName string            `json:"runner_name"`
	Host       dsgaSuspendedHost `json:"host"`
}

func (s *server) reconcileFailedDSGAResumeRetirement(ctx context.Context, id, worker string, p dsgaFailedResumeRetirement) error {
	return s.createStep(ctx, id, worker, 0, dsgaResumeRetirementSteps[0], func(ctx context.Context) error {
		return s.settleFailedDSGAResume(ctx, id, worker, p)
	})
}

func (s *server) settleFailedDSGAResume(ctx context.Context, id, worker string, p dsgaFailedResumeRetirement) error {
	var envelope struct {
		createPayload
		Plan           dsgaCreationPlan `json:"dsga_creation"`
		ResumeFrom     string           `json:"resume_from"`
		ResumeReplaces string           `json:"resume_replaces,omitempty"`
	}
	var raw []byte
	var uid, config, sqlService, managementService string
	var beforeRunning bool
	err := s.db.QueryRow(ctx, `SELECT o.payload,b.vm_uid,b.config_uid,sql.service_uid,mgmt.service_uid,
 EXISTS(SELECT 1 FROM operation_steps step WHERE step.operation_id=o.id AND step.ordinal=2
  AND step.name='wait_neonvm_running' AND step.state='failed')
 FROM operations o JOIN dsga_creation_bindings b ON b.operation_id=o.id
 JOIN dsga_creation_services sql ON sql.operation_id=o.id AND sql.vm_uid=b.vm_uid AND sql.service_kind='postgres'
 JOIN dsga_creation_services mgmt ON mgmt.operation_id=o.id AND mgmt.vm_uid=b.vm_uid AND mgmt.service_kind='management'
 WHERE o.id=$1 AND o.project_id=$2 AND o.resource_id=$3 AND o.resource_type='endpoint'
 AND o.action=$4 AND o.state='failed' AND o.finished_at IS NOT NULL AND o.lease_owner IS NULL AND o.lease_expires_at IS NULL
 AND b.namespace=$5 AND b.vm_name=$6 AND b.config_uid IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM dsga_creation_initialization_observations WHERE operation_id=o.id)`,
		p.FailedResume, p.ProjectID, p.EndpointID, dsgaResumeAction, s.kube.namespace, kubeName(p.EndpointID)).
		Scan(&raw, &uid, &config, &sqlService, &managementService, &beforeRunning)
	if err != nil {
		return err
	}
	if decodeDSGAObject(raw, &envelope) != nil || envelope.ProjectID != p.ProjectID || envelope.BranchID != p.BranchID ||
		envelope.EndpointID != p.EndpointID || envelope.Plan.Namespace != s.kube.namespace ||
		!envelope.Plan.ResumeGeneration || envelope.Plan.validate(envelope.createPayload) != nil {
		return errDSGACreationIdentity
	}
	previous, err := readDSGAResumeSource(ctx, s.db, envelope.createPayload, envelope.ResumeFrom)
	if err != nil {
		return err
	}
	if err = s.verifyDSGAUnpublishedResumeRoute(ctx, envelope.createPayload, envelope.Plan, previous); err != nil {
		return err
	}
	binding := journal.Binding{Namespace: envelope.Plan.Namespace, Name: kubeName(p.EndpointID), VMUID: types.UID(uid), Incarnation: envelope.Plan.Incarnation}
	clients, vms, err := newDSGAKubeClients(s.kube)
	if err != nil {
		return err
	}
	api := vms.NeonvmV1().VirtualMachines(binding.Namespace)
	var in dsgaFailedResumeIntent
	err = s.db.QueryRow(ctx, `SELECT intent FROM dsga_failed_resume_retirements WHERE operation_id=$1
 AND failed_resume_operation_id=$2`, id, p.FailedResume).Scan(&raw)
	frozen := err == nil
	if frozen {
		if decodeDSGAObject(raw, &in) != nil || in.VMUID != uid || in.ConfigUID != config ||
			dsgapi.ValidateID(in.RunnerName, "Runner name") != nil || dsgapi.ValidateID(string(in.RunnerUID), "Runner UID") != nil ||
			in.Host.Binding.Validate() != nil || in.Host.Sequence == 0 {
			return errDSGACreationIdentity
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	vm, err := api.Get(ctx, binding.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) || vm.DeletionTimestamp != nil {
		if !frozen || err == nil && vm.UID != binding.VMUID {
			return errDSGACreationIdentity
		}
	} else {
		// Inspect the still-uninitialized journal on the very resourceVersion
		// that DELETE will use. Kubernetes rejects a concurrent journal advance.
		identity := dsgaCreationBinding{intent: dsgaCreationEnvelope{createPayload: envelope.createPayload,
			Creation: envelope.Plan.dsgaCreationIdentity}}
		if identity.validateVM(vm) != nil || vm.UID != binding.VMUID {
			return errDSGACreationIdentity
		}
		snapshot, err := journal.Inspect(vm, binding)
		if err != nil {
			return err
		}
		state := snapshot.State()
		if state.PGInstance != nil || state.Active != nil {
			return errDSGACreationIdentity
		}
		cfg, err := clients.CoreV1().Secrets(binding.Namespace).Get(ctx, binding.Name+"-config", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !dsgaRetirementOwns(cfg, vm, config) || bootstrap.CheckReady(vm, cfg, false) != nil {
			return errDSGACreationIdentity
		}
		for name, serviceUID := range map[string]string{binding.Name: sqlService, binding.Name + "-management": managementService} {
			service, err := clients.CoreV1().Services(binding.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if !dsgaRetirementOwns(service, vm, serviceUID) {
				return errDSGACreationIdentity
			}
		}
		if beforeRunning {
			// A failed readiness step can also be a timeout with a live VM.
			// This path requires terminal pre-birth proof, including no native
			// resource, budget, session or service ownership ever published.
			if vm.Status.Phase != vmv1.VmFailed || !unusedDSGAFailedResumeJournal(state) {
				return errDSGACreationIdentity
			}
			if err := vmguard.ConfirmBirthSourcesAbsent(ctx, clients.CoreV1(), binding); err != nil {
				return err
			}
		}
		current, err := readDSGAFailedResumeIntent(ctx, clients, vm, binding, config, beforeRunning)
		if err != nil {
			return err
		}
		if !frozen {
			if err = s.recordDSGAFailedResumeIntent(ctx, id, worker, p.FailedResume, current); err != nil {
				return err
			}
			in = current
		} else if current.VMUID != in.VMUID || current.ConfigUID != in.ConfigUID ||
			current.RunnerUID != in.RunnerUID || current.RunnerName != in.RunnerName ||
			current.Host.Binding != in.Host.Binding || current.Host.Origin != in.Host.Origin || current.Host.Sequence < in.Host.Sequence {
			return errDSGACreationIdentity
		}
		if err = s.verifyDSGAUnpublishedResumeRoute(ctx, envelope.createPayload, envelope.Plan, previous); err != nil {
			return err
		}
		if err = s.assertLease(ctx, id, worker); err != nil {
			return err
		}
		options := &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &vm.UID, ResourceVersion: &vm.ResourceVersion}}
		err = vms.NeonvmV1().RESTClient().Delete().Namespace(binding.Namespace).Resource("virtualmachines").
			Name(binding.Name).Body(options).MaxRetries(0).Do(ctx).Error()
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	for {
		vm, err := api.Get(ctx, binding.Name, metav1.GetOptions{})
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
	if err = s.observeDeletedEndpointRunners(ctx, endpointDeletionPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID}); err != nil {
		return err
	}
	if _, err = clients.CoreV1().Pods(binding.Namespace).Get(ctx, in.RunnerName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return errDSGACreationIdentity
	}
	for {
		err = managedSQLReplacementRetired(ctx, s.kube, adapter.ManagedSQLBinding{
			Namespace: binding.Namespace, EndpointID: p.EndpointID, VMUID: uid, Incarnation: binding.Incarnation,
		})
		if err == nil {
			break
		}
		var stage managedSQLStageError
		if !errors.As(err, &stage) || stage.stage != "old_lease_present" && stage.stage != "old_pods_present" {
			return err
		}
		if err = waitDSGAStartupPoll(ctx); err != nil {
			return err
		}
	}
	if err = observeDSGAFailedResumeChildren(ctx, clients, binding, config, sqlService, managementService); err != nil {
		return err
	}
	for {
		node, err := clients.CoreV1().Nodes().Get(ctx, in.Host.Binding.NodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err = verifyDSGAFailedResumeHostLease(ctx, clients, in.Host); err != nil {
			return err
		}
		err = verifyDSGASuspensionHostSettlement(in.Host, node, uid, string(in.RunnerUID))
		if err == nil {
			if err = s.verifyDSGAUnpublishedResumeRoute(ctx, envelope.createPayload, envelope.Plan, previous); err != nil {
				return err
			}
			if _, err = api.Get(ctx, binding.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				if err != nil {
					return err
				}
				return errDSGACreationIdentity
			}
			current, _, err := readDSGASuspendedHost(node)
			if err != nil {
				return err
			}
			return s.recordDSGAFailedResumeSettlement(ctx, id, worker, current.Sequence)
		}
		if !errors.Is(err, errDSGAInitializationPending) {
			return err
		}
		if err = waitDSGAStartupPoll(ctx); err != nil {
			return err
		}
	}
}

func dsgaRetirementOwns(item metav1.Object, vm *vmv1.VirtualMachine, uid string) bool {
	return uid != "" && string(item.GetUID()) == uid && item.GetResourceVersion() != "" && item.GetDeletionTimestamp() == nil &&
		dsgaRetirementOwnsReference(item, vm)
}

func unusedDSGAFailedResumeJournal(m journal.Model) bool {
	return m.ProtocolVersion == 1 && m.HighWater == nil && m.Active == nil && len(m.History) == 0 &&
		m.ExpiredResults == 0 && m.ResourceIdentity == nil && m.PGInstance == nil &&
		m.BudgetHeld == nil && m.CapacityHeld == nil && m.InitialHeld == nil && m.BudgetBoundary == nil &&
		m.ServiceClosure == nil && m.ServiceLifecycle == nil && m.RetainedCoverage == nil && m.RetainedContextSHA256 == ""
}

func isDSGAFailedResumeTerminalRunner(vm *vmv1.VirtualMachine, pod *core.Pod) bool {
	if vm.Status.Phase != vmv1.VmFailed || vm.Spec.RestartPolicy != vmv1.RestartPolicyNever ||
		pod.Namespace != vm.Namespace || pod.Name != vm.Status.PodName || pod.Spec.NodeName == "" ||
		(vm.Status.Node != "" && pod.Spec.NodeName != vm.Status.Node) || pod.UID == "" ||
		pod.ResourceVersion == "" || len(pod.OwnerReferences) != 1 ||
		!slices.Contains(pod.Finalizers, hostguard.PodProtectionFinalizer) || !dsgaRetirementOwnsReference(pod, vm) {
		return false
	}
	// The original terminal Pod may have lost its container status during
	// eviction. This only permits the VM deletion request. The Host ledger and
	// Scheduler must still verify physical absence before settlement succeeds.
	return hostguard.PreBirthTerminalPod(pod) == nil ||
		(pod.DeletionTimestamp != nil && pod.Spec.RestartPolicy == core.RestartPolicyNever &&
			pod.Status.Phase == core.PodFailed && pod.Status.Reason == "Evicted")
}

func dsgaRetirementOwnsReference(item metav1.Object, vm *vmv1.VirtualMachine) bool {
	ref := metav1.GetControllerOf(item)
	return ref != nil && ref.APIVersion == vmv1.SchemeGroupVersion.String() && ref.Kind == "VirtualMachine" && ref.Name == vm.Name && ref.UID == vm.UID
}

func readDSGAFailedResumeIntent(ctx context.Context, clients kubernetes.Interface, vm *vmv1.VirtualMachine, binding journal.Binding, config string, beforeRunning bool) (dsgaFailedResumeIntent, error) {
	if vm.Status.PodName == "" {
		return dsgaFailedResumeIntent{}, errDSGACreationIdentity
	}
	pod, err := clients.CoreV1().Pods(binding.Namespace).Get(ctx, vm.Status.PodName, metav1.GetOptions{})
	if err != nil {
		return dsgaFailedResumeIntent{}, err
	}
	if beforeRunning && !isDSGAFailedResumeTerminalRunner(vm, pod) || !beforeRunning && !isDSGASuspensionRunner(vm, pod) {
		return dsgaFailedResumeIntent{}, errDSGACreationIdentity
	}
	node, err := clients.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return dsgaFailedResumeIntent{}, err
	}
	host, ledger, err := readDSGASuspendedHost(node)
	if err != nil || ledger.Profile == nil || ledger.Profile.SchedulerName != pod.Spec.SchedulerName {
		return dsgaFailedResumeIntent{}, errDSGACreationIdentity
	}
	if err = verifyDSGAFailedResumeHostLease(ctx, clients, host); err != nil {
		return dsgaFailedResumeIntent{}, err
	}
	for _, placement := range *ledger.Placements {
		if placement.Managed && placement.Namespace == binding.Namespace && placement.VMName == binding.Name &&
			placement.VMUID == string(binding.VMUID) && placement.Incarnation == binding.Incarnation &&
			placement.PodName == pod.Name && placement.PodUID == string(pod.UID) {
			return dsgaFailedResumeIntent{VMUID: string(vm.UID), ConfigUID: config, RunnerUID: pod.UID, RunnerName: pod.Name, Host: host}, nil
		}
	}
	return dsgaFailedResumeIntent{}, errDSGACreationIdentity
}

func verifyDSGAFailedResumeHostLease(ctx context.Context, clients kubernetes.Interface, host dsgaSuspendedHost) error {
	lease, err := clients.CoordinationV1().Leases(host.Binding.Namespace).Get(ctx, host.Binding.LeaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if lease.UID != host.Binding.LeaseUID || lease.DeletionTimestamp != nil {
		return errDSGACreationIdentity
	}
	return nil
}

func observeDSGAFailedResumeChildren(ctx context.Context, clients kubernetes.Interface, b journal.Binding, config, sql, management string) error {
	for {
		// These children are tied to the failed VM. The retained credential
		// Secret is a separate object and is never modified by retirement.
		cfg, err := clients.CoreV1().Secrets(b.Namespace).Get(ctx, b.Name+"-config", metav1.GetOptions{})
		switch {
		case err == nil && string(cfg.UID) != config:
			return errDSGACreationIdentity
		case err != nil && !apierrors.IsNotFound(err):
			return err
		}
		pending := err == nil
		for name, uid := range map[string]string{b.Name: sql, b.Name + "-management": management} {
			service, err := clients.CoreV1().Services(b.Namespace).Get(ctx, name, metav1.GetOptions{})
			switch {
			case err == nil && string(service.UID) != uid:
				return errDSGACreationIdentity
			case err != nil && !apierrors.IsNotFound(err):
				return err
			case err == nil:
				pending = true
			}
		}
		if !pending {
			return nil
		}
		if err := waitDSGAStartupPoll(ctx); err != nil {
			return err
		}
	}
}

func (s *server) verifyDSGAUnpublishedResumeRoute(ctx context.Context, p createPayload, plan dsgaCreationPlan, previous dsgaResumeSource) error {
	// The creation digest is domain-separated. The route uses the canonical
	// Secret data digest. Verify each against its own representation.
	var credentials core.Secret
	if err := managedSQLRead(s.kube, ctx, "secret", kubeName(p.EndpointID)+"-credentials", &credentials); err != nil {
		return fmt.Errorf("read retained credentials: %w", errDSGACredentialSource)
	}
	snapshot, err := snapshotDSGACredentials(&credentials, plan.Namespace, credentials.Name, p.ProjectID, p.EndpointID)
	if err != nil || credentials.Name != kubeName(p.EndpointID)+"-credentials" || snapshot.reference() != plan.Credentials {
		return fmt.Errorf("match retained credentials: %w", errDSGACredentialSource)
	}
	registry, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
	if err != nil {
		return err
	}
	if stringVal(nested(registry, "metadata", "uid")) == "" || stringVal(nested(registry, "metadata", "resourceVersion")) == "" ||
		nested(registry, "metadata", "deletionTimestamp") != nil {
		return errDSGACreationIdentity
	}
	raw, err := secretText(registry, "routes.json")
	if err != nil {
		return err
	}
	routes, err := managedSQLRouteMap([]byte(raw))
	if err != nil {
		return err
	}
	route, err := managedSQLRouteMap(routes[selector(p.EndpointID)])
	var binding adapter.ManagedSQLBinding
	if err != nil || dsgapi.UnmarshalObject(route["managed_binding"], &binding) != nil || binding.Validate() != nil ||
		binding.ProjectID != p.ProjectID || binding.EndpointID != p.EndpointID || binding.BranchID != p.BranchID ||
		binding.TenantID != p.TenantID || binding.TimelineID != p.TimelineID || binding.Namespace != plan.Namespace ||
		binding.VMUID != previous.VMUID || binding.ConfigSecretUID != previous.ConfigUID || binding.ServiceUID != previous.ServiceUID ||
		binding.Incarnation != previous.Incarnation || binding.ComputeInstanceID != previous.ComputeInstanceID ||
		binding.CredentialSecretUID != snapshot.uid || binding.CredentialDataSHA256 != managedSQLDataDigest(credentials.Data) {
		return fmt.Errorf("match predecessor route: %w", errDSGACreationIdentity)
	}
	return nil
}

func (s *server) recordDSGAFailedResumeIntent(ctx context.Context, id, worker, failed string, in dsgaFailedResumeIntent) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO dsga_failed_resume_retirements(operation_id,failed_resume_operation_id,intent) VALUES($1,$2,$3)`, id, failed, raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *server) recordDSGAFailedResumeSettlement(ctx context.Context, id, worker string, sequence dsgapi.Uint64) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO dsga_failed_resume_settlements(operation_id,host_sequence) VALUES($1,$2::text::numeric)
 ON CONFLICT(operation_id) DO NOTHING`, id, strconv.FormatUint(uint64(sequence), 10)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
