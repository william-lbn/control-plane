package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

// These are backing identities, not a claim that a legacy SQL object has never
// drifted. Once bootstrap succeeds, PostgreSQL/WAL owns its catalog; explicit
// catalog Operations perform their own SQL before publishing cold configuration.
type computeCatalogBootstrap struct {
	Project       string `json:"project_id"`
	Branch        string `json:"branch_id"`
	Endpoint      string `json:"endpoint_id"`
	EndpointType  string `json:"endpoint_type"`
	Tenant        string `json:"tenant_id"`
	Timeline      string `json:"timeline_id"`
	ConfigUID     string `json:"config_uid"`
	CredentialUID string `json:"credential_uid"`
	Operation     string `json:"operation_id"`
	Witness       string `json:"witness"`
}

func (v computeCatalogBootstrap) payload() createPayload {
	return createPayload{ProjectID: v.Project, BranchID: v.Branch, EndpointID: v.Endpoint, EndpointType: v.EndpointType, TenantID: v.Tenant, TimelineID: v.Timeline}
}
func bootstrapEndpointType(value string) string {
	if value == "" {
		return "read_write"
	}
	return value
}
func sameBootstrapScope(a, b createPayload) bool {
	return a.ProjectID == b.ProjectID && a.BranchID == b.BranchID && a.EndpointID == b.EndpointID && a.TenantID == b.TenantID && a.TimelineID == b.TimelineID && bootstrapEndpointType(a.EndpointType) == bootstrapEndpointType(b.EndpointType)
}

func computeCatalogBootstrapStates(action, project, branch, endpoint string, legacy bool) bool {
	if legacy {
		return project == "ready" && branch == "ready" && (endpoint == "active" || endpoint == "suspended")
	}
	endpointOK := endpoint == "provisioning" || endpoint == "error" || endpoint == "active" || endpoint == "suspended"
	branchOK := branch == "creating" || branch == "error" || branch == "ready"
	switch action {
	case "create_project":
		return (project == "provisioning" || project == "error" || project == "ready") && branchOK && endpointOK
	case "create_branch":
		return project == "ready" && branchOK && endpointOK
	case "create_endpoint":
		return project == "ready" && branch == "ready" && endpointOK
	}
	return false
}

func (k *kubeClient) readComputeCatalogBootstrap(ctx context.Context, p createPayload) (map[string]any, map[string]any, computeCatalogBootstrap, error) {
	var v computeCatalogBootstrap
	config, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(p.EndpointID)+"-config"), nil)
	if err != nil || !owned(config, p.ProjectID, p.EndpointID) {
		return nil, nil, v, errors.New("bootstrap config unavailable or unowned")
	}
	credential, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(p.EndpointID)+"-credentials"), nil)
	if err != nil || !owned(credential, p.ProjectID, p.EndpointID) {
		return nil, nil, v, errors.New("bootstrap credential unavailable or unowned")
	}
	raw, err := secretText(config, "config.json")
	if err != nil {
		return nil, nil, v, err
	}
	document := map[string]any{}
	if json.Unmarshal([]byte(raw), &document) != nil {
		return nil, nil, v, errors.New("bootstrap config invalid")
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return nil, nil, v, errors.New("bootstrap spec missing")
	}
	cluster, ok := spec["cluster"].(map[string]any)
	if !ok || cluster["cluster_id"] != p.ProjectID {
		return nil, nil, v, errors.New("bootstrap project mismatch")
	}
	mode := stringVal(spec["mode"])
	if (bootstrapEndpointType(p.EndpointType) == "read_only" && mode != "Replica") || (bootstrapEndpointType(p.EndpointType) == "read_write" && mode != "" && mode != "Primary") {
		return nil, nil, v, errors.New("bootstrap compute mode mismatch")
	}
	if spec["local_proxy_config"] != nil || (spec["drop_subscriptions_before_start"] != nil && spec["drop_subscriptions_before_start"] != false) {
		return nil, nil, v, errors.New("bootstrap requires explicit migration of optional startup services")
	}
	if flag, present := spec["skip_pg_catalog_updates"]; present && flag != true && flag != false {
		return nil, nil, v, errors.New("bootstrap catalog flag invalid")
	}
	settings, ok := cluster["settings"].([]any)
	if !ok {
		return nil, nil, v, errors.New("bootstrap settings missing")
	}
	identities := map[string]string{}
	for _, entry := range settings {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, nil, v, errors.New("bootstrap setting invalid")
		}
		name := stringVal(item["name"])
		if name == "neon.tenant_id" || name == "neon.timeline_id" {
			if _, exists := identities[name]; exists {
				return nil, nil, v, errors.New("bootstrap native scope ambiguous")
			}
			identities[name] = stringVal(item["value"])
		}
	}
	if identities["neon.tenant_id"] != p.TenantID || identities["neon.timeline_id"] != p.TimelineID {
		return nil, nil, v, errors.New("bootstrap native scope changed")
	}
	roles, ok := cluster["roles"].([]any)
	if !ok {
		return nil, nil, v, errors.New("bootstrap role intents missing")
	}
	for _, name := range []string{"cloud_admin", "control_probe"} {
		key := "adminVerifier"
		if name == "control_probe" {
			key = "probeVerifier"
		}
		matches := 0
		actual := ""
		for _, entry := range roles {
			item, ok := entry.(map[string]any)
			if !ok {
				return nil, nil, v, errors.New("bootstrap role invalid")
			}
			if item["name"] == name {
				actual = stringVal(item["encrypted_password"])
				matches++
			}
		}
		if matches != 1 {
			return nil, nil, v, errors.New("bootstrap system role intent ambiguous")
		}
		expected, err := secretText(credential, key)
		if err != nil && name == "control_probe" && stringVal(nested(credential, "data", key)) == "" {
			// Older successful creates retained the probe verifier only in the
			// config. Prove it against the protected original password in memory;
			// do not invent a verifier or modify the credential Secret.
			password, passwordErr := secretText(credential, "probePassword")
			if passwordErr == nil && functionsql.ValidVerifier(actual) && scramMatches(password, actual) {
				expected, err = actual, nil
			}
		}
		if err != nil || expected == "" || actual != expected {
			return nil, nil, v, errors.New("bootstrap verifier intent mismatch or unavailable")
		}
	}
	password, err := secretText(credential, "probePassword")
	if err != nil || password == "" {
		return nil, nil, v, errors.New("bootstrap probe unavailable")
	}
	stored, err := secretText(config, "probePassword")
	if err != nil || stored != password {
		return nil, nil, v, errors.New("bootstrap probe intent mismatch")
	}
	v = computeCatalogBootstrap{Project: p.ProjectID, Branch: p.BranchID, Endpoint: p.EndpointID, EndpointType: bootstrapEndpointType(p.EndpointType), Tenant: p.TenantID, Timeline: p.TimelineID, ConfigUID: stringVal(nested(config, "metadata", "uid")), CredentialUID: stringVal(nested(credential, "metadata", "uid"))}
	if v.ConfigUID == "" || v.CredentialUID == "" || stringVal(nested(config, "metadata", "resourceVersion")) == "" {
		return nil, nil, v, errors.New("bootstrap backing identity missing")
	}
	return config, document, v, nil
}

// Lock and validate the original create witness. A failed create or a ready
// metadata row alone is insufficient to opt an old Endpoint into this policy.
func (s *server) storeComputeCatalogBootstrap(ctx context.Context, operation, worker string, p createPayload, legacy bool, v computeCatalogBootstrap) (computeCatalogBootstrap, error) {
	if !sameBootstrapScope(v.payload(), p) || v.ConfigUID == "" || v.CredentialUID == "" {
		return v, errors.New("bootstrap observed backing scope rejected")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return v, err
	}
	defer tx.Rollback(ctx)
	var current createPayload
	var eligible bool
	var projectState, branchState, endpointState string
	err = tx.QueryRow(ctx, `SELECT pr.id,b.id,e.id,e.endpoint_type,pr.tenant_id,b.timeline_id,
 pr.source='managed' AND pr.deleted_at IS NULL AND b.deleted_at IS NULL AND e.deleted_at IS NULL AND e.workload_kind='neonvm',pr.state,b.state,e.state
 FROM projects pr JOIN branches b ON b.project_id=pr.id JOIN endpoints e ON e.branch_id=b.id AND e.project_id=pr.id
 WHERE pr.id=$1 AND b.id=$2 AND e.id=$3 FOR UPDATE OF pr,b,e`, p.ProjectID, p.BranchID, p.EndpointID).Scan(&current.ProjectID, &current.BranchID, &current.EndpointID, &current.EndpointType, &current.TenantID, &current.TimelineID, &eligible, &projectState, &branchState, &endpointState)
	if err != nil {
		return v, err
	}
	if !eligible || !sameBootstrapScope(current, p) {
		return v, errors.New("bootstrap parent or native identity changed")
	}
	var raw []byte
	var state, action, resource string
	var verified bool
	err = tx.QueryRow(ctx, `SELECT payload,state,action,resource_id,EXISTS(SELECT 1 FROM operation_steps WHERE operation_id=o.id AND name='sql_probe_via_proxy' AND state='succeeded') FROM operations o WHERE id=$1 AND project_id=$2 AND action IN ('create_project','create_branch','create_endpoint') FOR UPDATE`, operation, p.ProjectID).Scan(&raw, &state, &action, &resource, &verified)
	var original createPayload
	if err != nil || json.Unmarshal(raw, &original) != nil || !sameBootstrapScope(original, p) {
		return v, errors.New("bootstrap original create scope rejected")
	}
	expectedResource := map[string]string{"create_project": p.ProjectID, "create_branch": p.BranchID, "create_endpoint": p.EndpointID}[action]
	if resource != expectedResource || !computeCatalogBootstrapStates(action, projectState, branchState, endpointState, legacy) {
		return v, errors.New("bootstrap original creation state or resource changed")
	}
	if legacy {
		if state != "succeeded" || !verified {
			return v, errors.New("bootstrap requires original successful SQL witness")
		}
	} else {
		if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
			return v, err
		}
		var probing bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operation_steps WHERE operation_id=$1 AND name='sql_probe_via_proxy' AND state='running')`, operation).Scan(&probing); err != nil {
			return v, err
		}
		if !probing {
			return v, errors.New("bootstrap original SQL probe stage required")
		}
	}
	var competing bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE project_id=$1 AND id<>$3 AND state IN ('queued','running','retry_wait') AND (resource_id=$2 OR payload->>'branch_id'=$2))`, p.ProjectID, p.BranchID, operation).Scan(&competing)
	if err != nil {
		return v, err
	}
	if competing {
		return v, errors.New("bootstrap waits for active branch operation")
	}
	v.Operation = operation
	v.Witness = "original_sql_probe"
	if legacy {
		v.Witness = "legacy_successful_probe_initial_backing_observation"
	}
	raw, _ = json.Marshal(v)
	if _, err = tx.Exec(ctx, `INSERT INTO compute_catalog_bootstrap_receipts(endpoint_id,operation_id,proof) VALUES($1,$2,$3::jsonb) ON CONFLICT DO NOTHING`, p.EndpointID, operation, raw); err != nil {
		return v, err
	}
	var stored computeCatalogBootstrap
	if err = tx.QueryRow(ctx, `SELECT proof FROM compute_catalog_bootstrap_receipts WHERE endpoint_id=$1`, p.EndpointID).Scan(&raw); err != nil {
		return v, err
	}
	if json.Unmarshal(raw, &stored) != nil {
		return v, errors.New("bootstrap stored witness invalid")
	}
	v.Witness = stored.Witness
	if stored != v {
		return v, errors.New("bootstrap original backing witness changed")
	}
	return stored, tx.Commit(ctx)
}

func (s *server) publishComputeCatalogBootstrap(ctx context.Context, v computeCatalogBootstrap) error {
	for attempt := 0; attempt < 6; attempt++ {
		secret, document, current, err := s.kube.readComputeCatalogBootstrap(ctx, v.payload())
		if err != nil {
			return err
		}
		if current.ConfigUID != v.ConfigUID || current.CredentialUID != v.CredentialUID {
			return errors.New("bootstrap fixed backing UID changed")
		}
		spec := document["spec"].(map[string]any)
		if spec["skip_pg_catalog_updates"] == true {
			_, err = s.db.Exec(ctx, `INSERT INTO compute_catalog_bootstrap_publications(endpoint_id) VALUES($1) ON CONFLICT DO NOTHING`, v.Endpoint)
			return err
		}
		spec["skip_pg_catalog_updates"] = true
		raw, _ := json.Marshal(document)
		secret["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(raw)
		_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", kubeName(v.Endpoint)+"-config"), secret)
		// Re-observe both conflict and unknown transport outcomes using the original
		// UID. A successor is never adopted, and unrelated config fields are retained.
		if err != nil && !kubeStatusIs(err, http.StatusConflict) {
			return err
		}
	}
	return errors.New("bootstrap publication requires original fixed UID readback")
}

func (s *server) sealComputeCatalogBootstrap(ctx context.Context, operation, worker string, p createPayload, legacy bool) error {
	_, _, v, err := s.kube.readComputeCatalogBootstrap(ctx, p)
	if err != nil {
		return err
	}
	v, err = s.storeComputeCatalogBootstrap(ctx, operation, worker, p, legacy, v)
	if err != nil {
		return err
	}
	return s.publishComputeCatalogBootstrap(ctx, v)
}

// A retried create must reuse its first successful bootstrap receipt before it
// can recreate a VM. It cannot silently fall back to replaying catalog DDL.
func (s *server) resumeComputeCatalogBootstrap(ctx context.Context, p createPayload) error {
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT proof FROM compute_catalog_bootstrap_receipts WHERE endpoint_id=$1`, p.EndpointID).Scan(&raw)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var v computeCatalogBootstrap
	if json.Unmarshal(raw, &v) != nil || !sameBootstrapScope(v.payload(), p) {
		return errors.New("bootstrap resume scope changed")
	}
	return s.publishComputeCatalogBootstrap(ctx, v)
}

func (s *server) reconcileLegacyComputeCatalogBootstraps(ctx context.Context) error {
	rows, err := s.db.Query(ctx, `SELECT e.id, o.id,o.payload FROM endpoints e JOIN projects pr ON pr.id=e.project_id JOIN branches b ON b.id=e.branch_id
 JOIN operations o ON o.project_id=pr.id AND o.payload->>'endpoint_id'=e.id AND o.action IN ('create_project','create_branch','create_endpoint') AND o.state='succeeded'
 WHERE pr.source='managed' AND pr.state='ready' AND pr.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL
 AND e.deleted_at IS NULL AND e.state IN ('active','suspended') AND e.workload_kind='neonvm'
 AND NOT EXISTS(SELECT 1 FROM compute_catalog_bootstrap_publications pub WHERE pub.endpoint_id=e.id)
 AND (SELECT count(*) FROM operations candidate WHERE candidate.project_id=pr.id AND candidate.payload->>'endpoint_id'=e.id AND candidate.action IN ('create_project','create_branch','create_endpoint') AND candidate.state='succeeded')=1
 ORDER BY e.id LIMIT 50`)
	if err != nil {
		return err
	}
	type candidate struct {
		endpoint, operation string
		payload             []byte
	}
	items := []candidate{}
	for rows.Next() {
		var v candidate
		if err = rows.Scan(&v.endpoint, &v.operation, &v.payload); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range items {
		var p createPayload
		if json.Unmarshal(v.payload, &p) != nil || p.EndpointID != v.endpoint {
			return errors.New("legacy bootstrap original payload invalid")
		}
		if err = s.sealComputeCatalogBootstrap(ctx, v.operation, "", p, true); err != nil {
			if s.logger != nil {
				s.logger.Warn("legacy catalog bootstrap observation deferred", "endpoint_id", v.endpoint)
			}
			continue
		}
	}
	return nil
}
func (s *server) runComputeCatalogBootstrapController(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		attempt, cancel := context.WithTimeout(ctx, 25*time.Second)
		err := s.reconcileLegacyComputeCatalogBootstraps(attempt)
		cancel()
		if err != nil && s.logger != nil {
			s.logger.Warn("catalog bootstrap observation deferred")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
