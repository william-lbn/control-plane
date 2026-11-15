-- Admit a new delete only through the successful suspension of the currently bound DSGA generation.
CREATE FUNCTION dsga_suspended_delete_source(text, text, text, text)
RETURNS text LANGUAGE sql STABLE AS $$
  SELECT gone.id FROM endpoints e
  JOIN operations gone ON gone.project_id=e.project_id AND gone.resource_type='endpoint' AND gone.resource_id=e.id
  JOIN dsga_suspension_intents intent ON intent.operation_id=gone.id
  JOIN dsga_suspension_retirements settlement ON settlement.operation_id=gone.id
  JOIN operations original ON original.id=gone.payload->>'creation_operation_id' AND original.project_id=e.project_id
  JOIN dsga_creation_bindings binding ON binding.operation_id=original.id AND binding.vm_uid=settlement.vm_uid
  WHERE e.project_id=$1 AND e.branch_id=$2 AND e.id=$3 AND e.workload_kind='neonvm'
   AND e.state IN ('suspended','deleting','deleted')
   AND gone.action='suspend_endpoint_dsga_v1' AND gone.state='succeeded' AND ($4='' OR gone.id=$4)
   AND gone.finished_at IS NOT NULL AND gone.lease_owner IS NULL AND gone.lease_expires_at IS NULL
   AND gone.payload->>'branch_id'=e.branch_id AND gone.payload->>'vm_uid'=binding.vm_uid
   AND original.state='succeeded' AND original.payload->>'endpoint_id'=e.id AND original.payload->>'branch_id'=e.branch_id
   AND binding.config_uid IS NOT NULL AND binding.vm_name=e.workload_name
   AND ((e.dsga_runtime_operation_id IS NULL AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1'))
     OR (e.dsga_runtime_operation_id=original.id AND original.action='resume_endpoint_dsga_v1'))
   AND EXISTS (SELECT 1 FROM operation_steps step WHERE step.operation_id=gone.id
      AND step.name='drain_and_settle_original_compute' AND step.state='succeeded')
   AND NOT EXISTS (SELECT 1 FROM operations successor
     WHERE successor.project_id=e.project_id AND successor.resource_type='endpoint' AND successor.resource_id=e.id
     AND successor.action='resume_endpoint_dsga_v1' AND successor.payload->>'resume_from'=gone.id
     AND NOT EXISTS(SELECT 1 FROM dsga_failed_resume_retirements evidence JOIN operations retirement ON retirement.id=evidence.operation_id
       WHERE evidence.failed_resume_operation_id=successor.id AND retirement.project_id=e.project_id
       AND retirement.resource_type='endpoint' AND retirement.resource_id=e.id
       AND retirement.action='retire_failed_dsga_resume_v1' AND retirement.state='succeeded'
       AND retirement.finished_at IS NOT NULL AND retirement.lease_owner IS NULL AND retirement.lease_expires_at IS NULL));
$$;

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
   IF NEW.action='disable_managed_auth' AND NEW.resource_type='managed_auth' AND es='suspended'
     AND NEW.payload->>'project_id'=NEW.project_id AND NEW.payload->>'branch_id'=bid AND NEW.resource_id=bid
     AND EXISTS (
       SELECT 1 FROM managed_auth_instances a
       JOIN endpoints e ON e.id=a.endpoint_id AND e.project_id=a.project_id AND e.branch_id=a.branch_id
       JOIN operations o ON o.project_id=e.project_id AND o.resource_type='endpoint' AND o.resource_id=e.id
       JOIN dsga_suspension_retirements r ON r.operation_id=o.id
       JOIN dsga_creation_bindings b ON b.operation_id=o.payload->>'creation_operation_id' AND b.vm_uid=r.vm_uid
       JOIN operations original ON original.id=b.operation_id AND original.project_id=e.project_id
       WHERE a.project_id=NEW.project_id AND a.branch_id=bid AND a.endpoint_id=eid
        AND a.generation::text=NEW.payload->>'generation' AND a.secret_ref=NEW.payload->>'secret_ref'
        AND a.spec=NEW.payload->'spec' AND (a.state='disabling' OR (TG_OP='UPDATE' AND a.state='degraded'))
        AND e.workload_kind='neonvm' AND e.endpoint_type='read_write' AND e.deleted_at IS NULL
        AND o.action='suspend_endpoint_dsga_v1' AND o.state='succeeded'
        AND o.payload->>'branch_id'=e.branch_id AND o.payload->>'vm_uid'=r.vm_uid
        AND original.state='succeeded' AND original.payload->>'endpoint_id'=e.id
        AND original.payload->>'branch_id'=e.branch_id
        AND (e.dsga_runtime_operation_id=original.id OR (e.dsga_runtime_operation_id IS NULL
          AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')))
        AND NOT EXISTS (SELECT 1 FROM operations conflicting WHERE conflicting.project_id=e.project_id
          AND conflicting.resource_type='endpoint' AND conflicting.resource_id=e.id
          AND conflicting.state IN ('queued','running','retry_wait'))
     ) THEN RETURN NEW; END IF;
   IF NEW.action='disable_data_api' AND NEW.resource_type='data_api' AND es='suspended'
     AND NEW.payload->>'project_id'=NEW.project_id AND NEW.payload->>'branch_id'=bid AND NEW.resource_id=bid
     AND EXISTS (
       SELECT 1 FROM data_api_instances a
       JOIN endpoints e ON e.id=a.endpoint_id AND e.project_id=a.project_id AND e.branch_id=a.branch_id
       JOIN operations o ON o.project_id=e.project_id AND o.resource_type='endpoint' AND o.resource_id=e.id
       JOIN dsga_suspension_retirements r ON r.operation_id=o.id
       JOIN dsga_creation_bindings b ON b.operation_id=o.payload->>'creation_operation_id' AND b.vm_uid=r.vm_uid
       JOIN operations original ON original.id=b.operation_id AND original.project_id=e.project_id
       WHERE a.project_id=NEW.project_id AND a.branch_id=bid AND a.endpoint_id=eid
        AND a.generation::text=NEW.payload->>'generation' AND a.secret_ref=NEW.payload->>'secret_ref'
        AND a.spec=NEW.payload->'spec' AND (a.state='disabling' OR (TG_OP='UPDATE' AND a.state='degraded'))
        AND e.workload_kind='neonvm' AND e.endpoint_type='read_write' AND e.deleted_at IS NULL
        AND o.action='suspend_endpoint_dsga_v1' AND o.state='succeeded'
        AND o.payload->>'branch_id'=e.branch_id AND o.payload->>'vm_uid'=r.vm_uid
        AND original.state='succeeded' AND original.payload->>'endpoint_id'=e.id
        AND original.payload->>'branch_id'=e.branch_id
        AND (e.dsga_runtime_operation_id=original.id OR (e.dsga_runtime_operation_id IS NULL
          AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')))
        AND NOT EXISTS (SELECT 1 FROM operations conflicting WHERE conflicting.project_id=e.project_id
          AND conflicting.resource_type='endpoint' AND conflicting.resource_id=e.id
          AND conflicting.state IN ('queued','running','retry_wait'))
     ) THEN RETURN NEW; END IF;
   IF act='create_endpoint' AND es='provisioning' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND act='create_endpoint' AND es='error' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND NEW.action IN ('delete_endpoint','cleanup_failed_endpoint') AND did=NEW.id AND es IN ('deleting','deleted') THEN RETURN NEW; END IF;
   IF TG_OP='INSERT' AND NEW.action='delete_endpoint' AND (es='suspended' OR NEW.payload ? 'suspension_operation_id') THEN
     original_id := NULLIF(NEW.payload->>'suspension_operation_id','');
     IF es='suspended' AND NEW.state='queued' AND NEW.resource_type='endpoint' AND NEW.resource_id=eid AND did IS NULL
       AND NEW.payload->>'project_id'=NEW.project_id AND NEW.payload->>'branch_id'=bid AND NEW.payload->>'endpoint_id'=eid
       AND NOT (NEW.payload ? 'failed_create_operation_id') AND original_id IS NOT NULL
       AND dsga_suspended_delete_source(NEW.project_id,bid,eid,original_id)=original_id THEN
       RETURN NEW;
     END IF;
     RAISE EXCEPTION 'original settled Compute suspension required' USING ERRCODE='23514';
   END IF;
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
