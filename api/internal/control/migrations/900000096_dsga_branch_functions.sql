-- DSGA 分步分支/Writer 的 Functions 继承。保留历史 045 和旧分支合同；新
-- 来源必须有冻结选择、原 idempotency 接受记录、同项目原祖先与确认时间线。
-- 只授权暂停副本元数据准备；SQL/Secret/Guest 仍由原 Worker 独立核验。
CREATE FUNCTION dsga_function_branch_source_valid(p_operation text,p_project text,
 p_branch text,p_source text,p_actor text,p_ready boolean) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT EXISTS(
  SELECT 1 FROM operations o JOIN projects p ON p.id=o.project_id
  JOIN branches b ON b.id=p_branch AND b.project_id=p.id
  JOIN idempotency_keys k ON k.operation_id=o.id AND k.actor_id=o.actor_id
  WHERE o.id=p_operation AND o.project_id=p_project AND o.resource_type='branch'
   AND o.resource_id=b.id AND o.action='create_branch_dsga_v1' AND o.actor_id=p_actor
   AND p.source='managed' AND p.state='ready' AND p.deleted_at IS NULL
   AND p.postgres_version IN (17,18) AND b.deleted_at IS NULL
   AND b.parent_branch_id=p_source AND p_source<>p_branch
   AND o.payload->>'project_id'=p.id AND o.payload->>'branch_id'=b.id
   AND o.payload->>'tenant_id'=p.tenant_id AND o.payload->>'timeline_id'=b.timeline_id
   AND o.payload->>'parent_branch_id'=p_source
   AND COALESCE(o.payload->>'endpoint_id','')=''
   AND o.payload->>'endpoint_type'='read_write'
   AND o.payload->>'catalog_policy'='preserve_inherited_sql_v1'
   AND o.payload->>'branch_function_copy_policy'='inherit_paused'
   AND o.payload->>'parent_lsn'=b.parent_lsn
   AND o.payload->>'parent_timeline_id' ~ '^[0-9a-f]{32}$'
   AND o.payload#>'{dsga_creation,protocol_version}'='1'::jsonb
   AND o.payload#>'{dsga_creation,postgres_version}'=to_jsonb(p.postgres_version)
   AND ((NOT p_ready AND o.state='queued' AND b.state='creating' AND EXISTS(
       SELECT 1 FROM branches original WHERE original.id=p_source AND original.project_id=p.id
        AND original.timeline_id=o.payload->>'parent_timeline_id' AND original.state='ready'
        AND original.deleted_at IS NULL AND original.expires_at IS NULL))
     OR (p_ready AND o.state='succeeded' AND b.state='ready' AND EXISTS(
       SELECT 1 FROM dsga_creation_timelines t WHERE t.operation_id=o.id
        AND t.tenant_id=p.tenant_id AND t.timeline_id=b.timeline_id AND t.confirmed_at IS NOT NULL
        AND t.request=jsonb_build_object('new_timeline_id',b.timeline_id,'pg_version',p.postgres_version,
          'ancestor_timeline_id',o.payload->>'parent_timeline_id','ancestor_start_lsn',o.payload->>'parent_lsn'))))
 );
$$;
REVOKE ALL ON FUNCTION dsga_function_branch_source_valid(text,text,text,text,text,boolean) FROM PUBLIC;

CREATE OR REPLACE FUNCTION guard_function_branch_copy_plan() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND (
  ROW(NEW.branch_id,NEW.project_id,NEW.source_branch_id,NEW.source_function_id,NEW.source_deployment_id,
   NEW.source_version,NEW.function_id,NEW.deployment_id,NEW.operation_id,NEW.admitted_actor_id,NEW.creation_operation_id,NEW.created_at,NEW.trigger_snapshots)
  IS DISTINCT FROM
  ROW(OLD.branch_id,OLD.project_id,OLD.source_branch_id,OLD.source_function_id,OLD.source_deployment_id,
   OLD.source_version,OLD.function_id,OLD.deployment_id,OLD.operation_id,OLD.admitted_actor_id,OLD.creation_operation_id,OLD.created_at,OLD.trigger_snapshots)
  OR (OLD.prepared_function_id IS NOT NULL AND NEW IS DISTINCT FROM OLD)
 ) THEN
  RAISE EXCEPTION 'function branch copy plan is immutable' USING ERRCODE='23514';
 END IF;
 IF TG_OP='INSERT' AND NOT EXISTS(
  SELECT 1 FROM function_definitions f JOIN operations o ON o.id=NEW.creation_operation_id
  WHERE f.id=NEW.source_function_id AND f.project_id=NEW.project_id AND f.branch_id=NEW.source_branch_id
  AND f.version=NEW.source_version AND f.state NOT IN ('deleting','deleted')
  AND o.project_id=NEW.project_id AND o.resource_id=NEW.branch_id AND o.resource_type='branch'
  AND (o.action='create_branch' OR
    (o.action='create_branch_dsga_v1' AND dsga_function_branch_source_valid(
      NEW.creation_operation_id,NEW.project_id,NEW.branch_id,NEW.source_branch_id,NEW.admitted_actor_id,false)))
  AND o.actor_id=NEW.admitted_actor_id
  AND NEW.trigger_snapshots=COALESCE((SELECT jsonb_agg(jsonb_build_object('id',t.id,'type',t.type,'name',t.name,
   'function_path',t.function_path,'cron',t.cron,'bucket_name',t.bucket_name,'key_prefix',t.key_prefix,
   'origin_branch_id',t.origin_branch_id,'source_generation',t.generation) ORDER BY t.id)
   FROM function_triggers t WHERE t.function_id=f.id AND t.project_id=f.project_id AND t.branch_id=f.branch_id AND t.state='ready'),'[]'::jsonb)
  AND ((NEW.source_deployment_id IS NULL AND f.active_deployment_id IS NULL) OR
   EXISTS(SELECT 1 FROM function_deployments d WHERE d.id=NEW.source_deployment_id AND d.id=f.active_deployment_id
    AND d.function_id=f.id AND d.state='completed'))
 ) THEN
  RAISE EXCEPTION 'function branch snapshot source rejected' USING ERRCODE='23514';
 END IF;
 -- 元数据物化前再次核原分支完成与原时间线确认；只增加 action 名不构成准入。
 IF NEW.prepared_function_id IS NOT NULL AND EXISTS(
   SELECT 1 FROM operations WHERE id=NEW.creation_operation_id AND action='create_branch_dsga_v1'
 ) AND NOT dsga_function_branch_source_valid(NEW.creation_operation_id,NEW.project_id,
     NEW.branch_id,NEW.source_branch_id,NEW.admitted_actor_id,true) THEN
   RAISE EXCEPTION 'DSGA original branch completion is unconfirmed' USING ERRCODE='23514';
 END IF;
 IF NEW.prepared_function_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM function_definitions f JOIN operations o ON o.id=NEW.prepared_operation_id
  WHERE f.id=NEW.prepared_function_id AND f.project_id=NEW.project_id AND f.branch_id=NEW.branch_id
  AND o.project_id=f.project_id AND o.resource_id=f.id AND o.resource_type='function' AND o.actor_id=NEW.admitted_actor_id
  AND ((NEW.source_deployment_id IS NULL AND o.action='create_function' AND f.active_deployment_id IS NULL AND f.target_deployment_id IS NULL) OR
   EXISTS(SELECT 1 FROM function_copy_sources c WHERE c.deployment_id=NEW.deployment_id AND c.function_id=f.id
    AND c.source_deployment_id=NEW.source_deployment_id AND c.source_function_id=NEW.source_function_id
    AND c.source_branch_id=NEW.source_branch_id AND c.source_version=NEW.source_version
    AND c.admitted_operation_id=o.id))
 ) THEN
  RAISE EXCEPTION 'function branch snapshot preparation rejected' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
