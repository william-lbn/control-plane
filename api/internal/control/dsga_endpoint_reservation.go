package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type dsgaEndpointCredentialSource struct {
	Endpoint  string                  `json:"endpoint_id"`
	Operation string                  `json:"operation_id"`
	Reference dsgaCredentialReference `json:"reference"`
}

var errDSGAEndpointWriterExists = errors.New("a writer already exists on the original branch")
var errDSGACredentialWriterRetained = errors.New("the original writer runtime still exists")

func (s *dsgaEndpointCredentialSource) valid(target string) bool {
	return s != nil && s.Endpoint != target && dsgapi.ValidateID(s.Endpoint, "credential endpoint") == nil &&
		dsgapi.ValidateID(s.Operation, "credential operation") == nil && s.Reference.validate() == nil
}

func (s *server) createDSGARetainedEndpoint(w http.ResponseWriter, r *http.Request, body endpointCreateRequest) {
	if os.Getenv("NEON_DSGA_CREATE_ENABLED") != "true" || s.kube == nil || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "dsga_creation_disabled", "PostgreSQL DSGA creation is not enabled")
		return
	}
	mode := body.Type
	if mode == "" {
		mode = "read_write"
	}
	if (mode != "read_write" && mode != "read_only") || (mode == "read_only" && body.Password != "") {
		fail(w, r, 422, "invalid_endpoint", "Check Compute mode and password")
		return
	}
	if mode == "read_write" {
		if _, ok := dsgaPassword(body.Password); !ok {
			fail(w, r, 422, "invalid_database_password", "Supply the existing branch application password")
			return
		}
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	ctx, err := dsgaCreationRequestContext(r)
	if err != nil {
		s.dsgaCreationFailure(w, r, err)
		return
	}
	r = r.WithContext(ctx)
	project, actor, org := r.PathValue("project"), userFrom(r).ID, accessFrom(r).OrganizationID
	scope := "POST:/projects/" + project + "/endpoints"
	hash := s.createRequestHash(scope, body)
	if s.replayCreate(w, r, key, hash, "", "endpoint") {
		return
	}
	reservation, err := s.reserveDSGAEndpoint(ctx, actor, org, project, scope, key, hash, mode, body)
	if err != nil {
		s.dsgaCreationFailure(w, r, err)
		return
	}
	p := reservation.Draft.Payload
	if !reservation.Accepted {
		plan := reservation.Draft.Plan
		plan.Credentials = reservation.Credentials
		source := reservation.Draft.CredentialSource
		if p.EndpointType == "read_write" {
			if err := s.kube.verifyDSGAPriorWriterAbsent(ctx, plan.Namespace, source.Endpoint); err != nil {
				s.dsgaCreationFailure(w, r, err)
				return
			}
		}
		frozen, err := s.kube.prepareDSGACredentials(ctx, p, plan, reservation.Operation, dsgaCredentialInput{
			SourceEndpoint: source.Endpoint, Source: source.Reference, ExistingPassword: body.Password,
		})
		if errors.Is(err, errBranchPasswordMismatch) {
			fail(w, r, 422, "branch_password_mismatch", "Use the existing branch SQL password")
			return
		}
		if err != nil {
			fail(w, r, 503, "credential_unavailable", "Original branch credentials could not be confirmed")
			return
		}
		created, err := s.acceptDSGAEndpoint(ctx, reservation, frozen.Credentials, key, requestID(r))
		if err != nil {
			s.dsgaCreationFailure(w, r, err)
			return
		}
		if created {
			s.audit(ctx, actor, "create_endpoint_dsga_v1", "endpoint", p.EndpointID, "accepted", requestID(r))
		}
	}
	s.acceptCreate(w, r, project, p.EndpointID, reservation.Operation, "endpoint")
}

func (k *kubeClient) verifyDSGAPriorWriterAbsent(ctx context.Context, namespace, endpoint string) error {
	client, vms, err := newDSGAKubeClients(k)
	if err != nil {
		return err
	}
	name := kubeName(endpoint)
	_, err = vms.NeonvmV1().VirtualMachines(namespace).Get(ctx, name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return errDSGACredentialWriterRetained
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "vm.neon.tech/name=" + name, Limit: 1})
	if err != nil || len(pods.Items) != 0 || pods.Continue != "" {
		return errDSGACredentialWriterRetained
	}
	return nil
}

// Keep the project as the parent lock for writer/reader admission and retained-source checks.
func lockDSGAEndpointAuthority(ctx context.Context, tx pgx.Tx, org, project, actor string) (string, uint32, string, error) {
	var orgState, projectOrg, projectState, kind, tenant, region string
	var version uint32
	if err := tx.QueryRow(ctx, `SELECT state FROM organizations WHERE id=$1 FOR NO KEY UPDATE`, org).Scan(&orgState); err != nil {
		return "", 0, "", dsgaAuthorityReadError(err)
	}
	if err := tx.QueryRow(ctx, `SELECT org_id,state,source,tenant_id,postgres_version,region_id FROM projects
 WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, project).Scan(&projectOrg, &projectState, &kind, &tenant, &version, &region); err != nil {
		return "", 0, "", dsgaAuthorityReadError(err)
	}
	var orgRole, grant string
	if err := tx.QueryRow(ctx, `SELECT m.role FROM organization_members m JOIN users u ON u.id=m.user_id AND NOT u.disabled
 WHERE m.org_id=$1 AND m.user_id=$2 FOR SHARE OF m,u`, org, actor).Scan(&orgRole); err != nil {
		return "", 0, "", dsgaAuthorityReadError(err)
	}
	err := tx.QueryRow(ctx, `SELECT role FROM project_grants WHERE project_id=$1 AND user_id=$2 FOR SHARE`, project, actor).Scan(&grant)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", 0, "", err
	}
	if orgState != "active" || projectOrg != org || projectState != "ready" || kind != "managed" || effectivePermission(orgRole, grant) < 2 || !dsgaSupportedPostgresVersion(version) {
		return "", 0, "", errDSGACreationAuthority
	}
	err = lockDSGARequestCredentialForProject(ctx, tx, org, actor, project)
	return tenant, version, region, err
}

func lockDSGARetainedBranch(ctx context.Context, tx pgx.Tx, project, branch string) (string, error) {
	var timeline string
	err := tx.QueryRow(ctx, `SELECT b.timeline_id FROM branches b
 JOIN branch_service_instances v ON v.branch_id=b.id AND v.service_kind='postgres' AND v.driver_version='dsga-v1'
 WHERE b.id=$1 AND b.project_id=$2 AND b.state='ready' AND b.deleted_at IS NULL AND b.expires_at IS NULL FOR UPDATE OF b`, branch, project).Scan(&timeline)
	if err != nil {
		return "", dsgaAuthorityReadError(err)
	}
	if err := checkDefaultOwnerReservation(ctx, tx, project, branch); err != nil {
		return "", err
	}
	var catalogPending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND resource_type='branch_catalog' AND resource_id=$2 AND state IN ('queued','running','retry_wait'))`, project, branch).Scan(&catalogPending); err != nil {
		return "", err
	}
	if catalogPending {
		return "", errDSGACreationConflict
	}
	return timeline, nil
}

func readDSGARetainedWriter(ctx context.Context, tx pgx.Tx, p createPayload) (dsgaEndpointCredentialSource, error) {
	var source dsgaEndpointCredentialSource
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT e.id,o.id,o.payload#>'{dsga_creation,credentials}'
 FROM endpoints e JOIN operations o ON o.project_id=e.project_id AND o.payload->>'endpoint_id'=e.id
 JOIN dsga_creation_bindings b ON b.operation_id=o.id AND b.config_uid IS NOT NULL
 WHERE e.project_id=$1 AND e.branch_id=$2 AND e.endpoint_type='read_write' AND e.id<>$3
 AND o.action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1') AND o.state='succeeded'
 AND o.payload->>'branch_id'=$2 AND o.payload->>'tenant_id'=$4 AND o.payload->>'timeline_id'=$5
 AND (($6='read_only' AND e.state='active' AND e.deleted_at IS NULL)
   OR ($6='read_write' AND e.state='deleted' AND e.deleted_at IS NOT NULL AND EXISTS(
     SELECT 1 FROM operations d WHERE d.id=e.deletion_operation_id AND d.project_id=e.project_id AND d.resource_id=e.id
     AND d.action='delete_endpoint' AND d.state='succeeded')))
 ORDER BY e.created_at DESC,e.id DESC LIMIT 1`, p.ProjectID, p.BranchID, p.EndpointID, p.TenantID, p.TimelineID, p.EndpointType).
		Scan(&source.Endpoint, &source.Operation, &raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return source, errDSGACreationConflict
		}
		return source, err
	}
	if decodeDSGAObject(raw, &source.Reference) != nil || !source.valid(p.EndpointID) {
		return source, errDSGACredentialSource
	}
	return source, nil
}

func (s *server) reserveDSGAEndpoint(ctx context.Context, actor, org, project, scope, key, hash, mode string, body endpointCreateRequest) (dsgaProjectReservation, error) {
	var empty dsgaProjectReservation
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	tenant, version, region, err := lockDSGAEndpointAuthority(ctx, tx, org, project, actor)
	if err != nil {
		return empty, err
	}
	timeline, err := lockDSGARetainedBranch(ctx, tx, project, body.BranchID)
	if err != nil {
		return empty, err
	}
	pl, el, err := lockDSGAReservationQuota(ctx, tx, org)
	if err != nil {
		return empty, err
	}
	if _, err := tx.Exec(ctx, `SELECT dsga_lock_creation_key($1,$2)`, actor, keyHash(key)); err != nil {
		return empty, err
	}
	r, err := readDSGAReservation(ctx, tx, actor, keyHash(key))
	if err == nil {
		p := r.Draft.Payload
		if r.BaseAction != "create_endpoint" || r.RequestHash != hash || r.Org != org || r.Scope != scope || p.ProjectID != project ||
			p.BranchID != body.BranchID || p.EndpointType != mode || p.TenantID != tenant || p.TimelineID != timeline {
			return empty, errDSGACreationConflict
		}
		if !r.Accepted {
			source, err := readDSGARetainedWriter(ctx, tx, p)
			if err != nil || source != *r.Draft.CredentialSource {
				return empty, errDSGACreationConflict
			}
		}
		return r, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if mode == "read_write" {
		if err := verifyDSGAWriterSlot(ctx, tx, project, body.BranchID); err != nil {
			return empty, err
		}
	}
	if err := checkDSGAReservationQuota(ctx, tx, org, pl, el, false); err != nil {
		return empty, err
	}
	policy, err := selectDSGAProjectPolicy(s.kube.namespace, version, region)
	if err != nil || !policy.Creation.RetainedCatalog {
		return empty, errDSGARuntimeUnavailable
	}
	bounds := policy.Bounds
	if body.Autoscaling != nil {
		bounds = *body.Autoscaling
	}
	if bounds.MinCPU < policy.Bounds.MinCPU || bounds.MaxCPU > policy.Bounds.MaxCPU || bounds.MinMem < policy.Bounds.MinMem || bounds.MaxMem > policy.Bounds.MaxMem {
		return empty, errDSGAProjectBounds
	}
	p := creationPayload(project, body.BranchID, "ep_"+stableSuffix(actor, scope, key), tenant, timeline, bounds)
	p.EndpointType = mode
	source, err := readDSGARetainedWriter(ctx, tx, p)
	if err != nil {
		return empty, err
	}
	plan, err := newDSGACreationPlan(p, policy.Creation, time.Now())
	if err != nil {
		return empty, errDSGAProjectBounds
	}
	plan.RetainCatalog = true
	if err := plan.validate(p); err != nil {
		return empty, err
	}
	r = dsgaProjectReservation{Actor: actor, KeyHash: keyHash(key), RequestHash: hash, Scope: scope, Org: org,
		Operation: newID("op_"), BaseAction: "create_endpoint",
		Draft: dsgaCreationDraft{ProtocolVersion: 1, Region: region, Payload: p, Plan: plan, CredentialSource: &source}}
	raw, err := json.Marshal(r.Draft)
	if err != nil {
		return empty, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dsga_creation_reservations(actor_id,key_hash,request_hash,scope,org_id,base_action,operation_id,draft)
 VALUES($1,$2,$3,$4,$5,'create_endpoint',$6,$7)`, actor, r.KeyHash, hash, scope, org, r.Operation, raw); err != nil {
		return empty, err
	}
	return r, tx.Commit(ctx)
}

func (s *server) acceptDSGAEndpoint(ctx context.Context, r dsgaProjectReservation, credential dsgaCredentialReference, key, request string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if credential.validate() != nil || r.BaseAction != "create_endpoint" || r.KeyHash != keyHash(key) {
		return false, errDSGACreationPlan
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	p, plan := r.Draft.Payload, r.Draft.Plan
	tenant, version, region, err := lockDSGAEndpointAuthority(ctx, tx, r.Org, p.ProjectID, r.Actor)
	if err != nil {
		return false, err
	}
	timeline, err := lockDSGARetainedBranch(ctx, tx, p.ProjectID, p.BranchID)
	if err != nil || tenant != p.TenantID || timeline != p.TimelineID || version != plan.PostgresVersion || region != r.Draft.Region {
		return false, errDSGACreationConflict
	}
	pl, el, err := lockDSGAReservationQuota(ctx, tx, r.Org)
	if err != nil {
		return false, err
	}
	current, err := readDSGAReservation(ctx, tx, r.Actor, r.KeyHash)
	if err != nil {
		return false, err
	}
	if current.Operation != r.Operation || current.RequestHash != r.RequestHash || current.BaseAction != r.BaseAction {
		return false, errDSGACreationConflict
	}
	if current.Accepted {
		if credential != current.Credentials {
			return false, errDSGACredentialSource
		}
		return false, tx.Commit(ctx)
	}
	source, err := readDSGARetainedWriter(ctx, tx, p)
	if err != nil || source != *current.Draft.CredentialSource {
		return false, errDSGACreationConflict
	}
	if err := checkDSGAReservationQuota(ctx, tx, r.Org, pl, el, true); err != nil {
		return false, err
	}
	if p.EndpointType == "read_write" {
		if err := verifyDSGAWriterSlot(ctx, tx, p.ProjectID, p.BranchID); err != nil {
			return false, err
		}
	}
	if current.Credentials != (dsgaCredentialReference{}) && current.Credentials != credential {
		return false, errDSGACredentialSource
	}
	if current.Credentials == (dsgaCredentialReference{}) {
		raw, err := json.Marshal(credential)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE dsga_creation_reservations SET credential_reference=$3 WHERE actor_id=$1 AND key_hash=$2`, r.Actor, r.KeyHash, raw); err != nil {
			return false, err
		}
	}
	plan.Credentials = credential
	name := kubeName(p.EndpointID)
	if _, err := tx.Exec(ctx, `INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,
 role_name,database_name,min_cpu_milli,max_cpu_milli,min_memory_mib,max_memory_mib,slot_size_mib,scale_to_zero,endpoint_type,created_at,updated_at)
 VALUES($1,$2,$3,$4,'neonvm',$5,$5,'provisioning',$6,$7,$8,$9,$10,$11,$12,false,$13,clock_timestamp(),clock_timestamp())`,
		p.EndpointID, p.ProjectID, p.BranchID, selector(p.EndpointID), name, dsgaApplicationRole, dsgaApplicationDatabase,
		p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem, plan.MemorySlotBytes/(1<<20), p.EndpointType); err != nil {
		return false, err
	}
	if err := addDSGACreateOperation(ctx, tx, r.BaseAction, "endpoint", p.EndpointID, p.ProjectID, r.Operation, r.Actor, request, p, plan, key, r.RequestHash, dsgaEndpointCreateSteps()); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE dsga_creation_reservations SET accepted_id=operation_id,accepted_at=clock_timestamp() WHERE actor_id=$1 AND key_hash=$2`, r.Actor, r.KeyHash); err != nil {
		return false, err
	}
	if err := lockDSGARequestCredentialForProject(ctx, tx, r.Org, r.Actor, p.ProjectID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func verifyDSGAWriterSlot(ctx context.Context, tx pgx.Tx, project, branch string) error {
	var writer bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND endpoint_type='read_write' AND deleted_at IS NULL)`, project, branch).Scan(&writer); err != nil {
		return err
	}
	if writer {
		return errDSGAEndpointWriterExists
	}
	return nil
}
