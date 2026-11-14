-- A logical endpoint keeps its original name and credential Secret across
-- successive physical boots. The receipts of all generations remain immutable.
ALTER TABLE endpoints ADD COLUMN dsga_runtime_operation_id text REFERENCES operations(id);

CREATE UNIQUE INDEX dsga_resume_one_operation_per_suspension ON operations ((payload->>'resume_from'))
 WHERE action='resume_endpoint_dsga_v1';

ALTER TABLE dsga_creation_services
 ADD COLUMN resume_generation boolean NOT NULL DEFAULT false,
 DROP CONSTRAINT dsga_creation_services_namespace_service_name_key;
CREATE UNIQUE INDEX dsga_creation_services_original_name ON dsga_creation_services(namespace,service_name) WHERE NOT resume_generation;

ALTER TABLE dsga_creation_initialization_observations
 DROP CONSTRAINT dsga_creation_initialization_observations_credentials_uid_fkey;
ALTER TABLE dsga_creation_credential_sources
 ADD COLUMN resume_generation boolean NOT NULL DEFAULT false,
 DROP CONSTRAINT dsga_creation_credential_sources_credentials_uid_key,
 ADD UNIQUE(operation_id,credentials_uid);
CREATE UNIQUE INDEX dsga_creation_credentials_original_uid ON dsga_creation_credential_sources(credentials_uid) WHERE NOT resume_generation;
ALTER TABLE dsga_creation_initialization_observations
 ADD FOREIGN KEY(operation_id,credentials_uid) REFERENCES dsga_creation_credential_sources(operation_id,credentials_uid);

CREATE OR REPLACE FUNCTION guard_operation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ps text; bs text; bid text; eid text; es text; did text; original_id text; rk text; rid text; original_payload jsonb; act text; pv integer;
BEGIN
 -- 新action是协议分界：旧worker不认识时只能失败，不能按PG16忽略新增字段执行。
 act := CASE NEW.action
   WHEN 'create_project_dsga_v1' THEN 'create_project'
   WHEN 'create_branch_dsga_v1' THEN 'create_branch'
   WHEN 'create_endpoint_dsga_v1' THEN 'create_endpoint'
   WHEN 'resume_endpoint_dsga_v1' THEN 'resume_endpoint'
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
     AND resource_type=rk AND resource_id=rid AND action IN ('create_'||rk,'create_'||rk||'_dsga_v1') AND state='failed' AND finished_at IS NOT NULL
     AND lease_owner IS NULL AND lease_expires_at IS NULL;
   IF NEW.resource_type IS DISTINCT FROM rk||'_lifecycle' OR NEW.resource_id IS DISTINCT FROM rid OR
     NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id OR original_payload IS NULL OR
     original_payload->>'project_id' IS DISTINCT FROM NEW.project_id OR original_payload->>'branch_id' IS DISTINCT FROM bid OR
     NULLIF(original_payload->>'endpoint_id','') IS DISTINCT FROM eid OR
     (SELECT count(*) FROM operations WHERE project_id=NEW.project_id AND resource_type=rk AND resource_id=rid AND action IN ('create_'||rk,'create_'||rk||'_dsga_v1'))<>1 THEN
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
   IF act='resume_endpoint' AND es='suspended' THEN RETURN NEW; END IF;
   IF act='create_endpoint' AND es='provisioning' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND act='create_endpoint' AND es='error' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND NEW.action IN ('delete_endpoint','cleanup_failed_endpoint') AND did=NEW.id AND es IN ('deleting','deleted') THEN RETURN NEW; END IF;
   IF TG_OP='INSERT' AND NEW.action IN ('delete_endpoint','cleanup_failed_endpoint') THEN
     original_id := NULLIF(NEW.payload->>'failed_create_operation_id','');
     IF NEW.action='cleanup_failed_endpoint' AND NEW.resource_type='endpoint' AND NEW.resource_id=eid AND es='error' AND original_id IS NOT NULL AND
       NEW.payload->>'project_id'=NEW.project_id AND NEW.payload->>'endpoint_id'=eid AND NEW.payload->>'branch_id'=bid AND
       (SELECT count(*) FROM operations WHERE project_id=NEW.project_id AND resource_type='endpoint' AND resource_id=eid AND action IN ('create_endpoint','create_endpoint_dsga_v1'))=1 AND
       EXISTS(SELECT 1 FROM operations o WHERE o.id=original_id AND o.project_id=NEW.project_id AND o.resource_type='endpoint' AND o.resource_id=eid AND o.action IN ('create_endpoint','create_endpoint_dsga_v1')
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

CREATE FUNCTION guard_dsga_resume_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.state<>'queued' OR NEW.resource_type<>'endpoint'
  OR NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id
  OR NEW.payload->>'endpoint_id' IS DISTINCT FROM NEW.resource_id
  OR NEW.payload#>'{dsga_creation,resume_generation}' IS DISTINCT FROM 'true'::jsonb
  OR NEW.payload#>'{dsga_creation,retain_catalog}' IS DISTINCT FROM 'true'::jsonb
  OR NOT EXISTS (
   SELECT 1 FROM endpoints e JOIN projects p ON p.id=e.project_id
   JOIN branches br ON br.id=e.branch_id AND br.project_id=p.id
   JOIN operations gone ON gone.id=NEW.payload->>'resume_from'
   JOIN dsga_suspension_retirements retired ON retired.operation_id=gone.id
   JOIN operations original ON original.id=gone.payload->>'creation_operation_id'
   JOIN dsga_creation_bindings previous ON previous.operation_id=original.id
   JOIN dsga_creation_credential_sources c ON c.operation_id=original.id
   WHERE e.id=NEW.resource_id AND e.project_id=NEW.project_id AND e.state='suspended'
    AND e.deleted_at IS NULL AND e.workload_kind='neonvm' AND e.endpoint_type=NEW.payload->>'endpoint_type'
    AND to_jsonb(e.min_memory_mib)=NEW.payload#>'{dsga_creation,resume_min_memory_mib}'
    AND to_jsonb(e.max_memory_mib)=NEW.payload#>'{dsga_creation,resume_max_memory_mib}'
    AND to_jsonb(e.min_cpu_milli)=NEW.payload->'min_cpu_milli' AND to_jsonb(e.max_cpu_milli)=NEW.payload->'max_cpu_milli'
    AND p.deleted_at IS NULL AND p.state='ready' AND br.state='ready' AND br.deleted_at IS NULL AND br.expires_at IS NULL
    AND e.branch_id=NEW.payload->>'branch_id' AND p.tenant_id=NEW.payload->>'tenant_id' AND br.timeline_id=NEW.payload->>'timeline_id'
    AND gone.action='suspend_endpoint_dsga_v1' AND gone.state='succeeded' AND gone.finished_at IS NOT NULL
    AND gone.lease_owner IS NULL AND gone.lease_expires_at IS NULL
    AND gone.project_id=p.id AND gone.resource_type='endpoint' AND gone.resource_id=e.id AND gone.payload->>'branch_id'=e.branch_id
    AND EXISTS(SELECT 1 FROM operation_steps s WHERE s.operation_id=gone.id AND s.ordinal=1 AND s.state='succeeded')
    AND original.project_id=p.id AND original.state='succeeded'
    AND original.payload->>'endpoint_id'=e.id AND original.payload->>'branch_id'=e.branch_id
    AND original.payload->>'tenant_id'=p.tenant_id AND original.payload->>'timeline_id'=br.timeline_id
    AND ((e.dsga_runtime_operation_id=original.id AND original.action='resume_endpoint_dsga_v1')
      OR (e.dsga_runtime_operation_id IS NULL AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')))
    AND previous.vm_uid=retired.vm_uid AND previous.vm_uid=gone.payload->>'vm_uid' AND previous.config_uid IS NOT NULL
    AND previous.compute_instance_id=NEW.payload#>>'{dsga_creation,compute_instance_id}'
    AND previous.namespace=NEW.payload#>>'{dsga_creation,namespace}'
    AND previous.incarnation<>NEW.payload#>>'{dsga_creation,incarnation}'
    AND original.payload#>'{dsga_creation,operation_uuid}'<>NEW.payload#>'{dsga_creation,operation_uuid}'
    AND original.payload#>'{dsga_creation,storage}'=NEW.payload#>'{dsga_creation,storage}'
    AND original.payload#>'{dsga_creation,credentials}'=NEW.payload#>'{dsga_creation,credentials}'
    AND c.credentials_uid=NEW.payload#>>'{dsga_creation,credentials,uid}'
    AND encode(c.content_digest,'hex')=NEW.payload#>>'{dsga_creation,credentials,content_sha256}'
  ) THEN
   RAISE EXCEPTION 'resume requires the latest settled suspension and original credentials' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER dsga_resume_admission BEFORE INSERT ON operations
 FOR EACH ROW WHEN (NEW.action='resume_endpoint_dsga_v1')
 EXECUTE FUNCTION guard_dsga_resume_operation();

CREATE TRIGGER dsga_resume_immutable BEFORE UPDATE ON operations
 FOR EACH ROW WHEN (OLD.action='resume_endpoint_dsga_v1' OR NEW.action='resume_endpoint_dsga_v1')
 EXECUTE FUNCTION guard_dsga_creation_intent();

CREATE OR REPLACE FUNCTION guard_dsga_creation_service() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE frozen_action text;
BEGIN
 IF TG_OP='DELETE' THEN
   IF EXISTS(SELECT 1 FROM operations WHERE id=OLD.operation_id) THEN
     RAISE EXCEPTION 'DSGA original Service binding is immutable' USING ERRCODE='23514';
   END IF;
   RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' THEN
   RAISE EXCEPTION 'DSGA original Service binding is immutable' USING ERRCODE='23514';
 END IF;
 -- 与Go保持Operation→创建绑定→Service事实锁序。取得原Operation锁后再检查
 -- 当前时钟；等待期间到期不能靠事务开始时的now()获得新绑定。
 SELECT action INTO frozen_action FROM operations WHERE id=NEW.operation_id
   AND action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1','resume_endpoint_dsga_v1') FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA original creation operation required' USING ERRCODE='23514';
 END IF;
 PERFORM id FROM operations WHERE id=NEW.operation_id AND state='running'
   AND lease_owner IS NOT NULL AND lease_expires_at>clock_timestamp();
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA current creation lease required' USING ERRCODE='23514';
 END IF;
 PERFORM operation_id FROM dsga_creation_bindings b WHERE b.operation_id=NEW.operation_id
   AND b.namespace=NEW.namespace AND b.vm_uid=NEW.vm_uid AND b.config_uid IS NOT NULL
   AND NEW.service_name=(CASE NEW.service_kind WHEN 'management' THEN b.vm_name||'-management' ELSE b.vm_name END)
   FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA Service requires original VM and Config binding' USING ERRCODE='23514';
 END IF;
 NEW.resume_generation := frozen_action='resume_endpoint_dsga_v1';
 IF NEW.resume_generation AND NOT EXISTS (
   SELECT 1 FROM operations resume JOIN operations gone ON gone.id=resume.payload->>'resume_from'
    JOIN dsga_creation_services prior ON prior.operation_id=gone.payload->>'creation_operation_id'
   WHERE resume.id=NEW.operation_id AND gone.state='succeeded' AND prior.namespace=NEW.namespace AND prior.service_name=NEW.service_name AND prior.service_kind=NEW.service_kind
 ) THEN
   RAISE EXCEPTION 'resume receipt differs from the settled predecessor' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION guard_dsga_creation_credential_source() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE frozen_action text;
BEGIN
 IF TG_OP='DELETE' THEN
   -- 仅原Operation删除后的FK CASCADE清理可移除事实；保留原意图时禁止清空重绑。
   IF EXISTS(SELECT 1 FROM operations WHERE id=OLD.operation_id) THEN
     RAISE EXCEPTION 'DSGA credential source is immutable' USING ERRCODE='23514';
   END IF;
   RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' THEN
   RAISE EXCEPTION 'DSGA credential source is immutable' USING ERRCODE='23514';
 END IF;
 -- 锁序与Go一致：原Operation→绑定。Config UID出现后不能首次补认一份凭据，
 -- 防止把旧ready实例当作新创建；已有来源的恢复只读，不经过此INSERT。
 SELECT action INTO frozen_action FROM operations WHERE id=NEW.operation_id
   AND action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1','resume_endpoint_dsga_v1') FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA original creation operation required' USING ERRCODE='23514';
 END IF;
 PERFORM operation_id FROM dsga_creation_bindings WHERE operation_id=NEW.operation_id AND config_uid IS NULL FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA credential source must precede config binding' USING ERRCODE='23514';
 END IF;
 NEW.resume_generation := frozen_action='resume_endpoint_dsga_v1';
 IF NEW.resume_generation AND NOT EXISTS (
   SELECT 1 FROM operations resume JOIN operations gone ON gone.id=resume.payload->>'resume_from'
    JOIN dsga_creation_credential_sources prior ON prior.operation_id=gone.payload->>'creation_operation_id'
   WHERE resume.id=NEW.operation_id AND gone.state='succeeded' AND prior.credentials_uid=NEW.credentials_uid AND prior.content_digest=NEW.content_digest AND prior.source_version=NEW.source_version
 ) THEN
   RAISE EXCEPTION 'resume receipt differs from the settled predecessor' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION guard_dsga_creation_initialization_observation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
   IF EXISTS(SELECT 1 FROM operations WHERE id=OLD.operation_id) THEN
     RAISE EXCEPTION 'DSGA initialization observation is immutable' USING ERRCODE='23514';
   END IF;
   RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' THEN
   RAISE EXCEPTION 'DSGA initialization observation is immutable' USING ERRCODE='23514';
 END IF;
 -- 与Go相同：Operation→原创建绑定→凭据/Service→本boot观察。不跨网络持锁。
 PERFORM id FROM operations WHERE id=NEW.operation_id
   AND action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1','resume_endpoint_dsga_v1') FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA original creation operation required' USING ERRCODE='23514';
 END IF;
 PERFORM id FROM operations WHERE id=NEW.operation_id AND state='running'
   AND lease_owner IS NOT NULL AND lease_expires_at>clock_timestamp();
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA current creation lease required' USING ERRCODE='23514';
 END IF;
 IF NOT EXISTS (
   SELECT 1 FROM operations o JOIN dsga_creation_bindings b ON b.operation_id=o.id
     JOIN dsga_creation_credential_sources c ON c.operation_id=o.id
     JOIN dsga_creation_services s ON s.operation_id=o.id AND s.service_kind='management'
   WHERE o.id=NEW.operation_id AND NEW.vm_uid=b.vm_uid AND NEW.config_uid=b.config_uid
     AND NEW.credentials_uid=c.credentials_uid AND NEW.management_service_uid=s.service_uid
     AND s.vm_uid=b.vm_uid
     AND (NEW.receipt->'protocol_version'='1'::jsonb AND NEW.receipt->'vm_shrink_permit'='false'::jsonb
       AND NEW.receipt->'authentication_policy_verified'='true'::jsonb AND NEW.receipt->'ordinary_roles_verified'='true'::jsonb
       AND NEW.receipt->>'operation_uuid'=o.payload#>>'{dsga_creation,operation_uuid}'
       AND NEW.receipt->>'tenant_id'=o.payload->>'tenant_id' AND NEW.receipt->>'timeline_id'=o.payload->>'timeline_id'
       AND COALESCE(o.payload#>'{dsga_creation,native_bootstrap_version}','2'::jsonb) IN ('2'::jsonb,'3'::jsonb)
       AND NEW.receipt->'startup'=jsonb_build_object(
          'protocol_version',COALESCE(o.payload#>'{dsga_creation,native_bootstrap_version}','2'::jsonb),'vm_uid',b.vm_uid,
          'compute_instance_id',b.compute_instance_id,
          'bootstrap_buffer_bytes',o.payload#>'{dsga_creation,budget,bootstrap_buffer_bytes}',
          'maximum_buffer_bytes',o.payload#>'{dsga_creation,budget,maximum_buffer_bytes}',
          'minimum_guest_memory_bytes',o.payload#>'{dsga_creation,budget,minimum_guest_memory_bytes}',
          'non_buffer_reserve_bytes',o.payload#>'{dsga_creation,budget,non_buffer_reserve_bytes}')
       AND NEW.receipt#>'{observed,instance}'=jsonb_build_object('vm_uid',b.vm_uid,
          'compute_instance_id',b.compute_instance_id,'pg_boot_id',NEW.pg_boot_id)
       AND NEW.receipt-ARRAY['protocol_version','vm_shrink_permit','operation_uuid','tenant_id','timeline_id',
          'startup','authentication_policy_verified','ordinary_roles_verified','observed']='{}'::jsonb) IS TRUE
 ) THEN
   RAISE EXCEPTION 'DSGA initialization observation differs from original creation bindings' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION guard_dsga_supply_fact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'original supply fact immutable' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(
   SELECT 1 FROM operations o JOIN dsga_creation_bindings b ON b.operation_id=o.id
   WHERE o.id=NEW.operation_id AND o.state='running'
   AND o.lease_owner IS NOT NULL AND o.lease_expires_at>clock_timestamp() AND b.config_uid IS NOT NULL
   AND (o.action='create_project_dsga_v1' OR (
     o.action IN ('create_endpoint_dsga_v1','resume_endpoint_dsga_v1') AND o.resource_type='endpoint'
     AND o.resource_id=o.payload->>'endpoint_id'
     AND o.payload#>'{dsga_creation,retain_catalog}'='true'::jsonb
   ))
 ) THEN
   RAISE EXCEPTION 'supply requires original active creation binding' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION guard_dsga_suspension_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' OR NOT EXISTS (
  SELECT 1 FROM operations o
   JOIN operations original ON original.id=o.payload->>'creation_operation_id'
   JOIN dsga_creation_bindings b ON b.operation_id=original.id
   WHERE o.id=NEW.operation_id AND o.action='suspend_endpoint_dsga_v1'
    AND o.resource_type='endpoint' AND o.state='running'
    AND o.lease_owner IS NOT NULL AND o.lease_expires_at>clock_timestamp()
    AND o.project_id=o.payload->>'project_id' AND o.resource_id=o.payload->>'endpoint_id'
    AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1','resume_endpoint_dsga_v1')
    AND original.state='succeeded' AND original.project_id=o.project_id
    AND original.payload->>'endpoint_id'=o.resource_id
    AND original.payload->>'branch_id'=o.payload->>'branch_id'
    AND b.vm_uid=o.payload->>'vm_uid'
    AND b.namespace=NEW.intent #>> '{selection,next,namespace}'
    AND b.vm_name=NEW.intent #>> '{selection,next,name}'
    AND b.vm_uid=NEW.intent #>> '{selection,next,instance,vm_uid}'
    AND b.compute_instance_id=NEW.intent #>> '{selection,next,instance,compute_instance_id}'
    AND b.incarnation=NEW.intent #>> '{selection,next,incarnation}'
    AND NEW.operation_id=NEW.intent #>> '{selection,next,suspension_operation}'
 ) THEN
  RAISE EXCEPTION 'suspension intent requires an active operation and its original Compute' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION guard_dsga_suspend_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF OLD.action IS DISTINCT FROM NEW.action OR NEW.payload IS DISTINCT FROM OLD.payload
    OR ROW(NEW.project_id,NEW.resource_type,NEW.resource_id) IS DISTINCT FROM ROW(OLD.project_id,OLD.resource_type,OLD.resource_id) THEN
   RAISE EXCEPTION 'original suspend operation is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF NEW.state<>'queued' OR NEW.resource_type<>'endpoint'
   OR NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id
   OR NEW.payload->>'endpoint_id' IS DISTINCT FROM NEW.resource_id
   OR NOT EXISTS (
    SELECT 1 FROM endpoints e JOIN branches br ON br.id=e.branch_id AND br.project_id=e.project_id
     JOIN operations original ON original.id=NEW.payload->>'creation_operation_id'
     JOIN dsga_creation_bindings b ON b.operation_id=original.id
    WHERE e.id=NEW.resource_id AND e.project_id=NEW.project_id AND e.state='active' AND e.deleted_at IS NULL
     AND e.workload_kind='neonvm' AND original.project_id=e.project_id
     AND ((e.dsga_runtime_operation_id=original.id AND original.action='resume_endpoint_dsga_v1')
       OR (e.dsga_runtime_operation_id IS NULL AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')))
     AND original.state='succeeded' AND original.finished_at IS NOT NULL
     AND original.payload->>'endpoint_id'=e.id AND original.payload->>'branch_id'=e.branch_id
     AND original.payload->>'timeline_id'=br.timeline_id AND NEW.payload->>'branch_id'=e.branch_id
     AND b.vm_uid=NEW.payload->>'vm_uid' AND b.config_uid IS NOT NULL
     AND EXISTS(SELECT 1 FROM dsga_creation_supply_facts f WHERE f.operation_id=original.id AND f.kind='runtime_selection')
   ) THEN
  RAISE EXCEPTION 'suspend operation requires the original ready Compute' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
