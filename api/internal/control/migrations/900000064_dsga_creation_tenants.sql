-- Tenant先于VM创建，不能用VM绑定表表达其存在事实。根Tenant一旦确认，原意图
-- 恢复只能读取它；404不能重建同ID空Tenant，更不能因步骤状态被重试而遗忘事实。
CREATE TABLE dsga_creation_tenants (
 operation_id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
 tenant_id text NOT NULL UNIQUE CHECK (tenant_id ~ '^[0-9a-f]{32}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION guard_dsga_creation_tenant() RETURNS trigger LANGUAGE plpgsql AS $$
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
   AND payload->>'tenant_id'=NEW.tenant_id AND payload#>>'{dsga_creation,postgres_version}'='17'
   AND payload#>>'{dsga_creation,protocol_version}'='1';
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA original Tenant identity and current lease required' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_creation_tenant_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_creation_tenants
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_creation_tenant();
