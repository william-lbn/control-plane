package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

const resetInspectionAnnotation = "neon-control/reset-inspection"
const resetGrantAnnotation = "neon-control/reset-grant"
const resetGrantUIDAnnotation = "neon-control/reset-grant-uid"
const resetGrantHeader = "X-Neon-Reset-Grant"

// Only a private, disposable inspection VM may receive this short-lived grant.
// It binds one complete configuration, fixed Secret UID, native scope and the
// actual SQL observation. The ordinary catalog gateway policy stays unchanged.
type branchResetPlatformGrant struct {
	Operation         string             `json:"operation"`
	Project           string             `json:"project"`
	Branch            string             `json:"branch"`
	Endpoint          string             `json:"endpoint"`
	Scope             catalogNativeScope `json:"scope"`
	ConfigUID         string             `json:"config_uid"`
	SourceSHA256      string             `json:"source_sha256"`
	RequestedSHA256   string             `json:"requested_sha256"`
	ObservationSHA256 string             `json:"observation_sha256"`
	AdminOID          uint32             `json:"admin_oid"`
	ProbeOID          uint32             `json:"probe_oid"`
	ExpiresAt         time.Time          `json:"expires_at"`
}

func branchResetConfigDigest(config map[string]any) string {
	raw, _ := json.Marshal(config)
	return catalogMarkerDigest(string(raw))
}

func resetInspectionConfig(config map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	var next map[string]any
	if json.Unmarshal(raw, &next) != nil {
		return nil, errBranchResetConfig
	}
	spec, ok := next["spec"].(map[string]any)
	if !ok || spec["skip_pg_catalog_updates"] != true {
		return nil, errBranchResetConfig
	}
	mode, valid := gatewayComputeMode(next)
	if !valid || mode != "Primary" {
		return nil, errBranchResetConfig
	}
	cluster, ok := spec["cluster"].(map[string]any)
	if !ok {
		return nil, errBranchResetConfig
	}
	settings, ok := cluster["settings"].([]any)
	if !ok {
		return nil, errBranchResetConfig
	}
	seen := map[string]bool{}
	for _, raw := range settings {
		item, ok := raw.(map[string]any)
		name := stringVal(item["name"])
		if !ok || name == "" || seen[name] {
			return nil, errBranchResetConfig
		}
		seen[name] = true
		if name == "cron.launch_active_jobs" {
			item["value"] = "off"
			item["vartype"] = "bool"
		}
	}
	if !seen["cron.launch_active_jobs"] {
		settings = append(settings, map[string]any{"name": "cron.launch_active_jobs", "value": "off", "vartype": "bool"})
	}
	cluster["settings"] = settings
	return next, nil
}

func branchResetPlatformProjection(stored map[string]any, desiredRoots []any) (map[string]any, error) {
	isolated, err := resetInspectionConfig(stored)
	if err != nil || !reflect.DeepEqual(isolated, stored) {
		return nil, errBranchResetConfig
	}
	spec := isolated["spec"].(map[string]any)
	if spec["local_proxy_config"] != nil || spec["drop_subscriptions_before_start"] != nil && spec["drop_subscriptions_before_start"] != false {
		return nil, errBranchResetConfig
	}
	cluster := spec["cluster"].(map[string]any)
	original, ok := cluster["roles"].([]any)
	if !ok || len(original) != 2 || len(desiredRoots) != 2 {
		return nil, errBranchResetConfig
	}
	dbs, ok := cluster["databases"].([]any)
	if !ok || len(dbs) != 0 {
		return nil, errBranchResetConfig
	}
	deltas, ok := spec["delta_operations"].([]any)
	if !ok || len(deltas) != 0 {
		return nil, errBranchResetConfig
	}
	names := map[string]map[string]any{}
	for _, raw := range original {
		item, ok := raw.(map[string]any)
		name := stringVal(item["name"])
		if !ok || name != "cloud_admin" && name != "control_probe" || names[name] != nil || !functionsql.ValidVerifier(stringVal(item["encrypted_password"])) {
			return nil, errBranchResetConfig
		}
		names[name] = item
	}
	desired := []any{}
	seen := map[string]bool{}
	for _, raw := range desiredRoots {
		role, ok := raw.(map[string]any)
		name := stringVal(role["name"])
		if !ok || names[name] == nil || seen[name] || !functionsql.ValidVerifier(stringVal(role["encrypted_password"])) {
			return nil, errBranchResetConfig
		}
		// Restoring a platform password never changes its existing role options.
		comparable := map[string]any{}
		for key, value := range role {
			comparable[key] = value
		}
		comparable["encrypted_password"] = names[name]["encrypted_password"]
		if !reflect.DeepEqual(comparable, names[name]) {
			return nil, errBranchResetConfig
		}
		seen[name] = true
		desired = append(desired, role)
	}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(encoded, &desired) != nil {
		return nil, errBranchResetConfig
	}
	cluster["roles"] = desired
	spec["skip_pg_catalog_updates"] = false
	return isolated, nil
}

func makeBranchResetPlatformGrant(operation, branch, endpoint, configUID string, stored map[string]any, roots []any, observed catalogNativeSnapshot, expectedAdmin, expectedProbe uint32, now time.Time) (branchResetPlatformGrant, map[string]any, error) {
	grant := branchResetPlatformGrant{Operation: operation, Project: stringVal(nested(stored, "spec", "cluster", "cluster_id")), Branch: branch, Endpoint: endpoint, Scope: observed.Scope, ConfigUID: configUID, AdminOID: expectedAdmin, ProbeOID: expectedProbe, ExpiresAt: now.UTC().Add(60 * time.Second)}
	if !branchResetOperationID.MatchString(operation) || !dataBranchID.MatchString(branch) || !functionWriterEndpointID.MatchString(endpoint) || endpoint != branchResetProbeEndpoint(operation) || !functionResourceUID.MatchString(configUID) ||
		!observed.Scope.valid() || expectedAdmin == 0 || expectedProbe == 0 || expectedAdmin == expectedProbe || validateCatalogNativeSnapshot(observed) != nil {
		return grant, nil, errBranchResetConfig
	}
	admin, probe := false, false
	for _, role := range observed.Roles {
		if role.Name == "cloud_admin" {
			admin = role.OID == expectedAdmin && role.Flags.Login
		}
		if role.Name == "control_probe" {
			probe = role.OID == expectedProbe && role.Flags.Login
		}
	}
	settings, _ := nested(stored, "spec", "cluster", "settings").([]any)
	scope := map[string]string{}
	for _, raw := range settings {
		if setting, ok := raw.(map[string]any); ok {
			scope[stringVal(setting["name"])] = stringVal(setting["value"])
		}
	}
	if !admin || !probe || scope["neon.tenant_id"] != observed.Scope.TenantID || scope["neon.timeline_id"] != observed.Scope.TimelineID {
		return grant, nil, errBranchResetConfig
	}
	request, err := branchResetPlatformProjection(stored, roots)
	if err != nil {
		return grant, nil, err
	}
	raw, _ := json.Marshal(observed)
	grant.SourceSHA256, grant.RequestedSHA256, grant.ObservationSHA256 = branchResetConfigDigest(stored), branchResetConfigDigest(request), catalogMarkerDigest(string(raw))
	return grant, request, nil
}

func validBranchResetPlatformGrant(value branchResetPlatformGrant, configSecret map[string]any, stored, request map[string]any, now time.Time) bool {
	if !branchResetOperationID.MatchString(value.Operation) || !dataBranchID.MatchString(value.Branch) || !functionWriterEndpointID.MatchString(value.Endpoint) || value.Endpoint != branchResetProbeEndpoint(value.Operation) || !functionResourceUID.MatchString(value.ConfigUID) || !value.Scope.valid() ||
		!owned(configSecret, value.Project, value.Endpoint) || nested(configSecret, "metadata", "uid") != value.ConfigUID || nested(configSecret, "metadata", "labels", "neon-control/reset-operation") != value.Operation || nested(configSecret, "metadata", "annotations", resetInspectionAnnotation) != value.Operation ||
		value.AdminOID == 0 || value.ProbeOID == 0 || value.AdminOID == value.ProbeOID || len(value.ObservationSHA256) != 64 || strings.Trim(value.ObservationSHA256, "abcdef0123456789") != "" ||
		!value.ExpiresAt.After(now) || value.ExpiresAt.After(now.Add(90*time.Second)) || value.Project != stringVal(nested(stored, "spec", "cluster", "cluster_id")) ||
		value.SourceSHA256 != branchResetConfigDigest(stored) || value.RequestedSHA256 != branchResetConfigDigest(request) {
		return false
	}
	scope := map[string]string{}
	settings, _ := nested(stored, "spec", "cluster", "settings").([]any)
	for _, raw := range settings {
		if item, ok := raw.(map[string]any); ok {
			scope[stringVal(item["name"])] = stringVal(item["value"])
		}
	}
	if scope["neon.tenant_id"] != value.Scope.TenantID || scope["neon.timeline_id"] != value.Scope.TimelineID {
		return false
	}
	roots, ok := nested(request, "spec", "cluster", "roles").([]any)
	if !ok {
		return false
	}
	expected, err := branchResetPlatformProjection(stored, roots)
	return err == nil && reflect.DeepEqual(expected, request)
}

func (g *computeGateway) validBranchResetGrant(ctx context.Context, ref string, configSecret, stored, request map[string]any) bool {
	if len(ref) != len("branch-reset-grant-")+32 || !strings.HasPrefix(ref, "branch-reset-grant-") || strings.Trim(strings.TrimPrefix(ref, "branch-reset-grant-"), "abcdef0123456789") != "" ||
		nested(configSecret, "metadata", "annotations", resetGrantAnnotation) != ref {
		return false
	}
	secret, err := g.kube.request(ctx, http.MethodGet, g.kube.path("secret", ref), nil)
	if err != nil || secret["immutable"] != true || !functionResourceUID.MatchString(stringVal(nested(secret, "metadata", "uid"))) || nested(configSecret, "metadata", "annotations", resetGrantUIDAnnotation) != nested(secret, "metadata", "uid") {
		return false
	}
	raw, err := secretText(secret, "grant.json")
	var value branchResetPlatformGrant
	if err != nil || json.Unmarshal([]byte(raw), &value) != nil || !owned(secret, value.Project, value.Endpoint) || nested(secret, "metadata", "labels", "neon-control/reset-operation") != value.Operation {
		return false
	}
	return validBranchResetPlatformGrant(value, configSecret, stored, request, time.Now())
}

var errBranchResetProbe = errors.New("isolated restored catalog inspection rejected")

// Issuing a grant requires the original live Worker lease and current Admin.
// A generated inspection Endpoint is deliberately absent from public metadata.
func branchResetProbeEndpoint(operation string) string {
	return "ep_" + catalogMarkerDigest(operation + "/private-reset-probe")[:16]
}
func (s *server) issueBranchResetPlatformGrant(ctx context.Context, operation, worker, endpoint string, observed catalogNativeSnapshot, roots []any, adminOID, probeOID uint32) (string, error) {
	if !branchResetOperationID.MatchString(operation) || endpoint != branchResetProbeEndpoint(operation) {
		return "", errBranchResetProbe
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return "", err
	}
	var project, branch, tenant, timeline string
	var eligible bool
	err = tx.QueryRow(ctx, `SELECT c.project_id,c.branch_id,c.tenant_id,c.target_timeline_id,
  pr.state='ready' AND pr.source='managed' AND pr.deleted_at IS NULL AND b.state='restoring'
  AND b.deleted_at IS NULL AND b.timeline_id=c.original_timeline_id AND org.state='active'
  AND EXISTS(SELECT 1 FROM users u JOIN organization_members m ON m.user_id=u.id AND m.org_id=org.id
    LEFT JOIN project_grants g ON g.user_id=u.id AND g.project_id=pr.id
    WHERE u.id=o.actor_id AND NOT u.disabled AND (m.role='admin' OR g.role='admin'))
  AND EXISTS(SELECT 1 FROM operation_steps step WHERE step.operation_id=o.id AND step.name='probe_restored_native_catalog' AND step.state='running')
  AND NOT EXISTS(SELECT 1 FROM endpoints e WHERE e.id=$2 OR e.branch_id=b.id AND e.deleted_at IS NULL AND e.state<>'suspended')
 FROM branch_reset_checkpoints c JOIN operations o ON o.id=c.operation_id AND o.action='reset_branch'
   AND o.project_id=c.project_id AND o.resource_id=c.branch_id
 JOIN branches b ON b.id=c.branch_id AND b.project_id=c.project_id
 JOIN projects pr ON pr.id=c.project_id JOIN organizations org ON org.id=pr.org_id
 WHERE c.operation_id=$1 FOR UPDATE OF c,o,b,pr`, operation, endpoint).Scan(&project, &branch, &tenant, &timeline, &eligible)
	if err != nil {
		return "", err
	}
	if !eligible || observed.Scope != (catalogNativeScope{tenant, timeline}) {
		return "", errBranchResetProbe
	}
	config, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(endpoint)+"-config"), nil)
	if err != nil {
		return "", err
	}
	if !owned(config, project, endpoint) || nested(config, "metadata", "labels", "neon-control/reset-operation") != operation || nested(config, "metadata", "annotations", resetInspectionAnnotation) != operation {
		return "", errBranchResetProbe
	}
	vm, err := s.kube.request(ctx, http.MethodGet, s.kube.path("vm", kubeName(endpoint)), nil)
	if err != nil || !owned(vm, project, endpoint) || nested(vm, "status", "phase") != "Running" {
		return "", errBranchResetProbe
	}
	raw, err := secretText(config, "config.json")
	var stored map[string]any
	if err != nil || json.Unmarshal([]byte(raw), &stored) != nil {
		return "", errBranchResetProbe
	}
	grant, request, err := makeBranchResetPlatformGrant(operation, branch, endpoint, stringVal(nested(config, "metadata", "uid")), stored, roots, observed, adminOID, probeOID, time.Now())
	if err != nil {
		return "", err
	}
	if !validBranchResetPlatformGrant(grant, config, stored, request, time.Now()) {
		return "", errBranchResetProbe
	}
	// Record the grant attempt before any effective Kubernetes authorization.
	// A failed audit must never leave a live grant attached to an inspection VM.
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome)
  SELECT actor_id,'authorize_isolated_reset_platform_credentials','branch',resource_id,request_id,'accepted'
  FROM operations WHERE id=$1`, operation)
	if err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	if err = s.checkBranchResetGrantAuthority(ctx, operation, worker, grant); err != nil {
		return "", err
	}
	grantRaw, _ := json.Marshal(grant)
	ref := "branch-reset-grant-" + catalogMarkerDigest(operation + "/" + worker + "/" + randomToken(24))[:32]
	labels := credentialLabels(project, endpoint)
	labels["neon-control/reset-operation"] = operation
	secret, err := s.kube.createOwned(ctx, "secret", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
		"metadata": map[string]any{"name": ref, "labels": labels}, "data": map[string]string{"grant.json": base64.StdEncoding.EncodeToString(grantRaw)}}, project, endpoint)
	if err != nil {
		originalErr := err
		var fault kubeError
		if errors.As(err, &fault) && fault.Status < 500 {
			return "", err
		}
		secret, err = s.kube.request(ctx, http.MethodGet, s.kube.path("secret", ref), nil)
		if err != nil {
			return "", originalErr
		}
	}
	uid := stringVal(nested(secret, "metadata", "uid"))
	content, contentErr := secretText(secret, "grant.json")
	if secret["immutable"] != true || !functionResourceUID.MatchString(uid) || contentErr != nil || content != string(grantRaw) {
		return "", errBranchResetProbe
	}
	bound := false
	for attempt := 0; attempt < 6; attempt++ {
		if err = s.checkBranchResetGrantAuthority(ctx, operation, worker, grant); err != nil {
			return "", err
		}
		current, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(endpoint)+"-config"), nil)
		if err != nil {
			return "", err
		}
		text, err := secretText(current, "config.json")
		var doc map[string]any
		if err != nil || json.Unmarshal([]byte(text), &doc) != nil || !validBranchResetPlatformGrant(grant, current, doc, request, time.Now()) {
			return "", errBranchResetProbe
		}
		annotations, ok := current["metadata"].(map[string]any)["annotations"].(map[string]any)
		if !ok {
			return "", errBranchResetProbe
		}
		annotations[resetGrantAnnotation], annotations[resetGrantUIDAnnotation] = ref, uid
		_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", kubeName(endpoint)+"-config"), current)
		var conflict kubeError
		if errors.As(err, &conflict) && conflict.Status == 409 {
			continue
		}
		if err != nil {
			originalErr := err
			observed, readErr := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(endpoint)+"-config"), nil)
			if readErr != nil || nested(observed, "metadata", "uid") != grant.ConfigUID || nested(observed, "metadata", "annotations", resetGrantAnnotation) != ref || nested(observed, "metadata", "annotations", resetGrantUIDAnnotation) != uid {
				return "", originalErr
			}
			text, e := secretText(observed, "config.json")
			var doc map[string]any
			if e != nil || json.Unmarshal([]byte(text), &doc) != nil || !validBranchResetPlatformGrant(grant, observed, doc, request, time.Now()) {
				return "", errBranchResetProbe
			}
		}
		bound = true
		break
	}
	if !bound {
		return "", errBranchResetProbe
	}
	return ref, nil
}

func (s *server) checkBranchResetGrantAuthority(ctx context.Context, operation, worker string, grant branchResetPlatformGrant) error {
	var allowed bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_reset_checkpoints c
 JOIN operations o ON o.id=c.operation_id AND o.project_id=c.project_id AND o.resource_id=c.branch_id AND o.action='reset_branch'
 JOIN branches b ON b.id=c.branch_id AND b.project_id=c.project_id
 JOIN projects pr ON pr.id=c.project_id JOIN organizations org ON org.id=pr.org_id
 JOIN users u ON u.id=o.actor_id AND NOT u.disabled
 JOIN organization_members m ON m.user_id=u.id AND m.org_id=org.id
 LEFT JOIN project_grants g ON g.user_id=u.id AND g.project_id=pr.id
 WHERE c.operation_id=$1 AND c.project_id=$3 AND c.branch_id=$4 AND c.tenant_id=$5 AND c.target_timeline_id=$6
 AND o.state='running' AND o.lease_owner=$2 AND o.lease_expires_at>clock_timestamp()
 AND pr.state='ready' AND pr.source='managed' AND pr.deleted_at IS NULL
 AND b.state='restoring' AND b.deleted_at IS NULL AND b.timeline_id=c.original_timeline_id
 AND org.state='active' AND (m.role='admin' OR g.role='admin')
 AND EXISTS(SELECT 1 FROM operation_steps step WHERE step.operation_id=o.id AND step.name='probe_restored_native_catalog' AND step.state='running')
 AND NOT EXISTS(SELECT 1 FROM endpoints e WHERE e.id=$7 OR e.branch_id=b.id AND e.deleted_at IS NULL AND e.state<>'suspended'))`,
		operation, worker, grant.Project, grant.Branch, grant.Scope.TenantID, grant.Scope.TimelineID, grant.Endpoint).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return errBranchResetProbe
	}
	return nil
}
