package control

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

type publishedFunctionRuntime struct {
	target    functionInstanceTarget
	instance  functionInstanceRecord
	bootstrap functionBootstrapReference
	bootID    string
	version   int64
}

type functionObservationError struct{ code, message string }

func (e functionObservationError) Error() string { return e.code }

func (s *server) loadPublishedFunction(ctx context.Context, project, branch, slug string) (publishedFunctionRuntime, error) {
	var out publishedFunctionRuntime
	p, i, b := &out.target, &out.instance, &out.bootstrap
	var vmUID, serviceUID string
	err := s.db.QueryRow(ctx, `SELECT f.id,f.project_id,f.branch_id,f.slug,i.deployment_id,i.generation,i.id,i.vm_name,i.service_name,i.bootstrap_secret_ref,i.vm_uid,i.service_uid,i.boot_id,
 b.namespace,b.secret_name,b.secret_uid,b.content_mac,b.mac_key_version,d.bundle_digest,f.endpoint_id,f.version
 FROM function_definitions f JOIN function_instances i ON i.function_id=f.id AND i.deployment_id=f.active_deployment_id
 JOIN function_deployments d ON d.id=i.deployment_id JOIN function_instance_bootstraps b ON b.instance_id=i.id
 JOIN function_sql_credentials c ON c.function_id=f.id AND c.credential_generation=1
 JOIN projects p ON p.id=f.project_id JOIN organizations o ON o.id=f.org_id JOIN branches r ON r.id=f.branch_id
 WHERE f.project_id=$1 AND f.branch_id=$2 AND f.slug=$3 AND f.state='active' AND f.generation=i.generation AND i.state='ready'
 AND p.state='ready' AND p.deleted_at IS NULL AND r.state='ready' AND r.deleted_at IS NULL AND o.state='active' AND d.state='completed'`,
		project, branch, slug).Scan(&p.FunctionID, &p.ProjectID, &p.BranchID, &p.Slug, &p.DeploymentID, &p.Generation,
		&i.Scope.InstanceID, &i.VMName, &i.ServiceName, &i.SecretName, &vmUID, &serviceUID, &out.bootID,
		&b.Namespace, &b.Name, &b.UID, &b.MAC, &b.MACKeyVersion, &b.BundleDigest, &b.WriterID, &out.version)
	if isNoRows(err) {
		return out, functionObservationError{"function_not_ready", "An active published Function instance is required"}
	}
	if err != nil {
		return out, functionObservationError{"metadata_unavailable", "Published Function metadata unavailable"}
	}
	i.Scope.ProjectID, i.Scope.BranchID, i.Scope.DeploymentID, i.Scope.Generation, i.Scope.Slug = p.ProjectID, p.BranchID, p.DeploymentID, p.Generation, p.Slug
	i.State, i.VMUID, i.ServiceUID = "ready", &vmUID, &serviceUID
	b.Scope, b.FunctionID = i.Scope, p.FunctionID
	return out, nil
}

// Observe the saved original identity. No SQL connection, customer invocation,
// cold wake, Kubernetes mutation or metadata timestamp update is performed.
// The returned client must be closed; its private address/key never serialize.
func (s *server) observePublishedFunction(ctx context.Context, runtime publishedFunctionRuntime) (*functions.InstanceClient, functions.InstanceStatus, error) {
	var empty functions.InstanceStatus
	p, i, b := runtime.target, runtime.instance, runtime.bootstrap
	if s.functions == nil || s.kube == nil || b.MACKeyVersion != s.functions.input.KeyVersion {
		return nil, empty, functionObservationError{"function_trust_unavailable", "Original verification key required"}
	}
	config, err := s.kube.readFunctionBootstrap(ctx, s.functions.key, b)
	if err != nil || config.Config.ManagerCA != s.functions.guestCA {
		return nil, empty, functionObservationError{"function_trust_unavailable", "Original Function bootstrap unavailable"}
	}
	plan := functionRuntimePlan{Target: p, Instance: i, Bootstrap: b, Image: s.functions.input.VMImage, NodeName: s.functions.input.NodeName}
	vm, err := s.kube.request(ctx, http.MethodGet, s.kube.path("vm", i.VMName), nil)
	if err != nil {
		return nil, empty, functionObservationError{"function_runtime_unavailable", "Original VM unavailable"}
	}
	if _, err = verifyFunctionRuntimeResource(vm, plan, "vm"); err != nil || stringVal(nested(vm, "status", "phase")) != "Running" {
		return nil, empty, functionObservationError{"function_runtime_unavailable", "Original VM identity or phase rejected"}
	}
	service, err := s.kube.request(ctx, http.MethodGet, s.kube.path("service", i.ServiceName), nil)
	if err != nil {
		return nil, empty, functionObservationError{"function_runtime_unavailable", "Original Service unavailable"}
	}
	verified, err := verifyFunctionRuntimeResource(service, plan, "service")
	if err != nil {
		return nil, empty, functionObservationError{"function_runtime_unavailable", "Original Service identity rejected"}
	}
	key, err := base64.StdEncoding.Strict().DecodeString(config.Config.ManagerKey)
	if err != nil {
		return nil, empty, functionObservationError{"function_trust_unavailable", "Original manager identity unavailable"}
	}
	target, err := netip.ParseAddrPort(verified.ClusterIP + ":9090")
	if err != nil {
		return nil, empty, functionObservationError{"function_runtime_unavailable", "Original Service address rejected"}
	}
	client, err := functions.NewInstanceClient(i.Scope, target, key, s.functions.guestCA)
	if err != nil {
		return nil, empty, functionObservationError{"function_trust_unavailable", "Original manager identity rejected"}
	}
	status, err := client.Status(ctx)
	if err != nil || status.BootID != runtime.bootID {
		client.Close()
		return nil, empty, functionObservationError{"function_runtime_unavailable", "Original supervisor boot unavailable"}
	}
	var current bool
	err = s.db.QueryRow(ctx, `SELECT f.state='active' AND f.generation=$2 AND f.active_deployment_id=$3 AND f.version=$6
 AND i.state='ready' AND i.boot_id=$5 AND i.generation=$2 AND i.deployment_id=$3
 AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active'
 FROM function_definitions f JOIN function_instances i ON i.function_id=f.id AND i.id=$4
 JOIN projects p ON p.id=f.project_id JOIN branches b ON b.id=f.branch_id JOIN organizations o ON o.id=f.org_id
 WHERE f.id=$1`, p.FunctionID, p.Generation, p.DeploymentID, i.Scope.InstanceID, runtime.bootID, runtime.version).Scan(&current)
	if err != nil || !current {
		client.Close()
		return nil, empty, functionObservationError{"function_intent_changed", "Function intent changed during observation"}
	}
	return client, status, nil
}

func failFunctionObservation(w http.ResponseWriter, r *http.Request, err error) {
	var failure functionObservationError
	if !errors.As(err, &failure) {
		fail(w, r, 503, "function_runtime_unavailable", "Original Function observation unavailable")
		return
	}
	code := 503
	if failure.code == "function_not_ready" || failure.code == "function_intent_changed" {
		code = 409
	}
	fail(w, r, code, failure.code, failure.message)
}

// Deliberately exclude customer NodeObservation, keys, environment, SQL URL,
// Pod IPs and raw transport errors. Background work is explicitly unmetered;
// these observations are never authorization for automatic retirement.
type functionRuntimeObservation struct {
	FunctionID                string     `json:"function_id"`
	DefinitionState           string     `json:"definition_state"`
	DefinitionVersionToken    string     `json:"definition_version_token"`
	GenerationToken           string     `json:"generation_token"`
	Observed                  bool       `json:"observed"`
	ObservationSource         string     `json:"observation_source"`
	CheckedAt                 time.Time  `json:"checked_at"`
	InstanceID                string     `json:"instance_id,omitempty"`
	DeploymentID              string     `json:"deployment_id,omitempty"`
	VMPhase                   string     `json:"vm_phase,omitempty"`
	NodeReady                 *bool      `json:"node_ready,omitempty"`
	Draining                  *bool      `json:"draining,omitempty"`
	ActiveInvocations         *int       `json:"active_invocations,omitempty"`
	LastActivity              *time.Time `json:"last_activity,omitempty"`
	BackgroundCountersTrusted bool       `json:"background_counters_trusted"`
}

func observedFunctionRuntime(runtime publishedFunctionRuntime, status functions.InstanceStatus) functionRuntimeObservation {
	return functionRuntimeObservation{FunctionID: runtime.target.FunctionID, DefinitionState: "active", DefinitionVersionToken: strconv.FormatInt(runtime.version, 10), GenerationToken: strconv.FormatInt(runtime.target.Generation, 10), Observed: true, ObservationSource: "authenticated_supervisor", CheckedAt: time.Now(), InstanceID: runtime.instance.Scope.InstanceID, DeploymentID: runtime.target.DeploymentID, VMPhase: "Running", NodeReady: &status.NodeReady, Draining: &status.Draining, ActiveInvocations: &status.ActiveInvocations, LastActivity: &status.LastActivity, BackgroundCountersTrusted: false}
}

func (s *server) functionRuntimeRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.functionReadBranch(w, r) {
		return
	}
	if !functions.ValidSlug(r.PathValue("slug")) {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	var id, state string
	var version, generation int64
	err := s.db.QueryRow(r.Context(), `SELECT id,state,version,generation FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted'`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug")).Scan(&id, &state, &version, &generation)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Function metadata unavailable")
		return
	}
	if state != "active" || s.functions == nil {
		jsonResponse(w, 200, functionRuntimeObservation{FunctionID: id, DefinitionState: state, DefinitionVersionToken: strconv.FormatInt(version, 10), GenerationToken: strconv.FormatInt(generation, 10), Observed: false, ObservationSource: "metadata_only", CheckedAt: time.Now(), BackgroundCountersTrusted: false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	runtime, err := s.loadPublishedFunction(ctx, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug"))
	if err != nil {
		failFunctionObservation(w, r, err)
		return
	}
	client, status, err := s.observePublishedFunction(ctx, runtime)
	if err != nil {
		failFunctionObservation(w, r, err)
		return
	}
	defer client.Close()
	jsonResponse(w, 200, observedFunctionRuntime(runtime, status))
}
