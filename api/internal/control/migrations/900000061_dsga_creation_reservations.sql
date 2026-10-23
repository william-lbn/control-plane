-- 凭据外部写入前，先保留原随机计划及Operation ID；不保存密码/seed。
-- 不改写历史迁移，不放宽既有idempotency_keys的非NULL/FK约束。
CREATE TABLE dsga_creation_reservations (
 actor_id text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 128),
 key_hash text NOT NULL CHECK (key_hash ~ '^[0-9a-f]{64}$'),
 request_hash text NOT NULL CHECK (request_hash ~ '^h1:[0-9a-f]{64}$'),
 scope text NOT NULL CHECK (length(scope) BETWEEN 1 AND 512),
 org_id text NOT NULL REFERENCES organizations(id),
 base_action text NOT NULL CHECK (base_action IN ('create_project','create_branch','create_endpoint')),
 operation_id text NOT NULL UNIQUE CHECK (length(operation_id) BETWEEN 1 AND 128 AND operation_id ~ '^[!-~]+$'),
 draft jsonb NOT NULL CHECK (jsonb_typeof(draft)='object' AND octet_length(draft::text)<=1048576),
 credential_reference jsonb CHECK (credential_reference IS NULL OR (
   jsonb_typeof(credential_reference)='object' AND credential_reference->'protocol_version'='1'::jsonb AND
   jsonb_typeof(credential_reference->'uid')='string' AND length(credential_reference->>'uid') BETWEEN 1 AND 128 AND
   credential_reference->>'uid' ~ '^[!-~]+$' AND credential_reference->>'content_sha256' ~ '^[0-9a-f]{64}$') IS TRUE),
 accepted_id text UNIQUE REFERENCES operations(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 accepted_at timestamptz,
 PRIMARY KEY(actor_id,key_hash),
 CHECK ((accepted_id IS NULL AND accepted_at IS NULL) OR
        (accepted_id=operation_id AND accepted_at IS NOT NULL AND credential_reference IS NOT NULL))
);
CREATE UNIQUE INDEX dsga_reserved_project_name ON dsga_creation_reservations(org_id,lower(draft->>'resource_name'))
 WHERE base_action='create_project' AND accepted_id IS NULL;

-- 两张表都先锁同一actor/key，再读取另一张表。只在本事务内串行同键，hash
-- 碰撞最多导致额外串行，不参与授权。不能只各自检查SELECT不存在，否则新旧
-- API并发可分别在两表插入并都认为自己取得原键。SPI读位于等待之后，READ
-- COMMITTED会看到刚刚提交的赢家；调用方不跨Kube持该锁。
CREATE FUNCTION dsga_lock_creation_key(actor text, key_digest text) RETURNS void LANGUAGE sql AS $$
 SELECT pg_advisory_xact_lock(hashtextextended('neon-dsga-creation-key-v1:'||actor||':'||key_digest,0));
$$;

CREATE FUNCTION guard_dsga_creation_reservation() RETURNS trigger LANGUAGE plpgsql AS $$
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
    NEW.draft#>'{plan,vm,protocol_version}' IS DISTINCT FROM '1'::jsonb OR
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
CREATE TRIGGER dsga_creation_reservation_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_creation_reservations
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_creation_reservation();

CREATE FUNCTION guard_dsga_reserved_idempotency() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r dsga_creation_reservations%ROWTYPE;
BEGIN
 IF TG_OP<>'INSERT' THEN
   IF EXISTS(SELECT 1 FROM dsga_creation_reservations WHERE actor_id=OLD.actor_id AND key_hash=OLD.key_hash) THEN
     RAISE EXCEPTION 'DSGA original idempotency fact is immutable' USING ERRCODE='23514';
   END IF;
   IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 END IF;
 PERFORM dsga_lock_creation_key(NEW.actor_id,NEW.key_hash);
 SELECT * INTO r FROM dsga_creation_reservations WHERE actor_id=NEW.actor_id AND key_hash=NEW.key_hash;
 IF r.operation_id IS NOT NULL AND (NEW.operation_id IS DISTINCT FROM r.operation_id OR
    NEW.request_hash IS DISTINCT FROM r.request_hash OR r.credential_reference IS NULL) THEN
   RAISE EXCEPTION 'idempotency key belongs to original DSGA reservation' USING ERRCODE='23505';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_reserved_idempotency_guard BEFORE INSERT OR UPDATE OR DELETE ON idempotency_keys
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_reserved_idempotency();
