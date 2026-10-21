-- 两个已分别发布的分支均使用058，旧schema_migrations只记录数字，不能重命名
-- 或改写任何历史文件假装从未应用。本前向迁移在同一事务内补齐缺失的整份DDL；
-- 新库、原DSGA058、原主线058三条来源最终均具备两套功能。Go登记器仅允许这一
-- 已知冲突，其他重复编号在执行SQL前失败。DSGA后续使用900000000以上保留段，
-- 避免独立主线继续前进时反复冲突；数字仍在既有integer范围内。
-- 已存在的表不DROP/重建，也不清空数据；存在但私有约束/触发器缺半边则拒绝。
DO $dsga_058_join$
BEGIN
 IF to_regclass('dsga_creation_bindings') IS NULL THEN
   EXECUTE $original_dsga_058$
-- 仅追加迁移，不修改已经应用的001—057。完整保留041的失败清理守卫，
-- 并为版本化创建映射等价的生命周期准入。启动UID事实独立存储，避免在Operation
-- payload上重编码uint64预算，也避免绑定时触发父资源准入的反向取锁。
CREATE TABLE dsga_creation_bindings (
 operation_id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
 namespace text NOT NULL CHECK (length(namespace) BETWEEN 1 AND 63),
 vm_name text NOT NULL CHECK (length(vm_name) BETWEEN 1 AND 253),
 incarnation text NOT NULL CHECK (incarnation ~ '^[0-9a-f]{32}$' AND incarnation <> repeat('0',32)),
 compute_instance_id text NOT NULL CHECK (length(compute_instance_id) BETWEEN 1 AND 128 AND compute_instance_id ~ '^[!-~]+$'),
 intent_digest bytea NOT NULL CHECK (octet_length(intent_digest)=32),
 vm_uid text NOT NULL UNIQUE CHECK (length(vm_uid) BETWEEN 1 AND 128 AND vm_uid ~ '^[!-~]+$'),
 config_uid text UNIQUE CHECK (config_uid IS NULL OR (length(config_uid) BETWEEN 1 AND 128 AND config_uid ~ '^[!-~]+$')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

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
     OR (NEW.payload #> '{dsga_creation,postgres_version}') IS DISTINCT FROM '17'::jsonb
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
 IF act <> NEW.action AND pv IS DISTINCT FROM 17 THEN
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

-- 一旦接受DSGA创建意图，其协议/作用域/全部payload及授权审计身份不能改写。
-- 状态、lease和结果仍由既有worker更新；UID单独存表，不需要打开payload后门。
CREATE FUNCTION guard_dsga_creation_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.action IS DISTINCT FROM OLD.action OR NEW.project_id IS DISTINCT FROM OLD.project_id
   OR NEW.resource_type IS DISTINCT FROM OLD.resource_type OR NEW.resource_id IS DISTINCT FROM OLD.resource_id
   OR NEW.payload IS DISTINCT FROM OLD.payload OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
   OR NEW.request_id IS DISTINCT FROM OLD.request_id OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
   RAISE EXCEPTION 'DSGA creation intent is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_creation_intent_immutable BEFORE UPDATE ON operations
 FOR EACH ROW WHEN (OLD.action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1')
 OR NEW.action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1'))
 EXECUTE FUNCTION guard_dsga_creation_intent();

-- 数据库也保护one-shot绑定：VM/指纹/命名空间始终固定，Config只可NULL→原UID。
CREATE FUNCTION guard_dsga_creation_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- 原Operation仍在时不能DELETE再INSERT来改UID。仅允许原Operation已删除后的
 -- FK CASCADE清理；此时该原意图已无法恢复，不会把被清空的事实当作首次绑定。
 IF TG_OP='DELETE' THEN
   IF EXISTS(SELECT 1 FROM operations WHERE id=OLD.operation_id) THEN
     RAISE EXCEPTION 'DSGA creation binding is immutable' USING ERRCODE='23514';
   END IF;
   RETURN OLD;
 END IF;
 IF NEW.operation_id IS DISTINCT FROM OLD.operation_id OR NEW.namespace IS DISTINCT FROM OLD.namespace
   OR NEW.vm_name IS DISTINCT FROM OLD.vm_name OR NEW.incarnation IS DISTINCT FROM OLD.incarnation
   OR NEW.compute_instance_id IS DISTINCT FROM OLD.compute_instance_id OR NEW.intent_digest IS DISTINCT FROM OLD.intent_digest
   OR NEW.vm_uid IS DISTINCT FROM OLD.vm_uid OR NEW.created_at IS DISTINCT FROM OLD.created_at
   OR (OLD.config_uid IS NOT NULL AND NEW.config_uid IS DISTINCT FROM OLD.config_uid) THEN
   RAISE EXCEPTION 'DSGA creation binding is immutable' USING ERRCODE='23514';
 END IF;
 IF NEW.config_uid IS NOT DISTINCT FROM OLD.config_uid THEN NEW.updated_at := OLD.updated_at; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_creation_binding_immutable BEFORE UPDATE OR DELETE ON dsga_creation_bindings
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_creation_binding();

-- 保留042全部到期/回收约束；新DSGA创建也不能绕过将到期分支禁止新endpoint。
CREATE OR REPLACE FUNCTION guard_branch_expiration_operation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE bid text; deadline timestamptz; status text; generation bigint; deleting text;
BEGIN
 IF NEW.action='expire_branch' THEN
   IF NEW.resource_type IS DISTINCT FROM 'branch_lifecycle' OR jsonb_array_length(NEW.payload->'branch_ids')<>1 THEN
     RAISE EXCEPTION 'exact branch expiration scope required' USING ERRCODE='23514';
   END IF;
   bid:=NEW.payload->'branch_ids'->>0;
   PERFORM id FROM projects WHERE id=NEW.project_id AND state='ready' AND deleted_at IS NULL FOR UPDATE;
   IF NOT FOUND THEN RAISE EXCEPTION 'ready expiration parent required' USING ERRCODE='23514'; END IF;
   SELECT expires_at,state,expiration_generation,deletion_operation_id INTO deadline,status,generation,deleting
    FROM branches WHERE id=bid AND project_id=NEW.project_id AND NOT protected AND NOT is_default FOR UPDATE;
   IF deadline IS NULL OR deadline>clock_timestamp() OR NEW.resource_id IS DISTINCT FROM bid OR NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id
     OR generation::text IS DISTINCT FROM NEW.payload->>'expiration_generation'
     OR (TG_OP='INSERT' AND status IS DISTINCT FROM 'ready')
     OR (TG_OP='UPDATE' AND (deleting IS DISTINCT FROM NEW.id OR status NOT IN ('deleting','deleted'))) THEN
     RAISE EXCEPTION 'original due expiration generation required' USING ERRCODE='23514';
   END IF;
   RETURN NEW;
 END IF;
 IF NEW.action IN ('delete_project','delete_branch','delete_endpoint','cleanup_failed_project','cleanup_failed_branch','cleanup_failed_endpoint','recover_project','set_branch_expiration') THEN RETURN NEW; END IF;
 bid:=NULLIF(NEW.payload->>'branch_id','');
 IF NEW.resource_type='endpoint' THEN SELECT branch_id INTO bid FROM endpoints WHERE id=NEW.resource_id AND project_id=NEW.project_id; END IF;
 IF bid IS NULL THEN RETURN NEW; END IF;
 SELECT expires_at INTO deadline FROM branches WHERE id=bid AND project_id=NEW.project_id FOR UPDATE;
 IF deadline IS NOT NULL AND (deadline<=clock_timestamp() OR NEW.action IN ('create_endpoint','create_endpoint_dsga_v1') OR NEW.action LIKE '%function%' OR NEW.action='enable_data_api') THEN
   RAISE EXCEPTION 'branch expiration does not admit this work' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

$original_dsga_058$;
 END IF;
 IF to_regclass('catalog_current_observations') IS NULL THEN
   EXECUTE $original_catalog_058$
-- A new, explicit observation of the original OID at the current intent
-- generation. Earlier baselines/results are never rewritten or backfilled.
ALTER TABLE catalog_object_bindings DROP CONSTRAINT catalog_object_bindings_source_type_check;
ALTER TABLE catalog_object_bindings ADD CONSTRAINT catalog_object_bindings_source_type_check
 CHECK(source_type IN ('initial_observation','create_result','mutation_result','rename_result','rename_follower','inherited_identity','current_observation'));
CREATE TABLE catalog_current_observations (
 id text PRIMARY KEY,
 binding_id text NOT NULL UNIQUE REFERENCES catalog_object_bindings(id),
 baseline_id text NOT NULL REFERENCES catalog_identity_baselines(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 proof jsonb NOT NULL CHECK(jsonb_typeof(proof)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_catalog_current_observation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Current catalog identity observation is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_catalog_current_observation BEFORE UPDATE OR DELETE ON catalog_current_observations FOR EACH ROW EXECUTE FUNCTION preserve_catalog_current_observation();

$original_catalog_058$;
 END IF;
 -- 完整表已存在时保留原OID/行；必要列缺失、类型或可空语义不同不能当作
 -- 完成迁移。允许未来追加列，避免给后续合法前向升级制造历史DDL精确快照锁。
 IF EXISTS(SELECT 1 FROM (VALUES
   ('dsga_creation_bindings','operation_id','text',true),
   ('dsga_creation_bindings','namespace','text',true),
   ('dsga_creation_bindings','vm_name','text',true),
   ('dsga_creation_bindings','incarnation','text',true),
   ('dsga_creation_bindings','compute_instance_id','text',true),
   ('dsga_creation_bindings','intent_digest','bytea',true),
   ('dsga_creation_bindings','vm_uid','text',true),
   ('dsga_creation_bindings','config_uid','text',false),
   ('dsga_creation_bindings','created_at','timestamp with time zone',true),
   ('dsga_creation_bindings','updated_at','timestamp with time zone',true),
   ('catalog_current_observations','id','text',true),
   ('catalog_current_observations','binding_id','text',true),
   ('catalog_current_observations','baseline_id','text',true),
   ('catalog_current_observations','project_id','text',true),
   ('catalog_current_observations','branch_id','text',true),
   ('catalog_current_observations','proof','jsonb',true),
   ('catalog_current_observations','created_at','timestamp with time zone',true)
 ) AS expected(relation,column_name,type_name,not_null)
 WHERE NOT EXISTS(SELECT 1 FROM pg_attribute a
   WHERE a.attrelid=to_regclass(expected.relation) AND a.attname=expected.column_name
     AND NOT a.attisdropped AND a.atttypid=to_regtype(expected.type_name)
     AND a.attnotnull=expected.not_null)) THEN
   RAISE EXCEPTION '058 branch migration has incompatible or partial columns' USING ERRCODE='23514';
 END IF;
 IF EXISTS(SELECT 1 FROM (VALUES
   ('dsga_creation_bindings','dsga_creation_bindings_pkey','p'),
   ('dsga_creation_bindings','dsga_creation_bindings_vm_uid_key','u'),
   ('dsga_creation_bindings','dsga_creation_bindings_config_uid_key','u'),
   ('dsga_creation_bindings','dsga_creation_bindings_operation_id_fkey','f'),
   ('catalog_current_observations','catalog_current_observations_pkey','p'),
   ('catalog_current_observations','catalog_current_observations_binding_id_key','u'),
   ('catalog_current_observations','catalog_current_observations_binding_id_fkey','f'),
   ('catalog_current_observations','catalog_current_observations_baseline_id_fkey','f'),
   ('catalog_current_observations','catalog_current_observations_project_id_fkey','f'),
   ('catalog_current_observations','catalog_current_observations_branch_id_fkey','f')
 ) AS expected(relation,constraint_name,kind)
 WHERE NOT EXISTS(SELECT 1 FROM pg_constraint c WHERE c.conrelid=to_regclass(expected.relation)
   AND c.conname=expected.constraint_name AND c.contype::text=expected.kind AND c.convalidated)) THEN
   RAISE EXCEPTION '058 branch migration is missing original identity constraints' USING ERRCODE='23514';
 END IF;
 IF EXISTS(SELECT 1 FROM (VALUES
   ('operations','dsga_creation_intent_immutable','guard_dsga_creation_intent()'),
   ('dsga_creation_bindings','dsga_creation_binding_immutable','guard_dsga_creation_binding()'),
   ('catalog_current_observations','preserve_catalog_current_observation','preserve_catalog_current_observation()')
 ) AS expected(relation,trigger_name,function_name)
 WHERE NOT EXISTS(SELECT 1 FROM pg_trigger t WHERE t.tgrelid=to_regclass(expected.relation)
   AND t.tgname=expected.trigger_name AND t.tgfoid=to_regprocedure(expected.function_name)
   AND NOT t.tgisinternal AND t.tgenabled='O')) THEN
   RAISE EXCEPTION '058 branch migration is missing original immutable guards' USING ERRCODE='23514';
 END IF;
END $dsga_058_join$;
