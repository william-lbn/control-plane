-- Immutable cross-branch provenance. Code/env bytes remain in their scoped
-- backing resources. A copy is prepared paused, without an execution instance.
CREATE TABLE function_copy_sources (
 deployment_id text PRIMARY KEY,
 function_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 source_function_id text NOT NULL,
 source_branch_id text NOT NULL,
 source_deployment_id text NOT NULL,
 source_version bigint NOT NULL CHECK(source_version>0),
 admitted_actor_id text NOT NULL REFERENCES users(id),
 admitted_operation_id text NOT NULL UNIQUE REFERENCES operations(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(deployment_id,function_id,project_id,branch_id)
  REFERENCES function_deployments(id,function_id,project_id,branch_id),
 FOREIGN KEY(source_deployment_id,source_function_id,project_id,source_branch_id)
  REFERENCES function_deployments(id,function_id,project_id,branch_id),
 CHECK(branch_id<>source_branch_id AND function_id<>source_function_id)
);

CREATE FUNCTION guard_function_copy_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'function copy source is immutable' USING ERRCODE='23514';
 END IF;
 IF TG_OP='INSERT' AND NOT EXISTS(
  SELECT 1 FROM function_deployments source
  JOIN function_deployments target ON target.id=NEW.deployment_id
  JOIN operations op ON op.id=NEW.admitted_operation_id
  WHERE source.id=NEW.source_deployment_id AND source.state='completed'
  AND target.operation_id=op.id AND target.function_id=NEW.function_id
  AND op.project_id=NEW.project_id AND op.resource_id=NEW.function_id
  AND op.resource_type='function' AND op.action='deploy_function'
  AND op.actor_id=NEW.admitted_actor_id
  AND ROW(target.runtime,target.bundle_digest,target.bundle_bytes,target.entry,target.environment_names)
   IS NOT DISTINCT FROM ROW(source.runtime,source.bundle_digest,source.bundle_bytes,source.entry,source.environment_names)
 ) THEN
  RAISE EXCEPTION 'function copy provenance rejected' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER function_copy_source_guard BEFORE INSERT OR UPDATE ON function_copy_sources
 FOR EACH ROW EXECUTE FUNCTION guard_function_copy_source();

-- A branch policy snapshot precedes its first Writer and can survive a failed
-- create. Planned identities are fixed; actual metadata references appear only
-- when that original Writer is ready. No credential values are copied here.
CREATE TABLE function_branch_copy_plans (
 branch_id text NOT NULL,
 project_id text NOT NULL,
 source_branch_id text NOT NULL,
 source_function_id text NOT NULL,
 source_deployment_id text,
 source_version bigint NOT NULL CHECK(source_version>0),
 function_id text NOT NULL UNIQUE CHECK(function_id ~ '^fnc_[a-f0-9]{16}$'),
 trigger_snapshots jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(trigger_snapshots)='array' AND jsonb_array_length(trigger_snapshots)<=16),
 deployment_id text UNIQUE CHECK(deployment_id IS NULL OR deployment_id ~ '^fdp_[a-f0-9]{16}$'),
 operation_id text NOT NULL UNIQUE CHECK(operation_id ~ '^op_[a-f0-9]{16}$'),
 admitted_actor_id text NOT NULL REFERENCES users(id),
 creation_operation_id text NOT NULL REFERENCES operations(id),
 prepared_function_id text,
 prepared_operation_id text REFERENCES operations(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 prepared_at timestamptz,
 last_attempt_at timestamptz,
 last_error_code text CHECK(last_error_code IS NULL OR last_error_code ~ '^[a-z][a-z0-9_]{0,79}$'),
 PRIMARY KEY(branch_id,source_function_id),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id),
 FOREIGN KEY(source_function_id,project_id,source_branch_id)
  REFERENCES function_definitions(id,project_id,branch_id),
 FOREIGN KEY(source_deployment_id,source_function_id,project_id,source_branch_id)
  REFERENCES function_deployments(id,function_id,project_id,branch_id),
 FOREIGN KEY(prepared_function_id,project_id,branch_id)
  REFERENCES function_definitions(id,project_id,branch_id),
 CHECK(branch_id<>source_branch_id),
 CHECK((deployment_id IS NULL)=(source_deployment_id IS NULL)),
 CHECK((prepared_function_id IS NULL)=(prepared_operation_id IS NULL)),
 CHECK((prepared_function_id IS NULL)=(prepared_at IS NULL)),
 CHECK(prepared_function_id IS NULL OR
  (prepared_function_id=function_id AND prepared_operation_id=operation_id))
);

CREATE FUNCTION guard_function_branch_copy_plan() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND (
  ROW(NEW.branch_id,NEW.project_id,NEW.source_branch_id,NEW.source_function_id,NEW.source_deployment_id,
   NEW.source_version,NEW.function_id,NEW.deployment_id,NEW.operation_id,NEW.admitted_actor_id,NEW.creation_operation_id,NEW.created_at,NEW.trigger_snapshots)
  IS DISTINCT FROM
  ROW(OLD.branch_id,OLD.project_id,OLD.source_branch_id,OLD.source_function_id,OLD.source_deployment_id,
   OLD.source_version,OLD.function_id,OLD.deployment_id,OLD.operation_id,OLD.admitted_actor_id,OLD.creation_operation_id,OLD.created_at,OLD.trigger_snapshots)
  OR (OLD.prepared_function_id IS NOT NULL AND NEW IS DISTINCT FROM OLD)
 ) THEN
  RAISE EXCEPTION 'function branch copy plan is immutable' USING ERRCODE='23514';
 END IF;
 IF TG_OP='INSERT' AND NOT EXISTS(
  SELECT 1 FROM function_definitions f JOIN operations o ON o.id=NEW.creation_operation_id
  WHERE f.id=NEW.source_function_id AND f.project_id=NEW.project_id AND f.branch_id=NEW.source_branch_id
  AND f.version=NEW.source_version AND f.state NOT IN ('deleting','deleted')
  AND o.project_id=NEW.project_id AND o.resource_id=NEW.branch_id AND o.resource_type='branch'
  AND o.action='create_branch' AND o.actor_id=NEW.admitted_actor_id
  AND NEW.trigger_snapshots=COALESCE((SELECT jsonb_agg(jsonb_build_object('id',t.id,'type',t.type,'name',t.name,
   'function_path',t.function_path,'cron',t.cron,'bucket_name',t.bucket_name,'key_prefix',t.key_prefix,
   'origin_branch_id',t.origin_branch_id,'source_generation',t.generation) ORDER BY t.id)
   FROM function_triggers t WHERE t.function_id=f.id AND t.project_id=f.project_id AND t.branch_id=f.branch_id AND t.state='ready'),'[]'::jsonb)
  AND ((NEW.source_deployment_id IS NULL AND f.active_deployment_id IS NULL) OR
   EXISTS(SELECT 1 FROM function_deployments d WHERE d.id=NEW.source_deployment_id AND d.id=f.active_deployment_id
    AND d.function_id=f.id AND d.state='completed'))
 ) THEN
  RAISE EXCEPTION 'function branch snapshot source rejected' USING ERRCODE='23514';
 END IF;
 IF NEW.prepared_function_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM function_definitions f JOIN operations o ON o.id=NEW.prepared_operation_id
  WHERE f.id=NEW.prepared_function_id AND f.project_id=NEW.project_id AND f.branch_id=NEW.branch_id
  AND o.project_id=f.project_id AND o.resource_id=f.id AND o.resource_type='function' AND o.actor_id=NEW.admitted_actor_id
  AND ((NEW.source_deployment_id IS NULL AND o.action='create_function' AND f.active_deployment_id IS NULL AND f.target_deployment_id IS NULL) OR
   EXISTS(SELECT 1 FROM function_copy_sources c WHERE c.deployment_id=NEW.deployment_id AND c.function_id=f.id
    AND c.source_deployment_id=NEW.source_deployment_id AND c.source_function_id=NEW.source_function_id
    AND c.source_branch_id=NEW.source_branch_id AND c.source_version=NEW.source_version
    AND c.admitted_operation_id=o.id))
 ) THEN
  RAISE EXCEPTION 'function branch snapshot preparation rejected' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER function_branch_copy_plan_guard BEFORE INSERT OR UPDATE ON function_branch_copy_plans
 FOR EACH ROW EXECUTE FUNCTION guard_function_branch_copy_plan();
