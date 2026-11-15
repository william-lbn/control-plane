package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type suspendedEndpointDeletionSource struct {
	dsgaResumeSource
	Scope     createPayload
	Namespace string
	Intent    dsgaSuspensionIntent
}

// A suspended endpoint can be deleted only against its current, successfully
// settled generation. An unresolved resume may own resources under the same names.
func readSuspendedEndpointDeletionSource(ctx context.Context, db connectionDefaultReader, p endpointDeletionPayload) (suspendedEndpointDeletionSource, error) {
	var result suspendedEndpointDeletionSource
	scope := createPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID}
	var raw []byte
	var suspension string
	err := db.QueryRow(ctx, `SELECT gone.id,project.tenant_id,branch.timeline_id,binding.namespace,intent.intent
 FROM endpoints e JOIN projects project ON project.id=e.project_id
 JOIN branches branch ON branch.id=e.branch_id AND branch.project_id=e.project_id
 JOIN operations gone ON gone.id=dsga_suspended_delete_source(e.project_id,e.branch_id,e.id,$5)
 JOIN dsga_creation_bindings binding ON binding.operation_id=gone.payload->>'creation_operation_id'
 JOIN dsga_suspension_intents intent ON intent.operation_id=gone.id
 WHERE e.id=$1 AND e.project_id=$2 AND e.branch_id=$3 AND e.workload_name=$4`,
		p.EndpointID, p.ProjectID, p.BranchID, kubeName(p.EndpointID), p.SuspensionOperationID).
		Scan(&suspension, &scope.TenantID, &scope.TimelineID, &result.Namespace, &raw)
	if err != nil {
		return result, err
	}
	result.Scope = scope
	result.dsgaResumeSource, err = readDSGAResumeSource(ctx, db, scope, suspension)
	if err != nil {
		return result, err
	}
	binding := journal.Binding{Namespace: result.Namespace, Name: kubeName(scope.EndpointID),
		VMUID: types.UID(result.VMUID), Incarnation: result.Incarnation}
	if decodeDSGAObject(raw, &result.Intent) != nil || result.Intent.validate(suspension, binding) != nil ||
		result.Intent.Selection.Next.Instance.PGBootID != result.PGBootID {
		return suspendedEndpointDeletionSource{}, errDSGACreationIdentity
	}
	return result, nil
}

func (s *server) observeSuspendedEndpointDeletion(ctx context.Context, source suspendedEndpointDeletionSource) error {
	if source.Namespace != s.kube.namespace {
		return errDSGACreationIdentity
	}
	_, err := s.kube.request(ctx, http.MethodGet, s.kube.path("vm", kubeName(source.Scope.EndpointID)), nil)
	if !kubeStatusIs(err, http.StatusNotFound) {
		if err == nil {
			return errDSGACreationIdentity
		}
		return err
	}
	if err := managedSQLReplacementRetired(ctx, s.kube, adapter.ManagedSQLBinding{
		Namespace: source.Namespace, EndpointID: source.Scope.EndpointID, VMUID: source.VMUID, Incarnation: source.Incarnation,
	}); err != nil {
		return err
	}
	clients, _, err := newDSGAKubeClients(s.kube)
	if err != nil {
		return err
	}
	if _, err := clients.CoreV1().Pods(source.Namespace).Get(ctx, source.Intent.RunnerName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return errDSGACreationIdentity
		}
		return err
	}
	host := source.Intent.Host
	node, err := clients.CoreV1().Nodes().Get(ctx, host.Binding.NodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	lease, err := clients.CoordinationV1().Leases(host.Binding.Namespace).Get(ctx, host.Binding.LeaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if lease.UID != host.Binding.LeaseUID || lease.DeletionTimestamp != nil {
		return errDSGACreationIdentity
	}
	return verifyDSGASuspensionHostSettlement(host, node, source.VMUID, string(source.Intent.RunnerUID))
}

func (source suspendedEndpointDeletionSource) verifyRoute(route map[string]any, operation string) error {
	// DSGA suspension retains an active route record for the managed wake
	// path. The committed suspend and absent original VM prove retirement.
	state := stringVal(route["state"])
	deletion := stringVal(route["deletion_operation_id"])
	if (state != "active" && state != "suspended" || deletion != "") &&
		(state != "deleted" || deletion != operation) {
		return errors.New("suspended deletion route state changed")
	}
	raw, err := json.Marshal(route["managed_binding"])
	var binding adapter.ManagedSQLBinding
	p := source.Scope
	if err != nil || decodeDSGAObject(raw, &binding) != nil || binding.Validate() != nil ||
		binding.ProjectID != p.ProjectID || binding.BranchID != p.BranchID || binding.EndpointID != p.EndpointID ||
		binding.TenantID != p.TenantID || binding.TimelineID != p.TimelineID || binding.Namespace != source.Namespace ||
		binding.VMUID != source.VMUID || binding.Incarnation != source.Incarnation ||
		binding.ComputeInstanceID != source.ComputeInstanceID || binding.ConfigSecretUID != source.ConfigUID ||
		binding.ServiceUID != source.ServiceUID {
		return errDSGACreationIdentity
	}
	return nil
}
