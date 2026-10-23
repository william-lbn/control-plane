-- 初始化记录是某次真实观察的审计事实，不是当前准入/VM许可。每个PG boot首份
-- 观察永久保留；新boot另记，不覆盖旧记录，也不能从旧记录直接重开普通SQL。
CREATE TABLE dsga_creation_initialization_observations (
 operation_id text NOT NULL REFERENCES dsga_creation_bindings(operation_id) ON DELETE CASCADE,
 -- PG boot是uint64，可能超过SQL bigint上界；按规范十进制保留，不截断/转浮点。
 pg_boot_id text NOT NULL CHECK (pg_boot_id ~ '^[1-9][0-9]{0,19}$' AND pg_boot_id::numeric <= 18446744073709551615),
 vm_uid text NOT NULL,
 config_uid text NOT NULL,
 credentials_uid text NOT NULL REFERENCES dsga_creation_credential_sources(credentials_uid),
 management_service_uid text NOT NULL REFERENCES dsga_creation_services(service_uid),
 receipt jsonb NOT NULL CHECK (jsonb_typeof(receipt)='object' AND octet_length(receipt::text)<=65536),
 observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(operation_id,pg_boot_id)
);

CREATE FUNCTION guard_dsga_creation_initialization_observation() RETURNS trigger LANGUAGE plpgsql AS $$
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
       AND NEW.receipt->'startup'=jsonb_build_object('protocol_version',2,'vm_uid',b.vm_uid,
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
CREATE TRIGGER dsga_creation_initialization_observation_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_creation_initialization_observations
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_creation_initialization_observation();
