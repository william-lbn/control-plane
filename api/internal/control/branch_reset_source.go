package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Private source data stays outside any durable/public Operation payload. A
// data-only historical branch can reuse its frozen original ancestry witness;
// it must never borrow its parent's current native timeline or catalog intent.
type branchResetSourceRoot struct {
	StorageBranch  string
	StorageScope   catalogNativeScope
	WitnessBranch  string
	WitnessScope   catalogNativeScope
	Bootstrap      computeCatalogBootstrap
	ConfigData     map[string]string `json:"-"`
	CredentialData map[string]string `json:"-"`
}

var errBranchResetSource = errors.New("retained original restore source bootstrap unavailable")

func (k *kubeClient) originalBranchResetSourceFiles(ctx context.Context, q catalogForkReader, proof computeCatalogBootstrap) (map[string]string, map[string]string, error) {
	config, _, live, liveErr := k.readComputeCatalogBootstrap(ctx, proof.payload())
	if liveErr == nil && live.ConfigUID == proof.ConfigUID && live.CredentialUID == proof.CredentialUID {
		credential, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(proof.Endpoint)+"-credentials"), nil)
		if err != nil {
			return nil, nil, err
		}
		// A second read cannot adopt a replacement during source observation.
		if !owned(credential, proof.Project, proof.Endpoint) || nested(credential, "metadata", "uid") != proof.CredentialUID {
			return nil, nil, errBranchResetSource
		}
		configData, err := resetSecretData(config)
		if err != nil {
			return nil, nil, err
		}
		credentialData, err := resetSecretData(credential)
		if err != nil {
			return nil, nil, err
		}
		// Use the same complete-file validation as the retained rollback witness.
		target := catalogNativeScope{proof.Tenant, catalogMarkerDigest(proof.Operation + "/source-validation")[:32]}
		if target.TimelineID == proof.Timeline {
			target.TimelineID = catalogMarkerDigest(proof.Operation + "/other-validation")[:32]
		}
		if _, err = prepareBranchResetBacking("op_"+catalogMarkerDigest(proof.Operation + "/validate")[:24], proof, target, config, credential); err != nil {
			return nil, nil, err
		}
		return configData, credentialData, nil
	}
	// The parent may have moved to another reset epoch since the original fork.
	// Its old sealed private files are retained by the immutable rollback record.
	rows, err := q.Query(ctx, `SELECT c.operation_id,k.rollback_secret_ref,k.rollback_secret_uid,k.rollback_sha256
 FROM branch_reset_checkpoints c JOIN branch_reset_endpoint_backings k USING(operation_id)
 WHERE c.project_id=$1 AND c.branch_id=$2 AND c.tenant_id=$3 AND c.original_timeline_id=$4
 AND k.endpoint_id=$5 AND k.original_config_uid=$6 AND k.original_credential_uid=$7
 ORDER BY c.created_at LIMIT 2`, proof.Project, proof.Branch, proof.Tenant, proof.Timeline, proof.Endpoint, proof.ConfigUID, proof.CredentialUID)
	if err != nil {
		return nil, nil, err
	}
	type original struct{ Operation, Ref, UID, SHA string }
	snapshots := []original{}
	for rows.Next() {
		var v original
		if err = rows.Scan(&v.Operation, &v.Ref, &v.UID, &v.SHA); err != nil {
			rows.Close()
			return nil, nil, err
		}
		snapshots = append(snapshots, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(snapshots) == 0 {
		return nil, nil, errBranchResetSource
	}
	var configData, credentialData map[string]string
	digest := ""
	for _, v := range snapshots {
		original, err := k.readBranchResetBacking(ctx, v.Operation, proof.Project, proof.Endpoint, v.Ref, v.UID, v.SHA)
		if err != nil || original.Original != proof {
			return nil, nil, errBranchResetSource
		}
		raw, _ := json.Marshal([]any{original.ConfigData, original.CredentialData})
		next := catalogMarkerDigest(string(raw))
		if digest != "" && digest != next {
			return nil, nil, errBranchResetSource
		}
		digest, configData, credentialData = next, original.ConfigData, original.CredentialData
	}
	return configData, credentialData, nil
}

func (k *kubeClient) resolveBranchResetSourceRoot(ctx context.Context, q catalogForkReader, project, branch string, scope catalogNativeScope) (branchResetSourceRoot, error) {
	result := branchResetSourceRoot{StorageBranch: branch, StorageScope: scope, WitnessBranch: branch, WitnessScope: scope}
	if project == "" || !dataBranchID.MatchString(branch) || !scope.valid() {
		return result, errBranchResetSource
	}
	seen := map[string]bool{}
	for depth := 0; depth < 64; depth++ {
		key := result.WitnessBranch + "/" + result.WitnessScope.TimelineID
		if seen[key] {
			return result, errBranchResetSource
		}
		seen[key] = true
		var raw []byte
		err := q.QueryRow(ctx, `SELECT proof FROM (
   SELECT r.proof,e.deleted_at,e.created_at FROM compute_catalog_bootstrap_receipts r
    JOIN compute_catalog_bootstrap_publications pub ON pub.endpoint_id=r.endpoint_id
    JOIN endpoints e ON e.id=r.endpoint_id
    WHERE e.project_id=$1 AND e.branch_id=$2 AND r.proof->>'project_id'=$1
     AND r.proof->>'branch_id'=$2 AND r.proof->>'tenant_id'=$3 AND r.proof->>'timeline_id'=$4
     AND r.proof->>'endpoint_type'='read_write'
   UNION ALL
   SELECT r.proof,e.deleted_at,e.created_at FROM compute_catalog_reset_receipts r
    JOIN compute_catalog_reset_publications pub USING(operation_id,endpoint_id)
    JOIN endpoints e ON e.id=r.endpoint_id
    WHERE e.project_id=$1 AND e.branch_id=$2 AND r.proof->>'project_id'=$1
     AND r.proof->>'branch_id'=$2 AND r.proof->>'tenant_id'=$3 AND r.proof->>'timeline_id'=$4
     AND r.proof->>'endpoint_type'='read_write'
  ) published ORDER BY deleted_at NULLS FIRST,created_at DESC LIMIT 1`, project, result.WitnessBranch, result.WitnessScope.TenantID, result.WitnessScope.TimelineID).Scan(&raw)
		if err == nil {
			var proof computeCatalogBootstrap
			if json.Unmarshal(raw, &proof) != nil || proof.Project != project || proof.Branch != result.WitnessBranch || proof.Tenant != result.WitnessScope.TenantID || proof.Timeline != result.WitnessScope.TimelineID ||
				proof.EndpointType != "read_write" || !functionWriterEndpointID.MatchString(proof.Endpoint) || !functionResourceUID.MatchString(proof.ConfigUID) || !functionResourceUID.MatchString(proof.CredentialUID) ||
				proof.Witness != "original_sql_probe" && proof.Witness != "legacy_successful_probe_initial_backing_observation" && proof.Witness != branchResetBootstrapWitness {
				return result, errBranchResetSource
			}
			result.Bootstrap = proof
			result.ConfigData, result.CredentialData, err = k.originalBranchResetSourceFiles(ctx, q, proof)
			return result, err
		}
		if !isNoRows(err) {
			return result, err
		}
		err = q.QueryRow(ctx, `SELECT source.proof FROM catalog_inheritance_sources source JOIN operations o ON o.id=source.operation_id
   WHERE source.project_id=$1 AND source.branch_id=$2 AND o.action='create_branch'
    AND source.proof->>'endpoint'='' ORDER BY source.created_at LIMIT 1`, project, result.WitnessBranch).Scan(&raw)
		if err != nil {
			return result, errBranchResetSource
		}
		var original catalogInheritanceSource
		if json.Unmarshal(raw, &original) != nil || original.Project != project || original.Branch != result.WitnessBranch || original.Scope != result.WitnessScope || original.Endpoint != "" ||
			!dataBranchID.MatchString(original.SourceBranch) || original.SourceBranch == original.Branch || !original.SourceScope.valid() || original.SourceScope.TenantID != scope.TenantID || original.ParentLSN == "" {
			return result, errBranchResetSource
		}
		if _, err = canonicalLSN(original.ParentLSN); err != nil {
			return result, errBranchResetSource
		}
		result.WitnessBranch, result.WitnessScope = original.SourceBranch, original.SourceScope
	}
	return result, errBranchResetSource
}
