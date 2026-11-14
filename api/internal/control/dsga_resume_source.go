package control

import (
	"context"
	"strconv"
	"time"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/william-lbn/control-plane/api/internal/adapter"
)

const dsgaResumeAction = "resume_endpoint_dsga_v1"

// Read the predecessor from immutable creation and suspension records. The
// same statement is usable after the successor exists, before its route CAS.
type dsgaResumeSource struct {
	Operation, Creation, VMUID, ConfigUID, ServiceUID string
	Incarnation, ComputeInstanceID                    string
	PGBootID                                          dsgapi.Uint64
}

func readDSGAResumeSource(ctx context.Context, db connectionDefaultReader, p createPayload, operation string) (dsgaResumeSource, error) {
	var source dsgaResumeSource
	var boot string
	err := db.QueryRow(ctx, `SELECT o.id,c.id,b.vm_uid,b.config_uid,v.service_uid,b.incarnation,b.compute_instance_id,
 i.intent#>>'{selection,next,instance,pg_boot_id}'
 FROM operations o JOIN dsga_suspension_intents i ON i.operation_id=o.id
 JOIN dsga_suspension_retirements r ON r.operation_id=o.id
 JOIN operations c ON c.id=o.payload->>'creation_operation_id'
 JOIN dsga_creation_bindings b ON b.operation_id=c.id AND b.vm_uid=r.vm_uid
 JOIN dsga_creation_services v ON v.operation_id=c.id AND v.vm_uid=b.vm_uid AND v.service_kind='postgres'
 WHERE o.id=$1 AND o.action='suspend_endpoint_dsga_v1' AND o.state='succeeded'
 AND o.finished_at IS NOT NULL AND o.lease_owner IS NULL AND o.lease_expires_at IS NULL
 AND o.project_id=$2 AND o.resource_type='endpoint' AND o.resource_id=$3
 AND o.payload->>'branch_id'=$4 AND o.payload->>'vm_uid'=b.vm_uid
 AND c.project_id=$2 AND c.payload->>'endpoint_id'=$3 AND c.payload->>'branch_id'=$4
 AND c.payload->>'tenant_id'=$5 AND c.payload->>'timeline_id'=$6 AND c.state='succeeded'
 AND c.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1','resume_endpoint_dsga_v1')
 AND EXISTS(SELECT 1 FROM operation_steps st WHERE st.operation_id=o.id AND st.ordinal=1 AND st.name=$7 AND st.state='succeeded')`,
		operation, p.ProjectID, p.EndpointID, p.BranchID, p.TenantID, p.TimelineID, dsgaSuspensionSteps[1]).
		Scan(&source.Operation, &source.Creation, &source.VMUID, &source.ConfigUID, &source.ServiceUID,
			&source.Incarnation, &source.ComputeInstanceID, &boot)
	if err != nil {
		return source, err
	}
	bootID, err := strconv.ParseUint(boot, 10, 64)
	if err != nil || bootID == 0 || strconv.FormatUint(bootID, 10) != boot {
		return dsgaResumeSource{}, errDSGACreationIdentity
	}
	source.PGBootID = dsgapi.Uint64(bootID)
	return source, nil
}

// The previous freeze defines logical identity and storage. The trusted
// retained release supplies the new compatible image, Runner and limits.
func newDSGAResumePlan(p createPayload, previous dsgaCreationPlan, policy dsgaCreationPolicy, now time.Time) (dsgaCreationPlan, error) {
	if previous.validate(p) != nil || previous.Credentials.validate() != nil || !policy.RetainedCatalog ||
		policy.postgresVersion() != previous.PostgresVersion || policy.Namespace != previous.Namespace ||
		policy.Storage != previous.Storage || policy.VM.supply() == nil || policy.VM.ProtocolVersion != 5 {
		return dsgaCreationPlan{}, errDSGACreationPlan
	}
	next, err := newDSGACreationPlan(p, policy, now)
	if err != nil {
		return dsgaCreationPlan{}, err
	}
	next.Credentials = previous.Credentials
	next.ComputeInstanceID = previous.ComputeInstanceID
	next.RetainCatalog = true
	next.ResumeGeneration = true
	next.ResumeMinMem, next.ResumeMaxMem = p.MinMem, p.MaxMem
	return next, next.validate(p)
}

func readDSGAResumePlan(ctx context.Context, db connectionDefaultReader, source dsgaResumeSource, p createPayload) (dsgaCreationPlan, error) {
	var plan dsgaCreationPlan
	var raw []byte
	err := db.QueryRow(ctx, `SELECT payload->'dsga_creation' FROM operations WHERE id=$1`, source.Creation).Scan(&raw)
	if err != nil {
		return plan, err
	}
	if decodeDSGAObject(raw, &plan) != nil || plan.validate(p) != nil || plan.Credentials.validate() != nil ||
		plan.Incarnation != source.Incarnation || plan.ComputeInstanceID != source.ComputeInstanceID {
		return dsgaCreationPlan{}, errDSGACreationIdentity
	}
	return plan, nil
}

// Called by the resume Worker with its currently bound new VM and fresh
// initialization. The verify callback also confirms the Worker lease and
// the newly observed Service UID before every publication, including replay.
func (source dsgaResumeSource) routeProof(verify func(context.Context, adapter.ManagedSQLBinding) error) *managedSQLResumeProof {
	return &managedSQLResumeProof{OriginalVMUID: source.VMUID, OriginalConfigUID: source.ConfigUID,
		OriginalServiceUID: source.ServiceUID, Verify: verify}
}
