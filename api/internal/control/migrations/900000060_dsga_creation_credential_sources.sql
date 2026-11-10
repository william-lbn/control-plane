-- 只追加来源事实，不改历史058/前向join；secret内容不写入元数据库。
-- digest为带协议域的完整immutable Secret.Data摘要，UID同名重建不可替换。
CREATE TABLE dsga_creation_credential_sources (
 operation_id text PRIMARY KEY REFERENCES dsga_creation_bindings(operation_id) ON DELETE CASCADE,
 source_version integer NOT NULL CHECK (source_version=1),
 credentials_uid text NOT NULL UNIQUE CHECK (length(credentials_uid) BETWEEN 1 AND 128 AND credentials_uid ~ '^[!-~]+$'),
 content_digest bytea NOT NULL CHECK (octet_length(content_digest)=32),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION guard_dsga_creation_credential_source() RETURNS trigger LANGUAGE plpgsql AS $$
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
 PERFORM id FROM operations WHERE id=NEW.operation_id
   AND action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1') FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA original creation operation required' USING ERRCODE='23514';
 END IF;
 PERFORM operation_id FROM dsga_creation_bindings WHERE operation_id=NEW.operation_id AND config_uid IS NULL FOR SHARE;
 IF NOT FOUND THEN
   RAISE EXCEPTION 'DSGA credential source must precede config binding' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_creation_credential_source_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_creation_credential_sources
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_creation_credential_source();
