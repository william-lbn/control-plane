-- 创建Service属于原VM的持久路由身份；不能在同名重建后重新选择UID。
-- 只追加新表/守卫，不重写历史Operation、绑定或幂等键。
CREATE TABLE dsga_creation_services (
 operation_id text NOT NULL REFERENCES dsga_creation_bindings(operation_id) ON DELETE CASCADE,
 service_kind text NOT NULL CHECK (service_kind IN ('postgres','management')),
 namespace text NOT NULL,
 service_name text NOT NULL,
 vm_uid text NOT NULL,
 service_uid text NOT NULL UNIQUE CHECK (length(service_uid) BETWEEN 1 AND 128 AND service_uid ~ '^[!-~]+$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(operation_id,service_kind),
 UNIQUE(namespace,service_name)
);

CREATE FUNCTION guard_dsga_creation_service() RETURNS trigger LANGUAGE plpgsql AS $$
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
 PERFORM operation_id FROM dsga_creation_bindings b WHERE b.operation_id=NEW.operation_id
   AND b.namespace=NEW.namespace AND b.vm_uid=NEW.vm_uid AND b.config_uid IS NOT NULL
   AND NEW.service_name=(CASE NEW.service_kind WHEN 'management' THEN b.vm_name||'-management' ELSE b.vm_name END)
   FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA Service requires original VM and Config binding' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_creation_service_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_creation_services
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_creation_service();
