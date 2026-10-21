package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// A published inherited legacy role may retain its parent's immutable Secret.
// This proves that exact backing and observed OID, not an earlier native history.
func verifyCatalogBaselineInheritance(project, branch string, input catalogBaselineInput, intent catalogBaselineIntent, uid string, p createPayload, source catalogInheritanceSource, observation catalogInheritanceObservation) error {
	if p.CatalogPolicy != catalogPreserveInheritedSQL || p.ProjectID != project || p.BranchID != branch || p.EndpointID == "" || p.EndpointType != "" && p.EndpointType != "read_write" || source.Project != project || source.Branch != branch || source.SourceBranch == branch || !source.Scope.valid() || source.Scope != intent.Scope || source.Scope != (catalogNativeScope{p.TenantID, p.TimelineID}) || !source.SourceScope.valid() || source.SourceScope.TenantID != source.Scope.TenantID || source.Endpoint != p.EndpointID || source.PayloadSHA256 != catalogInheritancePayloadDigest(p) || source.Operation == "" || source.Operation != observation.Operation || observation.Project != project || observation.Branch != branch || observation.Endpoint != p.EndpointID || observation.Scope != source.Scope || observation.SourceSHA256 != catalogInheritanceSourceDigest(source) || intent.State != "ready" || intent.Version != 1 || input.Kind != "role" || !functionResourceUID.MatchString(uid) || len(source.Items) != len(observation.Objects) {
		return renameFault(409, "catalog_inheritance_observation_required", "Original published inheritance scope and unchanged legacy role intent required")
	}
	matches := 0
	for i, item := range source.Items {
		if item.Kind != "role" || item.Name != input.Name {
			continue
		}
		object := observation.Objects[i]
		if item.External || item.Binding != nil || item.CredentialRef != intent.Reference || item.CredentialUID != uid || object.Kind != item.Kind || object.Name != item.Name || object.Status != "legacy_unattested" || object.OID == 0 || object.OID != input.OID {
			return renameFault(409, "catalog_inheritance_identity_changed", "Original inherited role OID or immutable credential UID changed")
		}
		matches++
	}
	if matches != 1 {
		return renameFault(409, "catalog_inheritance_observation_required", "Exactly one original inherited role observation required")
	}
	return nil
}

func (s *server) catalogBaselineCredentialUID(ctx context.Context, project, branch, endpoint string, input catalogBaselineInput, intent catalogBaselineIntent) (string, error) {
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", intent.Reference), nil)
	if err != nil || secret["immutable"] != true || stringVal(nested(secret, "metadata", "labels", "neon-control/project-id")) != project {
		return "", errors.New("original immutable catalog credential unavailable")
	}
	uid := stringVal(nested(secret, "metadata", "uid"))
	if !functionResourceUID.MatchString(uid) {
		return "", errors.New("original immutable catalog credential UID unavailable")
	}
	// Check known lineage even when a replacement Secret copied endpoint labels.
	// A later same-branch Compute cannot replace the first physical fork witness.
	var operation, action string
	var rawPayload, rawSource, rawObservation []byte
	var sqlSucceeded bool
	err = s.db.QueryRow(ctx, `SELECT o.id,o.action,o.payload,s.proof,v.proof,EXISTS(SELECT 1 FROM operation_steps st WHERE st.operation_id=o.id AND st.name='sql_probe_via_proxy' AND st.state='succeeded')
 FROM catalog_inheritance_sources s JOIN operations o ON o.id=s.operation_id AND o.project_id=s.project_id
 JOIN catalog_inheritance_observations v ON v.operation_id=s.operation_id AND v.project_id=s.project_id AND v.branch_id=s.branch_id
 JOIN catalog_inheritance_publications pub ON pub.endpoint_id=v.endpoint_id
 WHERE s.project_id=$1 AND s.branch_id=$2 AND s.proof->>'source_branch'<>s.branch_id
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(s.proof->'items') item WHERE item->>'kind'='role' AND item->>'name'=$3)
 ORDER BY s.created_at,s.operation_id LIMIT 1`, project, branch, input.Name).Scan(&operation, &action, &rawPayload, &rawSource, &rawObservation, &sqlSucceeded)
	if isNoRows(err) {
		if !owned(secret, project, endpoint) {
			return "", renameFault(409, "catalog_inheritance_observation_required", "The original credential belongs to another Compute; a published inheritance witness is required")
		}
		return uid, nil
	}
	if err != nil {
		return "", err
	}
	var p createPayload
	var source catalogInheritanceSource
	var observation catalogInheritanceObservation
	if !sqlSucceeded || action != "create_branch" && action != "create_endpoint" || json.Unmarshal(rawPayload, &p) != nil || json.Unmarshal(rawSource, &source) != nil || json.Unmarshal(rawObservation, &observation) != nil || source.Operation != operation {
		return "", errors.New("original inherited catalog observation unavailable")
	}
	if err = verifyCatalogBaselineInheritance(project, branch, input, intent, uid, p, source, observation); err != nil {
		return "", err
	}
	return uid, nil
}
