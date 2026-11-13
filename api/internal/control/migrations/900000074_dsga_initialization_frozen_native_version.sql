-- 以原创建计划冻结的 Native 版本校验回执。历史计划缺少该字段时沿用 v2。
-- 仅替换校验函数，保留现有触发器与每个 PG boot 的原始观察。
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
   AND action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1') FOR SHARE;
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
