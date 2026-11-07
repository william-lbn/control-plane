-- PG17/PG18共用同一创建合同，按原冻结计划验证真实版本；不修改历史迁移/计划。
-- 版本不匹配、未知版本、来源漂移、过期worker、原意图改写仍由原保护拒绝。
-- 只替换四个明确带PG17常量的函数，不扩资源分配器或修改运行实例。
CREATE OR REPLACE FUNCTION guard_operation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ps text; bs text; bid text; eid text; es text; did text; original_id text; rk text; rid text; original_payload jsonb; act text; pv integer;
BEGIN
 -- 新action是协议分界：旧worker不认识时只能失败，不能按PG16忽略新增字段执行。
 act := CASE NEW.action
   WHEN 'create_project_dsga_v1' THEN 'create_project'
   WHEN 'create_branch_dsga_v1' THEN 'create_branch'
   WHEN 'create_endpoint_dsga_v1' THEN 'create_endpoint'
   ELSE NEW.action END;
 IF act <> NEW.action THEN
   IF jsonb_typeof(NEW.payload->'dsga_creation') IS DISTINCT FROM 'object'
     OR (NEW.payload #> '{dsga_creation,protocol_version}') IS DISTINCT FROM '1'::jsonb
     OR COALESCE((NEW.payload #> '{dsga_creation,postgres_version}') IN ('17'::jsonb,'18'::jsonb),false) IS NOT TRUE
     OR NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id THEN
     RAISE EXCEPTION 'DSGA creation protocol or scope differs' USING ERRCODE='23514';
   END IF;
 ELSIF act IN ('create_project','create_branch','create_endpoint') AND NEW.payload ? 'dsga_creation' THEN
   RAISE EXCEPTION 'DSGA creation intent cannot use a legacy action' USING ERRCODE='23514';
 END IF;
 IF NEW.action IN ('cleanup_failed_project','cleanup_failed_branch') THEN
   rk := CASE NEW.action WHEN 'cleanup_failed_project' THEN 'project' ELSE 'branch' END;
   SELECT state INTO ps FROM projects WHERE id=NEW.project_id FOR UPDATE;
   IF jsonb_typeof(NEW.payload->'branch_ids') IS DISTINCT FROM 'array' OR jsonb_array_length(NEW.payload->'branch_ids')<>1 OR
     jsonb_typeof(NEW.payload->'endpoint_ids') IS DISTINCT FROM 'array' OR jsonb_array_length(NEW.payload->'endpoint_ids')>1 THEN
     RAISE EXCEPTION 'exact original parent cleanup graph required' USING ERRCODE='23514';
   END IF;
   bid := NEW.payload->'branch_ids'->>0;
   eid := NEW.payload->'endpoint_ids'->>0;
   rid := CASE rk WHEN 'project' THEN NEW.project_id ELSE bid END;
   original_id := NULLIF(NEW.payload->>'failed_create_operation_id','');
   SELECT state,deletion_operation_id INTO bs,did FROM branches WHERE id=bid AND project_id=NEW.project_id FOR UPDATE;
   SELECT payload INTO original_payload FROM operations WHERE id=original_id AND project_id=NEW.project_id
     AND resource_type=rk AND resource_id=rid AND action='create_'||rk AND state='failed' AND finished_at IS NOT NULL
     AND lease_owner IS NULL AND lease_expires_at IS NULL;
   IF NEW.resource_type IS DISTINCT FROM rk||'_lifecycle' OR NEW.resource_id IS DISTINCT FROM rid OR
     NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id OR original_payload IS NULL OR
     original_payload->>'project_id' IS DISTINCT FROM NEW.project_id OR original_payload->>'branch_id' IS DISTINCT FROM bid OR
     NULLIF(original_payload->>'endpoint_id','') IS DISTINCT FROM eid OR
     (SELECT count(*) FROM operations WHERE project_id=NEW.project_id AND resource_type=rk AND resource_id=rid AND action='create_'||rk)<>1 THEN
     RAISE EXCEPTION 'unique original failed parent creation required' USING ERRCODE='23514';
   END IF;
   IF TG_OP='INSERT' THEN
     IF bs IS DISTINCT FROM 'error' OR (rk='project' AND ps IS DISTINCT FROM 'error') OR (rk='branch' AND ps IS DISTINCT FROM 'ready') THEN
       RAISE EXCEPTION 'original failed parent state required' USING ERRCODE='23514';
     END IF;
   ELSE
     IF did IS DISTINCT FROM NEW.id OR bs NOT IN ('deleting','deleted') OR
       (rk='project' AND NOT EXISTS(SELECT 1 FROM projects WHERE id=NEW.project_id AND deletion_operation_id=NEW.id AND state IN ('deleting','deleted'))) OR
       (rk='branch' AND ps IS DISTINCT FROM 'ready') THEN
       RAISE EXCEPTION 'original parent cleanup intent required' USING ERRCODE='23514';
     END IF;
   END IF;
   RETURN NEW;
 END IF;
 IF NEW.action IN ('delete_project','delete_branch','recover_project') THEN RETURN NEW; END IF;
 SELECT state,postgres_version INTO ps,pv FROM projects WHERE id=NEW.project_id AND deleted_at IS NULL FOR UPDATE;
 IF act <> NEW.action AND to_jsonb(pv) IS DISTINCT FROM (NEW.payload #> '{dsga_creation,postgres_version}') THEN
   RAISE EXCEPTION 'DSGA project PostgreSQL version differs' USING ERRCODE='23514';
 END IF;
 IF act='create_project' AND ps IN ('provisioning','ready') THEN RETURN NEW; END IF;
 IF TG_OP='UPDATE' AND act='create_project' AND ps='error' THEN RETURN NEW; END IF;
 IF ps IS DISTINCT FROM 'ready' THEN
   RAISE EXCEPTION 'project lifecycle does not admit new work' USING ERRCODE='23514';
 END IF;
 bid := NULLIF(NEW.payload->>'branch_id','');
 eid := NULLIF(NEW.payload->>'endpoint_id','');
 IF NEW.resource_type='endpoint' THEN eid := NEW.resource_id; END IF;
 IF eid IS NOT NULL THEN
   SELECT branch_id INTO bid FROM endpoints WHERE id=eid AND project_id=NEW.project_id;
 END IF;
 IF bid IS NOT NULL THEN
   SELECT state INTO bs FROM branches WHERE id=bid AND project_id=NEW.project_id AND deleted_at IS NULL FOR UPDATE;
   IF act='create_branch' AND bs='creating' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND act='create_branch' AND bs='error' THEN RETURN NEW; END IF;
   IF bs IS DISTINCT FROM 'ready' THEN
     RAISE EXCEPTION 'branch lifecycle does not admit new work' USING ERRCODE='23514';
   END IF;
 END IF;
 IF eid IS NOT NULL THEN
   SELECT state,deletion_operation_id INTO es,did FROM endpoints WHERE id=eid AND project_id=NEW.project_id FOR UPDATE;
   IF act='create_endpoint' AND es='provisioning' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND act='create_endpoint' AND es='error' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND NEW.action IN ('delete_endpoint','cleanup_failed_endpoint') AND did=NEW.id AND es IN ('deleting','deleted') THEN RETURN NEW; END IF;
   IF TG_OP='INSERT' AND NEW.action IN ('delete_endpoint','cleanup_failed_endpoint') THEN
     original_id := NULLIF(NEW.payload->>'failed_create_operation_id','');
     IF NEW.action='cleanup_failed_endpoint' AND NEW.resource_type='endpoint' AND NEW.resource_id=eid AND es='error' AND original_id IS NOT NULL AND
       NEW.payload->>'project_id'=NEW.project_id AND NEW.payload->>'endpoint_id'=eid AND NEW.payload->>'branch_id'=bid AND
       (SELECT count(*) FROM operations WHERE project_id=NEW.project_id AND resource_type='endpoint' AND resource_id=eid AND action='create_endpoint')=1 AND
       EXISTS(SELECT 1 FROM operations o WHERE o.id=original_id AND o.project_id=NEW.project_id AND o.resource_type='endpoint' AND o.resource_id=eid AND o.action='create_endpoint'
         AND o.state='failed' AND o.finished_at IS NOT NULL AND o.lease_owner IS NULL AND o.lease_expires_at IS NULL
         AND o.payload->>'project_id'=NEW.project_id AND o.payload->>'branch_id'=bid AND o.payload->>'endpoint_id'=eid) THEN RETURN NEW; END IF;
     IF NEW.action='cleanup_failed_endpoint' OR es='error' OR original_id IS NOT NULL THEN
       RAISE EXCEPTION 'unique original failed endpoint creation required' USING ERRCODE='23514';
     END IF;
   END IF;
   IF es IS DISTINCT FROM 'active' THEN RAISE EXCEPTION 'endpoint lifecycle does not admit new work' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION guard_dsga_creation_tenant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
   IF EXISTS(SELECT 1 FROM operations WHERE id=OLD.operation_id) THEN
     RAISE EXCEPTION 'DSGA original Tenant fact is immutable' USING ERRCODE='23514';
   END IF;
   RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' THEN
   RAISE EXCEPTION 'DSGA original Tenant fact is immutable' USING ERRCODE='23514';
 END IF;
 PERFORM id FROM operations WHERE id=NEW.operation_id FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA original project creation required' USING ERRCODE='23514';
 END IF;
 -- 等待原Operation锁后才读clock_timestamp，不能用事务开始时的now()放过到期者。
 PERFORM id FROM operations WHERE id=NEW.operation_id AND action='create_project_dsga_v1'
   AND state='running' AND lease_owner IS NOT NULL AND lease_expires_at>clock_timestamp()
   AND payload->>'tenant_id'=NEW.tenant_id AND payload#>'{dsga_creation,postgres_version}' IN ('17'::jsonb,'18'::jsonb)
   AND payload#>>'{dsga_creation,protocol_version}'='1';
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA original Tenant identity and current lease required' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION guard_dsga_creation_timeline() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE original operations%ROWTYPE; expected jsonb;
BEGIN
 IF TG_OP='DELETE' THEN
   IF EXISTS(SELECT 1 FROM operations WHERE id=OLD.operation_id) THEN
     RAISE EXCEPTION 'DSGA original timeline intent is immutable' USING ERRCODE='23514';
   END IF;
   RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' THEN
   IF OLD.confirmed_at IS NOT NULL OR NEW.confirmed_at IS NULL OR
      (to_jsonb(NEW)-'confirmed_at') IS DISTINCT FROM (to_jsonb(OLD)-'confirmed_at') THEN
     RAISE EXCEPTION 'DSGA only first timeline confirmation is allowed' USING ERRCODE='23514';
   END IF;
   NEW.confirmed_at:=clock_timestamp();
 ELSIF NEW.confirmed_at IS NOT NULL THEN
   RAISE EXCEPTION 'DSGA timeline must retain original unconfirmed intent first' USING ERRCODE='23514';
 END IF;
 -- 调用方先锁Operation→timeline；触发器同序读取。取得锁后再读真实时钟，
 -- 不能用事务开始now()将等待期间已经过期的lease当有效。
 SELECT * INTO original FROM operations WHERE id=NEW.operation_id FOR SHARE;
 IF NOT FOUND OR original.action NOT IN ('create_project_dsga_v1','create_branch_dsga_v1') OR
    original.state<>'running' OR original.lease_owner IS NULL OR
    original.lease_expires_at IS NULL OR original.lease_expires_at<=clock_timestamp() OR
    original.payload->>'tenant_id' IS DISTINCT FROM NEW.tenant_id OR
    original.payload->>'timeline_id' IS DISTINCT FROM NEW.timeline_id OR
    COALESCE(original.payload#>'{dsga_creation,postgres_version}' IN ('17'::jsonb,'18'::jsonb),false) IS NOT TRUE OR
    original.payload#>>'{dsga_creation,protocol_version}' IS DISTINCT FROM '1' THEN
   RAISE EXCEPTION 'DSGA original timeline identity and live lease required' USING ERRCODE='23514';
 END IF;
 expected:=jsonb_build_object('new_timeline_id',NEW.timeline_id,'pg_version',original.payload#>'{dsga_creation,postgres_version}');
 IF COALESCE(original.payload->>'parent_timeline_id','')<>'' THEN
   expected:=expected||jsonb_build_object('ancestor_timeline_id',original.payload->>'parent_timeline_id',
      'ancestor_start_lsn',original.payload->>'parent_lsn');
 END IF;
 IF NEW.request IS DISTINCT FROM expected OR
    (original.action='create_project_dsga_v1' AND COALESCE(original.payload->>'parent_timeline_id','')<>'') OR
    (original.action='create_branch_dsga_v1' AND COALESCE(original.payload->>'parent_timeline_id','')='') THEN
   RAISE EXCEPTION 'DSGA frozen timeline request differs' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

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
    COALESCE(NEW.draft#>'{plan,postgres_version}' IN ('17'::jsonb,'18'::jsonb),false) IS NOT TRUE OR
    -- 新生产只接受显式v3 placement；旧v1/v2仅可沿原不可变计划绑定来源/结案，
    -- 不能UPDATE历史payload来补授权，也不在迁移中重写任何已有行。
    (TG_OP='INSERT' AND NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '3'::jsonb) OR
    (TG_OP='UPDATE' AND NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '1'::jsonb AND
      NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '2'::jsonb AND
      NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '3'::jsonb) OR
    (NEW.draft#>'{plan,vm,protocol_version}' IN ('2'::jsonb,'3'::jsonb) AND
      jsonb_typeof(NEW.draft#>'{plan,vm,resource_limits}') IS DISTINCT FROM 'object') OR
    (NEW.draft#>'{plan,vm,protocol_version}' IS NOT DISTINCT FROM '3'::jsonb AND (
      jsonb_typeof(NEW.draft#>'{plan,vm,placement}') IS DISTINCT FROM 'object' OR
      jsonb_typeof(NEW.draft#>'{plan,vm,placement,node_selector}') IS DISTINCT FROM 'object' OR
      CASE WHEN jsonb_typeof(NEW.draft#>'{plan,vm,placement,allowed_node_names}')='array'
        THEN jsonb_array_length(NEW.draft#>'{plan,vm,placement,allowed_node_names}')<>3 OR
          EXISTS(SELECT 1 FROM jsonb_array_elements(NEW.draft#>'{plan,vm,placement,allowed_node_names}') n(value)
            WHERE jsonb_typeof(n.value)<>'string' OR n.value#>>'{}'='') OR
          (SELECT count(DISTINCT n.value) FROM jsonb_array_elements(NEW.draft#>'{plan,vm,placement,allowed_node_names}') n(value))<>3
        ELSE TRUE END)) OR
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
       WHERE p.id=op.project_id AND p.org_id=NEW.org_id AND p.state='provisioning' AND to_jsonb(p.postgres_version)=NEW.draft#>'{plan,postgres_version}'
         AND p.name=NEW.draft->>'resource_name' AND p.region_id=NEW.draft->>'region'
         AND b.id=NEW.draft#>>'{payload,branch_id}' AND b.name=NEW.draft->>'default_branch' AND b.state='creating'
         AND e.id=NEW.draft#>>'{payload,endpoint_id}' AND e.state='provisioning' AND e.endpoint_type='read_write'
         AND e.role_name='app_owner' AND e.database_name='neondb'
         AND e.slot_size_mib::numeric*1048576=(NEW.draft#>>'{plan,memory_slot_bytes}')::numeric) OR
     EXISTS(SELECT 1 FROM operation_steps WHERE operation_id=NEW.operation_id AND state<>'queued') OR
     (SELECT array_agg(name ORDER BY ordinal) FROM operation_steps WHERE operation_id=NEW.operation_id)
       IS DISTINCT FROM ARRAY['create_managed_tenant','create_timeline','prepare_neonvm_dsga','wait_neonvm_running',
          'confirm_native_initialization','publish_proxy_route','sql_probe_via_proxy','persist_ready_state']::text[]
     AND NOT (NEW.draft#>'{plan,postgres_version}'='17'::jsonb AND
       (SELECT array_agg(name ORDER BY ordinal) FROM operation_steps WHERE operation_id=NEW.operation_id)
         IS NOT DISTINCT FROM ARRAY['create_managed_tenant','create_timeline_pg17','prepare_neonvm_dsga','wait_neonvm_running',
          'confirm_native_initialization','publish_proxy_route','sql_probe_via_proxy','persist_ready_state']::text[])) THEN
     RAISE EXCEPTION 'DSGA project requires complete original resource graph and steps' USING ERRCODE='23514';
   END IF;
 END IF;
 RETURN NEW;
END $$;
