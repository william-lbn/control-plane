-- 前向修正reservation与已发布VM资源计划v2生产者的版本约定。
-- 不编辑900000061、不UPDATE任何历史reservation/Operation/来源或UID，不重建表。
-- 完整保留原函数的锁序、键互斥、不可变原意图、凭据首次绑定及资源图接受约束；
-- 只扩大格式识别至生产v2，并在新INSERT拒绝无资源对象的伪v2及旧v1。
-- 这里仅是数据库结构守卫。完整Limits数值/总量、来源和实际资源强制由原Go
-- validateResourceBudget/Runner/Guest路径核验，不能由SQL格式成功推导已落实预算。
CREATE OR REPLACE FUNCTION guard_dsga_creation_reservation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE expected jsonb; op operations%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN
   RAISE EXCEPTION 'DSGA original reservation cannot be removed' USING ERRCODE='23514';
 END IF;
 IF TG_OP='INSERT' THEN
   PERFORM dsga_lock_creation_key(NEW.actor_id,NEW.key_hash);
   IF EXISTS(SELECT 1 FROM idempotency_keys WHERE actor_id=NEW.actor_id AND key_hash=NEW.key_hash) THEN
     RAISE EXCEPTION 'idempotency key already accepted' USING ERRCODE='23505';
   END IF;
   IF NEW.credential_reference IS NOT NULL OR NEW.accepted_id IS NOT NULL OR NEW.accepted_at IS NOT NULL THEN
     RAISE EXCEPTION 'DSGA reservation must precede external credentials' USING ERRCODE='23514';
   END IF;
 ELSE
   -- 只允许来源首次固定及最终接受两次前进，禁止改原计划/键/时间或清空来源。
   IF ROW(NEW.actor_id,NEW.key_hash,NEW.request_hash,NEW.scope,NEW.org_id,NEW.base_action,NEW.operation_id,NEW.draft,NEW.created_at)
      IS DISTINCT FROM ROW(OLD.actor_id,OLD.key_hash,OLD.request_hash,OLD.scope,OLD.org_id,OLD.base_action,OLD.operation_id,OLD.draft,OLD.created_at)
      OR (OLD.credential_reference IS NOT NULL AND NEW.credential_reference IS DISTINCT FROM OLD.credential_reference)
      OR OLD.accepted_id IS NOT NULL OR NEW IS NOT DISTINCT FROM OLD THEN
     RAISE EXCEPTION 'DSGA original reservation is immutable' USING ERRCODE='23514';
   END IF;
 END IF;
 IF NEW.draft->'protocol_version' IS DISTINCT FROM '1'::jsonb OR
    jsonb_typeof(NEW.draft->'payload') IS DISTINCT FROM 'object' OR
    jsonb_typeof(NEW.draft->'plan') IS DISTINCT FROM 'object' OR
    NEW.draft#>'{plan,protocol_version}' IS DISTINCT FROM '1'::jsonb OR
    NEW.draft#>'{plan,postgres_version}' IS DISTINCT FROM '17'::jsonb OR
    -- 新reservation必须使用真实生产者要求的v2并带显式资源对象；不能把
    -- 旧v1原意图从当前env补成v2。已有v1仅沿旧不可变守卫保留；实际Go恢复
    -- 仍严格要求完整v2，数据库格式兼容不授予Guest启动/预算或执行许可。
    (NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '2'::jsonb AND
      NOT (TG_OP='UPDATE' AND NEW.draft#>'{plan,vm,protocol_version}' IS NOT DISTINCT FROM '1'::jsonb)) OR
    (NEW.draft#>'{plan,vm,protocol_version}' IS NOT DISTINCT FROM '2'::jsonb AND
      jsonb_typeof(NEW.draft#>'{plan,vm,resource_limits}') IS DISTINCT FROM 'object') OR
    NEW.draft#>'{plan,credentials}' IS NOT NULL THEN
   RAISE EXCEPTION 'DSGA original reservation plan rejected' USING ERRCODE='23514';
 END IF;
 IF NEW.accepted_id IS NOT NULL THEN
   SELECT * INTO op FROM operations WHERE id=NEW.accepted_id;
   expected := (NEW.draft->'payload') || jsonb_build_object('dsga_creation',
     (NEW.draft->'plan') || jsonb_build_object('credentials',NEW.credential_reference));
   IF op.id IS NULL OR op.id IS DISTINCT FROM NEW.operation_id OR op.actor_id IS DISTINCT FROM NEW.actor_id OR
      op.state IS DISTINCT FROM 'queued' OR
      op.action IS DISTINCT FROM NEW.base_action||'_dsga_v1' OR op.payload IS DISTINCT FROM expected OR
      op.project_id IS DISTINCT FROM NEW.draft#>>'{payload,project_id}' OR
      op.resource_type IS DISTINCT FROM replace(NEW.base_action,'create_','') OR
      op.resource_id IS DISTINCT FROM (CASE NEW.base_action WHEN 'create_project' THEN NEW.draft#>>'{payload,project_id}'
        WHEN 'create_branch' THEN NEW.draft#>>'{payload,branch_id}' ELSE NEW.draft#>>'{payload,endpoint_id}' END) OR
      NOT EXISTS(SELECT 1 FROM projects WHERE id=op.project_id AND org_id=NEW.org_id) OR
      NOT EXISTS(SELECT 1 FROM idempotency_keys WHERE actor_id=NEW.actor_id AND key_hash=NEW.key_hash
        AND request_hash=NEW.request_hash AND operation_id=NEW.operation_id) THEN
     RAISE EXCEPTION 'DSGA acceptance must use original operation and complete plan' USING ERRCODE='23514';
   END IF;
   -- 新项目接受时完整资源图及步骤必须已在同一事务内，不接受只有Operation
   -- 但缺默认分支/普通业务身份或初始化步骤的伪成功。后续worker不更新此见证。
   IF NEW.base_action='create_project' AND (
     NOT EXISTS(SELECT 1 FROM projects p JOIN branches b ON b.id=p.default_branch_id AND b.project_id=p.id
       JOIN endpoints e ON e.project_id=p.id AND e.branch_id=b.id
       WHERE p.id=op.project_id AND p.org_id=NEW.org_id AND p.state='provisioning' AND p.postgres_version=17
         AND p.name=NEW.draft->>'resource_name' AND p.region_id=NEW.draft->>'region'
         AND b.id=NEW.draft#>>'{payload,branch_id}' AND b.name=NEW.draft->>'default_branch' AND b.state='creating'
         AND e.id=NEW.draft#>>'{payload,endpoint_id}' AND e.state='provisioning' AND e.endpoint_type='read_write'
         AND e.role_name='app_owner' AND e.database_name='neondb'
         AND e.slot_size_mib::numeric*1048576=(NEW.draft#>>'{plan,memory_slot_bytes}')::numeric) OR
     EXISTS(SELECT 1 FROM operation_steps WHERE operation_id=NEW.operation_id AND state<>'queued') OR
     (SELECT array_agg(name ORDER BY ordinal) FROM operation_steps WHERE operation_id=NEW.operation_id)
       IS DISTINCT FROM ARRAY['create_managed_tenant','create_timeline_pg17','prepare_neonvm_dsga','wait_neonvm_running',
          'confirm_native_initialization','publish_proxy_route','sql_probe_via_proxy','persist_ready_state']::text[]) THEN
     RAISE EXCEPTION 'DSGA project requires complete original resource graph and steps' USING ERRCODE='23514';
   END IF;
 END IF;
 RETURN NEW;
END $$;
