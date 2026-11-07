-- 自动供应只增加新v2发行，旧计划/身份不重写。
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
    -- 新生产接受显式v3/v4 placement；v4另需通用Guest合同；旧v1/v2仅可沿原计划绑定/结案，
    -- 不能UPDATE历史payload来补授权，也不在迁移中重写任何已有行。
    (TG_OP='INSERT' AND COALESCE(NEW.draft#>'{plan,vm,protocol_version}' IN ('3'::jsonb,'4'::jsonb),false) IS NOT TRUE) OR
    (TG_OP='UPDATE' AND NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '1'::jsonb AND
      NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '2'::jsonb AND
      NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '3'::jsonb AND
      NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '4'::jsonb) OR
    (NEW.draft#>'{plan,vm,protocol_version}' IN ('2'::jsonb,'3'::jsonb,'4'::jsonb) AND
      jsonb_typeof(NEW.draft#>'{plan,vm,resource_limits}') IS DISTINCT FROM 'object') OR
    (NEW.draft#>'{plan,vm,protocol_version}' IN ('3'::jsonb,'4'::jsonb) AND (
      jsonb_typeof(NEW.draft#>'{plan,vm,placement}') IS DISTINCT FROM 'object' OR
      jsonb_typeof(NEW.draft#>'{plan,vm,placement,node_selector}') IS DISTINCT FROM 'object' OR
      CASE WHEN jsonb_typeof(NEW.draft#>'{plan,vm,placement,allowed_node_names}')='array'
        THEN jsonb_array_length(NEW.draft#>'{plan,vm,placement,allowed_node_names}')<>3 OR
          EXISTS(SELECT 1 FROM jsonb_array_elements(NEW.draft#>'{plan,vm,placement,allowed_node_names}') n(value)
            WHERE jsonb_typeof(n.value)<>'string' OR n.value#>>'{}'='') OR
          (SELECT count(DISTINCT n.value) FROM jsonb_array_elements(NEW.draft#>'{plan,vm,placement,allowed_node_names}') n(value))<>3
        ELSE TRUE END)) OR
    (NEW.draft#>'{plan,vm,protocol_version}'='4'::jsonb AND (
      NEW.draft#>'{plan,native_bootstrap_version}' IS DISTINCT FROM '3'::jsonb OR
      COALESCE(NEW.draft#>'{plan,vm,generic_guest,protocol_version}' IN ('1'::jsonb,'2'::jsonb),false) IS NOT TRUE OR
      (NEW.draft#>'{plan,vm,generic_guest,protocol_version}'='2'::jsonb AND
        jsonb_typeof(NEW.draft#>'{plan,vm,generic_guest,supply}') IS DISTINCT FROM 'object') OR
      jsonb_typeof(NEW.draft#>'{plan,vm,generic_guest,policy_templates}') IS DISTINCT FROM 'object' OR
      NEW.draft#>>'{plan,vm,tls_cluster_issuer}' IS DISTINCT FROM '')) OR
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

-- 原创建操作的外部对象回执；不是资源分配账本。固定UID/摘要后只能原样读取。
CREATE TABLE dsga_creation_supply_facts (
 operation_id text NOT NULL REFERENCES operations(id),
 kind text NOT NULL CHECK(kind IN ('collector_config','qualification_job','qualification_result','runtime_selection')),
 value jsonb NOT NULL CHECK(jsonb_typeof(value)='object' AND octet_length(value::text)<=65536),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(operation_id,kind)
);
CREATE FUNCTION guard_dsga_supply_fact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'original supply fact immutable' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(SELECT 1 FROM operations o JOIN dsga_creation_bindings b ON b.operation_id=o.id
   WHERE o.id=NEW.operation_id AND o.action='create_project_dsga_v1' AND o.state='running'
   AND o.lease_expires_at>clock_timestamp() AND b.config_uid IS NOT NULL) THEN
   RAISE EXCEPTION 'supply requires original active creation binding' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_supply_fact_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_creation_supply_facts
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_supply_fact();
