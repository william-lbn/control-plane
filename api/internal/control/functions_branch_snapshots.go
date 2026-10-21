package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The caller holds the original project/parent lifecycle locks. Freeze the
// accepted code pointer now; delayed Writer creation must never read latest.
func (s *server) snapshotFunctionBranchTx(ctx context.Context, tx pgx.Tx, project, parent, branch, creation, actor string) error {
	rows, err := tx.Query(ctx, `SELECT f.id,f.version,COALESCE(f.active_deployment_id,''),
  f.active_deployment_id IS NULL OR (d.state='completed' AND b.deployment_id IS NOT NULL),
  COALESCE((SELECT jsonb_agg(jsonb_build_object('id',t.id,'type',t.type,'name',t.name,
   'function_path',t.function_path,'cron',t.cron,'bucket_name',t.bucket_name,'key_prefix',t.key_prefix,
   'origin_branch_id',t.origin_branch_id,'source_generation',t.generation) ORDER BY t.id)
   FROM function_triggers t WHERE t.function_id=f.id AND t.project_id=f.project_id AND t.branch_id=f.branch_id AND t.state='ready'),'[]'::jsonb)
  FROM function_definitions f LEFT JOIN function_deployments d ON d.id=f.active_deployment_id
  LEFT JOIN function_deployment_backings b ON b.deployment_id=d.id
  WHERE f.project_id=$1 AND f.branch_id=$2 AND f.state NOT IN ('deleting','deleted') ORDER BY f.slug`, project, parent)
	if err != nil {
		return err
	}
	type item struct {
		id, source string
		version    int64
		ready      bool
		triggers   []byte
	}
	items := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.version, &v.source, &v.ready, &v.triggers); err != nil {
			break
		}
		items = append(items, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range items {
		if !v.ready {
			return errFunctionCopy
		}
		canonical, _ := json.Marshal(record{"action": "snapshot_branch_function", "project": project, "branch": branch, "source_function": v.id, "source_deployment": v.source, "source_version": v.version, "creation_operation": creation})
		fingerprint := s.functionFingerprint(canonical)
		functionID := s.functionRequestID("fnc_", actor, branch+"/"+v.id, fingerprint)
		operationID := s.functionRequestID("op_", actor, branch+"/"+v.id, fingerprint)
		deploymentID := ""
		if v.source != "" {
			deploymentID = s.functionRequestID("fdp_", actor, branch+"/"+v.id, fingerprint)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO function_branch_copy_plans(branch_id,project_id,source_branch_id,source_function_id,source_deployment_id,source_version,function_id,deployment_id,operation_id,admitted_actor_id,creation_operation_id,trigger_snapshots) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$7,NULLIF($8,''),$9,$10,$11,$12)`, branch, project, parent, v.id, v.source, v.version, functionID, deploymentID, operationID, actor, creation, v.triggers); err != nil {
			return err
		}
	}
	return nil
}

// Materialization performs metadata admission only, after the original native
// Writer is ready. Its own transaction keeps retries on the same planned IDs.
// Function quota failures never turn an already-ready database branch failed.
func (s *server) prepareFunctionBranchPlans(ctx context.Context, project, branch string) error {
	if !s.functionBranchCopyEnabled() {
		return errFunctionsPlatform
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var org, writer, locked string
	if err = tx.QueryRow(ctx, `SELECT p.org_id FROM projects p JOIN organizations o ON o.id=p.org_id WHERE p.id=$1 AND p.state='ready' AND p.source='managed' AND p.deleted_at IS NULL AND o.state='active' FOR UPDATE OF p`, project).Scan(&org); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND state='ready' AND deleted_at IS NULL FOR UPDATE`, branch, project).Scan(&locked); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND endpoint_type='read_write' AND workload_kind='neonvm' AND state IN ('active','suspended') AND deleted_at IS NULL FOR UPDATE`, project, branch).Scan(&writer); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT plan.function_id,COALESCE(plan.deployment_id,''),plan.operation_id,plan.source_function_id,plan.source_branch_id,COALESCE(plan.source_deployment_id,''),plan.source_version,plan.admitted_actor_id,creation.request_id,source.slug,source.database_name,source.sql_schema,plan.trigger_snapshots
  FROM function_branch_copy_plans plan JOIN function_definitions source ON source.id=plan.source_function_id
  JOIN operations creation ON creation.id=plan.creation_operation_id
  WHERE plan.project_id=$1 AND plan.branch_id=$2 AND plan.prepared_function_id IS NULL ORDER BY source.slug FOR UPDATE OF plan`, project, branch)
	if err != nil {
		return err
	}
	admissions := []functionCopyAdmission{}
	for rows.Next() {
		a := functionCopyAdmission{Org: org, Writer: writer}
		a.Target.ProjectID, a.Target.BranchID, a.Target.Generation = project, branch, 1
		if err = rows.Scan(&a.Target.FunctionID, &a.Target.DeploymentID, &a.Operation, &a.Source.Function, &a.Source.Branch, &a.Source.Deployment, &a.SourceVersion, &a.Source.Actor, &a.Request, &a.Target.Slug, &a.Database, &a.Schema, &a.TriggerSnapshots); err != nil {
			break
		}
		admissions = append(admissions, a)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range admissions {
		if err = s.authorizeFunctionCopyActor(ctx, project, a.Source.Actor); err != nil {
			return err
		}
		if a.Source.Deployment == "" {
			if err = insertFunctionCopyDefinitionTx(ctx, tx, a); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,started_at,finished_at) VALUES($1,$2,'function',$3,'create_function','succeeded',$4,$5,'{}',now(),now())`, a.Operation, project, a.Target.FunctionID, a.Source.Actor, a.Request); err != nil {
				return err
			}
		} else {
			err = tx.QueryRow(ctx, `SELECT d.runtime,d.bundle_digest,d.bundle_bytes,d.entry,d.environment_names FROM function_deployments d JOIN function_deployment_backings b ON b.deployment_id=d.id AND b.function_id=d.function_id AND b.project_id=d.project_id AND b.branch_id=d.branch_id WHERE d.id=$1 AND d.function_id=$2 AND d.project_id=$3 AND d.branch_id=$4 AND d.state='completed'`, a.Source.Deployment, a.Source.Function, project, a.Source.Branch).Scan(&a.Source.Summary.Runtime, &a.Source.Summary.BundleDigest, &a.Source.Summary.BundleBytes, &a.Source.Summary.Entry, &a.Source.Summary.EnvironmentNames)
			if err != nil {
				return errFunctionCopy
			}
			if err = insertFunctionCopyTx(ctx, tx, a); err != nil {
				return err
			}
		}
		if err = insertPausedFunctionTriggersTx(ctx, tx, a); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE function_branch_copy_plans SET prepared_function_id=function_id,prepared_operation_id=operation_id,prepared_at=clock_timestamp(),last_error_code=NULL,last_attempt_at=clock_timestamp() WHERE project_id=$1 AND branch_id=$2 AND source_function_id=$3 AND prepared_function_id IS NULL`, project, branch, a.Source.Function); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Stable trigger IDs are branch scoped already. Child edits and tombstones
// operate on their own row; no scheduler occurrence or invocation is copied.
func insertPausedFunctionTriggersTx(ctx context.Context, tx pgx.Tx, a functionCopyAdmission) error {
	var snapshots []json.RawMessage
	if json.Unmarshal(a.TriggerSnapshots, &snapshots) != nil {
		return errFunctionCopy
	}
	if len(snapshots) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(794210063)`); err != nil {
		return err
	}
	var total, branch int
	if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE branch_id=$1) FROM function_triggers WHERE state='ready'`, a.Target.BranchID).Scan(&total, &branch); err != nil {
		return err
	}
	if total+len(snapshots) > 128 || branch+len(snapshots) > 16 {
		return errFunctionTriggerCopyQuota
	}
	_, err := tx.Exec(ctx, `INSERT INTO function_triggers(branch_id,id,project_id,org_id,function_id,actor_id,origin_branch_id,type,name,function_path,cron,enabled,next_run_at,bucket_name,key_prefix)
 SELECT $1,t.id,$2,$3,$4,$5,t.origin_branch_id,t.type,t.name,t.function_path,t.cron,false,NULL,t.bucket_name,t.key_prefix
 FROM jsonb_to_recordset($6::jsonb) AS t(id text,origin_branch_id text,type text,name text,function_path text,cron text,bucket_name text,key_prefix text)`, a.Target.BranchID, a.Target.ProjectID, a.Org, a.Target.FunctionID, a.Source.Actor, a.TriggerSnapshots)
	return err
}

var errFunctionTriggerCopyQuota = errors.New("branch Function trigger copy budget exhausted")

func (s *server) functionBranchCopyController(ctx context.Context) {
	if s.processRole != roleWorker {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.functionBranchCopyEnabled() {
				continue
			}
			rows, err := s.db.Query(ctx, `SELECT plan.project_id,plan.branch_id FROM function_branch_copy_plans plan
   JOIN projects p ON p.id=plan.project_id JOIN branches b ON b.id=plan.branch_id
   WHERE plan.prepared_function_id IS NULL AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL
   AND (plan.last_attempt_at IS NULL OR plan.last_attempt_at<clock_timestamp()-interval '15 seconds')
   AND EXISTS(SELECT 1 FROM endpoints e WHERE e.project_id=plan.project_id AND e.branch_id=plan.branch_id AND e.endpoint_type='read_write' AND e.workload_kind='neonvm' AND e.state IN ('active','suspended') AND e.deleted_at IS NULL)
   GROUP BY plan.project_id,plan.branch_id ORDER BY min(plan.last_attempt_at) NULLS FIRST,plan.branch_id LIMIT 4`)
			if err != nil {
				continue
			}
			targets := [][2]string{}
			for rows.Next() {
				var v [2]string
				if rows.Scan(&v[0], &v[1]) == nil {
					targets = append(targets, v)
				}
			}
			rows.Close()
			for _, v := range targets {
				call, done := context.WithTimeout(ctx, 10*time.Second)
				err = s.prepareFunctionBranchPlans(call, v[0], v[1])
				done()
				code := ""
				if err != nil {
					code = "function_copy_preparation_pending"
					var pgerr *pgconn.PgError
					if errors.As(err, &pgerr) && pgerr.Code == "23514" && pgerr.Message == "function definition budget exhausted" {
						code = "function_copy_quota_exceeded"
					}
					if errors.Is(err, errFunctionTriggerCopyQuota) {
						code = "function_trigger_copy_quota_exceeded"
					}
				}
				s.db.Exec(ctx, `UPDATE function_branch_copy_plans SET last_attempt_at=clock_timestamp(),last_error_code=NULLIF($3,'') WHERE project_id=$1 AND branch_id=$2 AND prepared_function_id IS NULL`, v[0], v[1], code)
			}
		}
	}
}

func (s *server) functionBranchCopyPlans(w http.ResponseWriter, r *http.Request) {
	if !s.functionReadBranch(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	items, err := s.many(r.Context(), `SELECT plan.source_branch_id,plan.source_function_id,plan.source_deployment_id,plan.source_version::text AS source_version_token,source.slug,
  plan.prepared_function_id AS function_id,plan.prepared_operation_id AS operation_id,plan.created_at AS snapshot_at,plan.prepared_at,plan.last_error_code,
  CASE WHEN plan.prepared_function_id IS NOT NULL THEN CASE WHEN plan.source_deployment_id IS NULL THEN 'definition_only' ELSE COALESCE(op.state,'operation_unavailable') END
  WHEN NOT $3 THEN 'operator_disabled'
  WHEN NOT EXISTS(SELECT 1 FROM endpoints e WHERE e.project_id=plan.project_id AND e.branch_id=plan.branch_id AND e.endpoint_type='read_write' AND e.workload_kind='neonvm' AND e.state IN ('active','suspended') AND e.deleted_at IS NULL) THEN 'waiting_for_writer'
  WHEN plan.last_error_code IS NOT NULL THEN 'blocked' ELSE 'pending' END AS preparation_state,
  target.state AS function_state
  FROM function_branch_copy_plans plan JOIN function_definitions source ON source.id=plan.source_function_id
  LEFT JOIN operations op ON op.id=plan.prepared_operation_id LEFT JOIN function_definitions target ON target.id=plan.prepared_function_id
  WHERE plan.project_id=$1 AND plan.branch_id=$2 ORDER BY source.slug LIMIT 100`, r.PathValue("project"), r.PathValue("branch"), s.functionBranchCopyEnabled())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Branch Function snapshot unavailable")
		return
	}
	jsonResponse(w, 200, record{"items": items, "branch_copy_enabled": s.functionBranchCopyEnabled(), "source_policy": "frozen_at_branch_admission_not_historical_sql_code", "execution": "prepared_paused_no_customer_code"})
}
