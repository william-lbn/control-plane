package control

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functions"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

type functionCopyAdmission struct {
	Target                                            functionInstanceTarget
	Source                                            functionCopySource
	SourceVersion                                     int64
	TriggerSnapshots                                  []byte
	Org, Writer, Database, Schema, Operation, Request string
}

func insertFunctionCopyDefinitionTx(ctx context.Context, tx pgx.Tx, a functionCopyAdmission) error {
	scope := functionsql.Scope{FunctionID: a.Target.FunctionID, ProjectID: a.Target.ProjectID, BranchID: a.Target.BranchID, Database: a.Database, Schema: a.Schema, ProvisioningRole: "control_probe"}
	if scope.Validate() != nil || !functions.ValidSlug(a.Target.Slug) || !functionWriterEndpointID.MatchString(a.Writer) || a.SourceVersion < 1 || a.Source.Actor == "" || a.Source.Branch == a.Target.BranchID {
		return errFunctionCopy
	}
	_, err := tx.Exec(ctx, `INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'provisioning')`, a.Target.FunctionID, a.Org, a.Target.ProjectID, a.Target.BranchID, a.Writer, a.Target.Slug, a.Database, a.Schema, scope.Role())
	return err
}

// Shared metadata-only admission for explicit copies and frozen branch plans.
// The caller locks lifecycle/authority and owns the transaction/HTTP receipt.
func insertFunctionCopyTx(ctx context.Context, tx pgx.Tx, a functionCopyAdmission) error {
	if !a.Target.valid() || !functionDefinitionID.MatchString(a.Source.Function) || !functionDeploymentID.MatchString(a.Source.Deployment) {
		return errFunctionCopy
	}
	if err := insertFunctionCopyDefinitionTx(ctx, tx, a); err != nil {
		return err
	}
	p, source := a.Target, a.Source
	payload, _ := json.Marshal(p)
	if _, err := tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'function',$3,'deploy_function','queued',$4,$5,$6)`, a.Operation, p.ProjectID, p.FunctionID, source.Actor, a.Request, payload); err != nil {
		return err
	}
	artifact, err := functionArtifactKey(p.ProjectID, p.BranchID, source.Summary.BundleDigest)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO function_deployments(id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,artifact_key,environment_secret_ref,environment_names,actor_id,operation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID, source.Summary.Runtime, source.Summary.BundleDigest, source.Summary.BundleBytes, source.Summary.Entry, artifact, "fdp-"+p.DeploymentID[4:]+"-environment", source.Summary.EnvironmentNames, source.Actor, a.Operation); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE function_definitions SET target_deployment_id=$2 WHERE id=$1`, p.FunctionID, p.DeploymentID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO function_copy_sources(deployment_id,function_id,project_id,branch_id,source_function_id,source_branch_id,source_deployment_id,source_version,admitted_actor_id,admitted_operation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, p.DeploymentID, p.FunctionID, p.ProjectID, p.BranchID, source.Function, source.Branch, source.Deployment, a.SourceVersion, source.Actor, a.Operation); err != nil {
		return err
	}
	for i, name := range []string{"verify_and_rewrap_copy_backing", "apply_destination_sql_identity", "commit_paused_function_copy"} {
		if _, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, a.Operation, i, name); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'copy_function_branch','function',$2,'accepted',$3)`, source.Actor, p.FunctionID, a.Request)
	return err
}
