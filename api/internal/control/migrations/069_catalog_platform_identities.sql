-- Explicit first observation of protected platform/default identities. A row
-- records identity only from its capture time, never fabricated older history.
-- Native identities remain independent from current Endpoint credential UIDs.
CREATE TABLE catalog_platform_identity_baselines (
 id text PRIMARY KEY CHECK(id ~ '^cps_[a-f0-9]{24}$'),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 tenant_id text NOT NULL CHECK(tenant_id ~ '^[a-f0-9]{32}$'),
 timeline_id text NOT NULL CHECK(timeline_id ~ '^[a-f0-9]{32}$'),
 endpoint_id text NOT NULL REFERENCES endpoints(id),
 credential_ref text NOT NULL,
 credential_uid text NOT NULL,
 config_uid text NOT NULL,
 observation jsonb NOT NULL CHECK(jsonb_typeof(observation)='object' AND octet_length(observation::text)<=16384),
 observation_sha256 text NOT NULL CHECK(observation_sha256 ~ '^[a-f0-9]{64}$'),
 captured_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(branch_id,tenant_id,timeline_id)
);
CREATE TRIGGER catalog_platform_identity_immutable BEFORE UPDATE OR DELETE ON catalog_platform_identity_baselines FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();

-- Original SQL receipts remain immutable. A dormant name-only migration signs
-- a new scope using the same private login bytes and retains all older receipts.
ALTER TABLE function_definitions ADD COLUMN sql_credential_generation bigint NOT NULL DEFAULT 1 CHECK(sql_credential_generation>0);
ALTER TABLE function_sql_credentials DROP CONSTRAINT function_sql_credentials_credential_generation_check;
ALTER TABLE function_sql_credentials ADD CONSTRAINT function_sql_credentials_credential_generation_check CHECK(credential_generation>0);

CREATE OR REPLACE FUNCTION guard_function_definition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE original jsonb; item jsonb;
BEGIN
 IF TG_OP='INSERT' AND NEW.sql_credential_generation<>1 THEN
  RAISE EXCEPTION 'new function must begin with original SQL generation' USING ERRCODE='23514';
 END IF;
 IF EXISTS(SELECT 1 FROM operations op WHERE op.project_id=NEW.project_id AND op.resource_type='branch_catalog'
  AND op.resource_id=NEW.branch_id AND op.action='rename_database' AND op.state IN ('queued','running','retry_wait')
  AND op.payload->'backend_dependencies' IS NOT NULL
  AND op.id IS DISTINCT FROM current_setting('neon_control.catalog_backend_rename_operation',true)) THEN
  RAISE EXCEPTION 'original Backend name migration is active' USING ERRCODE='23514';
 END IF;
 IF TG_OP='UPDATE' AND ROW(NEW.id,NEW.org_id,NEW.project_id,NEW.branch_id,NEW.slug,NEW.sql_schema,NEW.sql_role)
  IS DISTINCT FROM ROW(OLD.id,OLD.org_id,OLD.project_id,OLD.branch_id,OLD.slug,OLD.sql_schema,OLD.sql_role) THEN
  RAISE EXCEPTION 'function identity is immutable' USING ERRCODE='23514';
 END IF;
 IF TG_OP='UPDATE' AND ROW(NEW.database_name,NEW.sql_credential_generation) IS DISTINCT FROM ROW(OLD.database_name,OLD.sql_credential_generation) THEN
  SELECT payload INTO original FROM operations WHERE id=current_setting('neon_control.catalog_backend_rename_operation',true)
   AND project_id=OLD.project_id AND resource_type='branch_catalog' AND resource_id=OLD.branch_id AND action='rename_database' AND state='running'
   AND payload->>'name'=OLD.database_name AND payload->>'new_name'=NEW.database_name AND payload->>'branch_id'=OLD.branch_id;
  SELECT v INTO item FROM jsonb_array_elements(original->'backend_dependencies'->'items') v WHERE v->>'kind'='function' AND v->>'id'=OLD.id;
  IF original IS NULL OR item IS NULL OR OLD.state<>'suspended' OR NEW.state<>'suspended'
   OR NEW.sql_credential_generation<>OLD.sql_credential_generation+1 OR (item->>'generation')::bigint<>OLD.sql_credential_generation
   OR item->>'endpoint'<>OLD.endpoint_id OR NEW.endpoint_id IS DISTINCT FROM OLD.endpoint_id
   OR NEW.version<>OLD.version+1 OR NEW.generation<>OLD.generation
   OR EXISTS(SELECT 1 FROM function_instances WHERE function_id=OLD.id AND state<>'retired') THEN
   RAISE EXCEPTION 'explicit dormant original database migration required' USING ERRCODE='23514';
  END IF;
 END IF;
 IF TG_OP='UPDATE' AND (NEW.version<OLD.version OR NEW.generation<OLD.generation) THEN
  RAISE EXCEPTION 'function version cannot regress' USING ERRCODE='23514';
 END IF;
 IF TG_OP='UPDATE' AND OLD.state='deleted' AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'retired function definition is immutable' USING ERRCODE='23514';
 END IF;
 PERFORM pg_advisory_xact_lock(794210061);
 IF NEW.state<>'deleted' AND (SELECT count(*) FROM function_definitions WHERE id<>NEW.id AND state<>'deleted')>=8 THEN
  RAISE EXCEPTION 'function definition budget exhausted' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION check_function_renamed_sql_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM function_sql_credentials c WHERE c.function_id=NEW.id
  AND c.credential_generation=NEW.sql_credential_generation AND c.project_id=NEW.project_id AND c.branch_id=NEW.branch_id
  AND c.writer_id=NEW.endpoint_id AND c.database_name=NEW.database_name AND c.sql_schema=NEW.sql_schema) THEN
  RAISE EXCEPTION 'renamed Function SQL scope must retain an immutable credential receipt' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER function_renamed_sql_scope AFTER UPDATE ON function_definitions DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW WHEN (OLD.database_name IS DISTINCT FROM NEW.database_name OR OLD.sql_credential_generation IS DISTINCT FROM NEW.sql_credential_generation)
 EXECUTE FUNCTION check_function_renamed_sql_scope();
