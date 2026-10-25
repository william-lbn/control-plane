package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

// This opt-in only changes the name of an original managed database. Consumers
// must already be disabled/suspended and all Function guests retired. The
// original immutable credential bytes and native service generations survive.
type catalogBackendRenamePlan struct {
	MetadataSHA256 string                           `json:"metadata_sha256"`
	Items          []catalogBackendRenameDependency `json:"items"`
}
type catalogBackendRenameDependency struct {
	Kind              string                      `json:"kind"`
	ID                string                      `json:"id"`
	Endpoint          string                      `json:"endpoint"`
	Generation        int64                       `json:"generation"`
	State             string                      `json:"state"`
	Version           int64                       `json:"version,omitempty"`
	RuntimeGeneration int64                       `json:"runtime_generation,omitempty"`
	Spec              json.RawMessage             `json:"spec,omitempty"`
	SecretRef         string                      `json:"secret_ref,omitempty"`
	SecretUID         string                      `json:"secret_uid,omitempty"`
	WorkloadUID       string                      `json:"workload_uid,omitempty"`
	SQL               *catalogBackendSQLReference `json:"sql,omitempty"`
}

// Explicit serialization keeps the old signed scope intact. No credential
// value is ever included in an Operation or a metadata receipt.
type catalogBackendSQLReference struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	MAC        string `json:"mac"`
	KeyVersion string `json:"key_version"`
	Schema     string `json:"schema"`
}

func (v catalogBackendRenameDependency) sqlReference(p catalogRenamePayload) functionSQLReference {
	r := functionSQLReference{Generation: v.Generation, WriterID: v.Endpoint, Scope: functionsql.Scope{FunctionID: v.ID, ProjectID: p.ProjectID, BranchID: p.BranchID, Database: p.Name, ProvisioningRole: "control_probe"}}
	if v.SQL != nil {
		r.Namespace = v.SQL.Namespace
		r.Name = v.SQL.Name
		r.UID = v.SQL.UID
		r.MAC = v.SQL.MAC
		r.MACKeyVersion = v.SQL.KeyVersion
		r.Scope.Schema = v.SQL.Schema
	}
	return r
}
func readCatalogBackendRenamePlan(ctx context.Context, db catalogRenameReader, p catalogRenamePayload) (*catalogBackendRenamePlan, error) {
	var raw []byte
	err := db.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(v ORDER BY v->>'kind',v->>'id'),'[]'::jsonb) FROM (
 SELECT jsonb_build_object('kind','auth','id',a.branch_id,'endpoint',a.endpoint_id,'generation',a.generation,'state',a.state,'spec',a.spec,'secret_ref',a.secret_ref) v FROM managed_auth_instances a WHERE a.project_id=$1 AND a.branch_id=$2 AND a.spec->>'database'=$3
 UNION ALL SELECT jsonb_build_object('kind','data_api','id',a.branch_id,'endpoint',a.endpoint_id,'generation',a.generation,'state',a.state,'spec',a.spec,'secret_ref',a.secret_ref) FROM data_api_instances a WHERE a.project_id=$1 AND a.branch_id=$2 AND a.spec->>'database'=$3
 UNION ALL SELECT jsonb_build_object('kind','object_storage','id',a.branch_id,'endpoint',a.endpoint_id,'generation',a.generation,'state',a.state,'spec',a.spec) FROM object_storage_instances a WHERE a.project_id=$1 AND a.branch_id=$2 AND a.spec->>'database'=$3
 UNION ALL SELECT jsonb_build_object('kind','function','id',f.id,'endpoint',f.endpoint_id,'generation',f.sql_credential_generation,'state',f.state,'version',f.version,'runtime_generation',f.generation,'sql',CASE WHEN c.function_id IS NULL THEN NULL ELSE jsonb_build_object('namespace',c.namespace,'name',c.secret_name,'uid',c.secret_uid,'mac',c.content_mac,'key_version',c.mac_key_version,'schema',c.sql_schema) END) FROM function_definitions f LEFT JOIN function_sql_credentials c ON c.function_id=f.id AND c.credential_generation=f.sql_credential_generation WHERE f.project_id=$1 AND f.branch_id=$2 AND f.database_name=$3 AND f.state<>'deleted'
 ) dependencies`, p.ProjectID, p.BranchID, p.Name).Scan(&raw)
	if err != nil {
		return nil, err
	}
	plan := &catalogBackendRenamePlan{MetadataSHA256: catalogMarkerDigest(string(raw))}
	if json.Unmarshal(raw, &plan.Items) != nil || len(plan.Items) > 32 {
		return nil, errors.New("bounded Backend dependency inventory required")
	}
	for _, v := range plan.Items {
		if v.Generation < 1 || !functionWriterEndpointID.MatchString(v.Endpoint) || (v.Kind == "function" && (v.State != "suspended" || v.SQL == nil || !v.sqlReference(p).valid())) || (v.Kind != "function" && v.State != "disabled") {
			return nil, renameFault(409, "catalog_backend_quiescence_required", "Disable Auth, Data API and Storage, and suspend initialized Functions before migrating their original database name")
		}
	}
	var active bool
	if err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM function_instances i JOIN function_definitions f ON f.id=i.function_id WHERE f.project_id=$1 AND f.branch_id=$2 AND f.database_name=$3 AND i.state<>'retired')`, p.ProjectID, p.BranchID, p.Name).Scan(&active); err != nil {
		return nil, err
	}
	if active {
		return nil, renameFault(409, "catalog_backend_quiescence_required", "Retire every original Function guest before database name migration")
	}
	return plan, nil
}
func checkCatalogBackendRenamePlan(ctx context.Context, db catalogRenameReader, p catalogRenamePayload) error {
	if p.Backend == nil || p.Kind != "database" || !budgetEvidence.MatchString(p.Backend.MetadataSHA256) {
		return errors.New("explicit original Backend dependency inventory required")
	}
	actual, err := readCatalogBackendRenamePlan(ctx, db, p)
	if err != nil {
		return err
	}
	if actual.MetadataSHA256 != p.Backend.MetadataSHA256 || len(actual.Items) != len(p.Backend.Items) {
		return renameFault(412, "catalog_backend_dependencies_changed", "Refresh original Backend dependencies before migration")
	}
	for i, v := range actual.Items {
		original := p.Backend.Items[i]
		original.SecretUID = ""
		original.WorkloadUID = ""
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(original)
		if string(a) != string(b) {
			return errors.New("original Backend dependency snapshot rejected")
		}
	}
	return nil
}
func backendRenameSecretName(p catalogRenamePayload, v catalogBackendRenameDependency) string {
	// The same proposal always resolves to the same new immutable backing.
	digest := catalogMarkerDigest(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s\x00%s\x00%s", p.ProjectID, p.BranchID, p.ObjectOID, p.ExpectedVersion, p.Name, p.NewName, v.SecretUID))
	return "catalog-" + strings.ReplaceAll(v.Kind, "_", "-") + "-" + digest[:32]
}
func backendRenameWorkload(v catalogBackendRenameDependency, branch string) string {
	if v.Kind == "auth" {
		return managedAuthName(branch)
	}
	if v.Kind == "data_api" {
		return dataAPIName(branch)
	}
	return ""
}
func (s *server) verifyBackendRenameWorkload(ctx context.Context, p catalogRenamePayload, v *catalogBackendRenameDependency, capture bool) error {
	name := backendRenameWorkload(*v, p.BranchID)
	if name == "" {
		return nil
	}
	item, err := s.kube.request(ctx, http.MethodGet, s.kube.path("deployment", name), nil)
	absent := kubeStatusIs(err, 404) && (capture || v.WorkloadUID == "")
	if err != nil && !absent {
		return err
	}
	uid := stringVal(nested(item, "metadata", "uid"))
	if !absent && (!owned(item, p.ProjectID, v.Endpoint) || !functionResourceUID.MatchString(uid) || (!capture && uid != v.WorkloadUID) || number(nested(item, "spec", "replicas")) != 0 || number(nested(item, "status", "replicas")) != 0 || number(nested(item, "status", "observedGeneration")) < number(nested(item, "metadata", "generation"))) {
		return errors.New("original dormant Backend workload identity rejected")
	}
	if capture {
		// Split API binds the original Deployment and metadata intent only. Pod
		// listing belongs to the independently authorized Worker, which must
		// establish complete absence before preparing secrets or native DDL.
		if !absent {
			v.WorkloadUID = uid
		}
		return nil
	}
	// A replacement/terminating Pod may outlive replicas=0 in Deployment status.
	pods, err := s.kube.request(ctx, http.MethodGet, s.kube.path("pod", "")+"?labelSelector="+map[string]string{"auth": "neon-control/managed-auth", "data_api": "neon-control/data-api"}[v.Kind]+"="+p.BranchID, nil)
	if err != nil {
		return err
	}
	if items, ok := pods["items"].([]any); !ok || len(items) != 0 {
		return errors.New("retire original Backend Pods before database rename")
	}
	return nil
}
func (s *server) captureCatalogBackendRename(ctx context.Context, p *catalogRenamePayload) error {
	plan, err := readCatalogBackendRenamePlan(ctx, s.db, *p)
	if err != nil {
		return err
	}
	p.Backend = plan
	for i := range plan.Items {
		v := &plan.Items[i]
		if err = s.verifyBackendRenameWorkload(ctx, *p, v, true); err != nil {
			return err
		}
		if v.Kind == "auth" || v.Kind == "data_api" {
			secret, e := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", v.SecretRef), nil)
			if e != nil || !owned(secret, p.ProjectID, v.Endpoint) || nested(secret, "metadata", "labels", "neon-control/branch-id") != p.BranchID || secret["immutable"] != true || nested(secret, "metadata", "deletionTimestamp") != nil {
				return errors.New("original immutable Backend connection identity rejected")
			}
			v.SecretUID = stringVal(nested(secret, "metadata", "uid"))
			if !functionResourceUID.MatchString(v.SecretUID) {
				return errors.New("original Backend credential UID required")
			}
		} else if v.Kind == "function" {
			if s.functions == nil || v.SQL.KeyVersion != s.functions.input.KeyVersion {
				return errFunctionSQLSecret
			}
			if _, err = s.kube.readFunctionSQLSecret(ctx, s.functions.key, v.sqlReference(*p)); err != nil {
				return err
			}
		}
	}
	return nil
}

type catalogBackendRenameBackings map[string]functionSQLReference

// Prepare new immutable files before native DDL. Retrying reads and validates
// their exact deterministic content; no original Secret is patched or removed.
func (s *server) prepareCatalogBackendRename(ctx context.Context, p catalogRenamePayload) (catalogBackendRenameBackings, error) {
	result := catalogBackendRenameBackings{}
	if p.Backend == nil {
		return result, nil
	}
	if err := checkCatalogBackendRenamePlan(ctx, s.db, p); err != nil {
		return nil, err
	}
	for _, v := range p.Backend.Items {
		if err := s.verifyBackendRenameWorkload(ctx, p, &v, false); err != nil {
			return nil, err
		}
		if v.Kind == "object_storage" {
			continue
		}
		if v.Kind == "function" {
			if s.functions == nil || v.SQL == nil || v.SQL.KeyVersion != s.functions.input.KeyVersion {
				return nil, errFunctionSQLSecret
			}
			old, err := s.kube.readFunctionSQLSecret(ctx, s.functions.key, v.sqlReference(p))
			if err != nil {
				return nil, err
			}
			next := old.Reference
			next.Generation++
			next.Name = "fn-" + strings.TrimPrefix(v.ID, "fnc_") + "-sql-" + strconv.FormatInt(next.Generation, 10)
			next.Scope.Database = p.NewName
			next.UID = ""
			next.MAC = ""
			// Generation authenticates a new database scope while preserving password,
			// verifier and login. The original receipt and Secret remain immutable.
			created, err := s.kube.reserveFunctionSQLSecret(ctx, s.functions.key, next, old.Credential)
			if err != nil {
				return nil, err
			}
			result[v.ID] = created.Reference
			continue
		}
		old, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", v.SecretRef), nil)
		if err != nil || !owned(old, p.ProjectID, v.Endpoint) || stringVal(nested(old, "metadata", "uid")) != v.SecretUID || nested(old, "metadata", "labels", "neon-control/branch-id") != p.BranchID || old["immutable"] != true || nested(old, "metadata", "deletionTimestamp") != nil {
			return nil, errors.New("original Backend credential changed")
		}
		data, err := catalogBackendRenameSecretData(old, p, v)
		if err != nil {
			return nil, err
		}
		body := record{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true, "metadata": record{"name": backendRenameSecretName(p, v), "labels": nested(old, "metadata", "labels")}, "data": data}
		if err = s.kube.reserveCatalogBackendRenameSecret(ctx, p, v, body); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (k *kubeClient) reserveCatalogBackendRenameSecret(ctx context.Context, p catalogRenamePayload, v catalogBackendRenameDependency, body map[string]any) error {
	name := backendRenameSecretName(p, v)
	if !functionEnvironmentNamespace.MatchString(name) || stringVal(nested(body, "metadata", "name")) != name {
		return errors.New("invalid new Backend connection backing name")
	}
	target, err := k.request(ctx, http.MethodGet, k.path("secret", name), nil)
	if kubeStatusIs(err, 404) {
		target, err = k.request(ctx, http.MethodPost, k.path("secret", ""), body)
		if err != nil {
			original := err
			target, err = k.request(ctx, http.MethodGet, k.path("secret", name), nil)
			// A failed recovery read must not hide a definitive creation error.
			if kubeStatusIs(err, 404) {
				return original
			}
		}
	}
	if err != nil {
		return err
	}
	actual, _ := json.Marshal(target["data"])
	expected, _ := json.Marshal(body["data"])
	if !owned(target, p.ProjectID, v.Endpoint) || nested(target, "metadata", "labels", "neon-control/branch-id") != p.BranchID || target["type"] != "Opaque" || target["immutable"] != true || nested(target, "metadata", "deletionTimestamp") != nil || !functionResourceUID.MatchString(stringVal(nested(target, "metadata", "uid"))) || string(actual) != string(expected) {
		return errors.New("new immutable Backend connection content rejected")
	}
	return nil
}

func commitCatalogBackendRenameTx(ctx context.Context, tx pgx.Tx, operation string, p catalogRenamePayload, backings catalogBackendRenameBackings) error {
	if p.Backend == nil {
		return nil
	}
	if err := checkCatalogBackendRenamePlan(ctx, tx, p); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('neon_control.catalog_backend_rename_operation',$1,true)`, operation); err != nil {
		return err
	}
	for _, v := range p.Backend.Items {
		if v.Kind == "function" {
			next, ok := backings[v.ID]
			old := v.sqlReference(p)
			if !ok || !next.valid() || next.UID == "" || next.MAC == "" || next.Scope.FunctionID != v.ID || next.Scope.ProjectID != p.ProjectID || next.Scope.BranchID != p.BranchID || next.Scope.Database != p.NewName || next.Scope.Schema != old.Scope.Schema || next.WriterID != old.WriterID || next.Generation != old.Generation+1 || next.MACKeyVersion != old.MACKeyVersion {
				return errFunctionSQLSecret
			}
			tag, err := tx.Exec(ctx, `UPDATE function_definitions SET database_name=$3,sql_credential_generation=$4,version=version+1,updated_at=now() WHERE id=$1 AND database_name=$2 AND sql_credential_generation=$5 AND state='suspended'`, v.ID, p.Name, p.NewName, next.Generation, old.Generation)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("original Function database scope changed")
			}
			_, err = tx.Exec(ctx, `INSERT INTO function_sql_credentials(function_id,project_id,branch_id,credential_generation,writer_id,database_name,sql_schema,provisioning_role,namespace,secret_name,secret_uid,content_mac,mac_key_version,created_operation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, v.ID, p.ProjectID, p.BranchID, next.Generation, next.WriterID, next.Scope.Database, next.Scope.Schema, next.Scope.ProvisioningRole, next.Namespace, next.Name, next.UID, next.MAC, next.MACKeyVersion, operation)
			if err != nil {
				return err
			}
			continue
		}
		table := map[string]string{"auth": "managed_auth_instances", "data_api": "data_api_instances", "object_storage": "object_storage_instances"}[v.Kind]
		if table == "" {
			return errors.New("unknown Backend dependency kind")
		}
		query := `UPDATE ` + table + ` SET spec=jsonb_set(spec,'{database}',to_jsonb($3::text)),updated_at=now()`
		args := []any{p.BranchID, p.ProjectID, p.NewName, v.Generation, p.Name, v.Spec}
		if v.Kind != "object_storage" {
			query += `,secret_ref=$7`
			args = append(args, backendRenameSecretName(p, v))
		}
		query += ` WHERE branch_id=$1 AND project_id=$2 AND generation=$4 AND spec->>'database'=$5 AND spec=$6::jsonb AND state='disabled'`
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("original dormant Backend spec changed")
		}
	}
	return nil
}

func sortedBackendKinds(plan *catalogBackendRenamePlan) []string {
	kinds := []string{}
	if plan != nil {
		for _, v := range plan.Items {
			kinds = append(kinds, v.Kind)
		}
	}
	sort.Strings(kinds)
	return kinds
}
