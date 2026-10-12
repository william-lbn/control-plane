package control

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// Console invocation is an authenticated JSON envelope. Customer HTML/JS and
// Set-Cookie are NEVER served as executable content on the Console origin.
// A public raw HTTP/SSE/WS ingress requires a separate origin/admission gate.
func (s *server) functionConsoleInvoke(w http.ResponseWriter, r *http.Request) {
	if !s.functionsConfigured(w, r) {
		return
	}
	var input struct {
		Method  string              `json:"method"`
		Path    string              `json:"path"`
		Headers map[string][]string `json:"headers"`
		Body    string              `json:"body"`
	}
	if readJSON(r, &input) != nil || len(input.Body) > functions.MaxInvokeBytes || !strings.HasPrefix(input.Path, "/") {
		fail(w, r, 422, "invalid_invocation", "Bounded method/path/body required")
		return
	}
	parsed, err := url.ParseRequestURI(input.Path)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Fragment != "" {
		fail(w, r, 422, "invalid_invocation", "An application-relative path is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var p functionInstanceTarget
	var instance functionInstanceRecord
	var boot functionBootstrapReference
	var vmUID, serviceUID, bootID string
	err = s.db.QueryRow(ctx, `SELECT f.id,f.project_id,f.branch_id,f.slug,i.deployment_id,i.generation,i.id,i.vm_name,i.service_name,i.bootstrap_secret_ref,i.vm_uid,i.service_uid,i.boot_id,
 b.namespace,b.secret_name,b.secret_uid,b.content_mac,b.mac_key_version,d.bundle_digest,f.endpoint_id
 FROM function_definitions f JOIN function_instances i ON i.function_id=f.id AND i.deployment_id=f.active_deployment_id
 JOIN function_deployments d ON d.id=i.deployment_id JOIN function_instance_bootstraps b ON b.instance_id=i.id
 JOIN function_sql_credentials c ON c.function_id=f.id AND c.credential_generation=1
 JOIN projects p ON p.id=f.project_id JOIN organizations o ON o.id=f.org_id JOIN branches r ON r.id=f.branch_id
 WHERE f.project_id=$1 AND f.branch_id=$2 AND f.slug=$3 AND f.state='active' AND f.generation=i.generation AND i.state='ready'
 AND p.state='ready' AND p.deleted_at IS NULL AND r.state='ready' AND r.deleted_at IS NULL AND o.state='active' AND d.state='completed'`,
		r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug")).Scan(&p.FunctionID, &p.ProjectID, &p.BranchID, &p.Slug, &p.DeploymentID, &p.Generation,
		&instance.Scope.InstanceID, &instance.VMName, &instance.ServiceName, &instance.SecretName, &vmUID, &serviceUID, &bootID,
		&boot.Namespace, &boot.Name, &boot.UID, &boot.MAC, &boot.MACKeyVersion, &boot.BundleDigest, &boot.WriterID)
	if err != nil {
		fail(w, r, 409, "function_not_ready", "An active published Function instance is required")
		return
	}
	instance.Scope.ProjectID, instance.Scope.BranchID, instance.Scope.DeploymentID, instance.Scope.Generation, instance.Scope.Slug = p.ProjectID, p.BranchID, p.DeploymentID, p.Generation, p.Slug
	instance.State = "starting"
	if input.Headers == nil {
		input.Headers = map[string][]string{}
	}
	invocation := functions.Invocation{BootID: bootID, Generation: p.Generation, Method: input.Method,
		URL: "https://function.invalid/functions/v1/" + p.BranchID + "/" + p.Slug + input.Path, Headers: input.Headers, Body: []byte(input.Body)}
	if !functions.ValidateInvocation(instance.Scope, invocation) {
		fail(w, r, 422, "invalid_invocation", "Invocation method, path or headers rejected before dispatch")
		return
	}
	instance.VMUID = &vmUID
	instance.ServiceUID = &serviceUID
	boot.Scope, boot.FunctionID = instance.Scope, p.FunctionID
	if boot.MACKeyVersion != s.functions.input.KeyVersion {
		fail(w, r, 503, "function_trust_unavailable", "Original verification key required")
		return
	}
	config, err := s.kube.readFunctionBootstrap(ctx, s.functions.key, boot)
	if err != nil || config.Config.ManagerCA != s.functions.guestCA {
		fail(w, r, 503, "function_trust_unavailable", "Original Function bootstrap unavailable")
		return
	}
	plan := functionRuntimePlan{Target: p, Instance: instance, Bootstrap: boot, Image: s.functions.input.VMImage, NodeName: s.functions.input.NodeName}
	vm, err := s.kube.request(ctx, http.MethodGet, s.kube.path("vm", instance.VMName), nil)
	if err != nil {
		fail(w, r, 503, "function_runtime_unavailable", "Original VM unavailable")
		return
	}
	if _, err = verifyFunctionRuntimeResource(vm, plan, "vm"); err != nil || stringVal(nested(vm, "status", "phase")) != "Running" {
		fail(w, r, 503, "function_runtime_unavailable", "Original VM identity or phase rejected")
		return
	}
	service, err := s.kube.request(ctx, http.MethodGet, s.kube.path("service", instance.ServiceName), nil)
	if err != nil {
		fail(w, r, 503, "function_runtime_unavailable", "Original Service unavailable")
		return
	}
	verified, err := verifyFunctionRuntimeResource(service, plan, "service")
	if err != nil {
		fail(w, r, 503, "function_runtime_unavailable", "Original Service identity rejected")
		return
	}
	key, err := base64.StdEncoding.Strict().DecodeString(config.Config.ManagerKey)
	if err != nil {
		fail(w, r, 503, "function_trust_unavailable", "Original manager identity unavailable")
		return
	}
	client, err := functions.NewInstanceClient(instance.Scope, netip.MustParseAddrPort(verified.ClusterIP+":9090"), key, s.functions.guestCA)
	if err != nil {
		fail(w, r, 503, "function_trust_unavailable", "Original manager identity rejected")
		return
	}
	defer client.Close()
	status, err := client.Status(ctx)
	if err != nil || status.BootID != bootID || !status.NodeReady || status.Draining {
		fail(w, r, 503, "function_runtime_unavailable", "Original boot is not ready")
		return
	}
	var current bool
	err = s.db.QueryRow(ctx, `SELECT f.state='active' AND f.generation=$2 AND f.active_deployment_id=$3
 AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active'
 FROM function_definitions f JOIN projects p ON p.id=f.project_id JOIN branches b ON b.id=f.branch_id JOIN organizations o ON o.id=f.org_id WHERE f.id=$1`, p.FunctionID, p.Generation, p.DeploymentID).Scan(&current)
	if err != nil || !current {
		fail(w, r, 409, "function_intent_changed", "Function intent changed before invocation")
		return
	}
	response, err := client.Invoke(ctx, invocation)
	if err != nil {
		fail(w, r, 502, "function_invocation_outcome_unknown", "Invocation outcome may be unknown; inspect before retrying")
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, functions.MaxInvokeBytes+1))
	if err != nil || len(body) > functions.MaxInvokeBytes {
		fail(w, r, 502, "function_invocation_outcome_unknown", "Invocation response failed or exceeded limits; do not replay automatically")
		return
	}
	_, _ = s.db.Exec(ctx, `UPDATE function_instances SET last_invoked_at=now(),updated_at=now() WHERE id=$1 AND boot_id=$2`, instance.Scope.InstanceID, bootID)
	jsonResponse(w, 200, record{"function_id": p.FunctionID, "deployment_id": p.DeploymentID, "status_code": response.StatusCode, "headers": response.Header, "body": base64.StdEncoding.EncodeToString(body), "body_encoding": "base64", "request_id": requestID(r)})
}
