-- Forward-only Functions metadata foundation. This migration does not enable
-- a public API, execute customer code, or create a VM. Secret values, code and
-- invocation bodies belong to their dedicated immutable backing resources.
CREATE UNIQUE INDEX projects_identity_org ON projects(id,org_id);
CREATE UNIQUE INDEX endpoints_identity_branch ON endpoints(id,project_id,branch_id);

CREATE TABLE function_definitions (
 id text PRIMARY KEY CHECK(id ~ '^fnc_[0-9a-f]{16}$'),
 org_id text NOT NULL REFERENCES organizations(id),
 project_id text NOT NULL,
 branch_id text NOT NULL,
 endpoint_id text NOT NULL,
 slug text NOT NULL CHECK(slug ~ '^[a-z0-9]{1,20}$'),
 database_name text NOT NULL CHECK(database_name ~ '^[a-zA-Z_][a-zA-Z0-9_]{0,62}$'),
 sql_schema text NOT NULL CHECK(sql_schema ~ '^[a-zA-Z_][a-zA-Z0-9_]{0,62}$'),
 sql_role text NOT NULL CHECK(sql_role ~ '^fn_[0-9a-f]{16}$'),
 version bigint NOT NULL DEFAULT 1 CHECK(version > 0),
 generation bigint NOT NULL DEFAULT 1 CHECK(generation > 0),
 state text NOT NULL CHECK(state IN ('provisioning','active','suspended','degraded','deleting','deleted')),
 active_deployment_id text,
 target_deployment_id text,
 idle_timeout_seconds integer NOT NULL DEFAULT 60 CHECK(idle_timeout_seconds BETWEEN 30 AND 3600),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 deleted_at timestamptz,
 UNIQUE(id,project_id,branch_id),
 UNIQUE(project_id,branch_id,slug),
 UNIQUE(project_id,branch_id,sql_role),
 FOREIGN KEY(project_id,org_id) REFERENCES projects(id,org_id),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id),
 FOREIGN KEY(endpoint_id,project_id,branch_id) REFERENCES endpoints(id,project_id,branch_id),
 CHECK((state='deleted') = (deleted_at IS NOT NULL)),
 CHECK(state<>'active' OR active_deployment_id IS NOT NULL)
);

CREATE TABLE function_deployments (
 id text PRIMARY KEY CHECK(id ~ '^fdp_[0-9a-f]{16}$'),
 function_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 runtime text NOT NULL CHECK(runtime='nodejs24'),
 bundle_digest text NOT NULL CHECK(bundle_digest ~ '^[0-9a-f]{64}$'),
 bundle_bytes integer NOT NULL CHECK(bundle_bytes BETWEEN 1 AND 8388608),
 entry text NOT NULL CHECK(entry IN ('index.mjs','index.js')),
 artifact_key text NOT NULL CHECK(length(artifact_key) BETWEEN 1 AND 240),
 environment_secret_ref text NOT NULL CHECK(environment_secret_ref ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
 environment_names text[] NOT NULL DEFAULT '{}' CHECK(cardinality(environment_names)<=32),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','building','completed','failed')),
 actor_id text NOT NULL REFERENCES users(id),
 operation_id text NOT NULL UNIQUE REFERENCES operations(id),
 failure_code text CHECK(failure_code IS NULL OR failure_code ~ '^[a-z][a-z0-9_]{0,79}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 UNIQUE(id,function_id,project_id,branch_id),
 FOREIGN KEY(function_id,project_id,branch_id) REFERENCES function_definitions(id,project_id,branch_id),
 CHECK((state IN ('completed','failed')) = (finished_at IS NOT NULL)),
 CHECK(state='failed' OR failure_code IS NULL)
);
ALTER TABLE function_definitions ADD CONSTRAINT functions_active_deployment_scope
 FOREIGN KEY(active_deployment_id,id,project_id,branch_id) REFERENCES function_deployments(id,function_id,project_id,branch_id);
ALTER TABLE function_definitions ADD CONSTRAINT functions_target_deployment_scope
 FOREIGN KEY(target_deployment_id,id,project_id,branch_id) REFERENCES function_deployments(id,function_id,project_id,branch_id);
CREATE INDEX function_deployments_history ON function_deployments(function_id,created_at DESC,id);

CREATE TABLE function_instances (
 id text PRIMARY KEY CHECK(id ~ '^fni_[0-9a-f]{16}$'),
 function_id text NOT NULL,
 deployment_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 generation bigint NOT NULL CHECK(generation>0),
 vm_name text NOT NULL UNIQUE CHECK(vm_name ~ '^fn-[0-9a-f]{16}$'),
 vm_uid text CHECK(vm_uid IS NULL OR vm_uid ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 service_name text NOT NULL UNIQUE CHECK(service_name ~ '^fn-[0-9a-f]{16}$'),
 service_uid text CHECK(service_uid IS NULL OR service_uid ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 bootstrap_secret_ref text NOT NULL UNIQUE CHECK(bootstrap_secret_ref ~ '^fn-[0-9a-f]{16}-bootstrap$'),
 boot_id text CHECK(boot_id IS NULL OR boot_id ~ '^[0-9a-f]{32}$'),
 state text NOT NULL CHECK(state IN ('provisioning','starting','ready','draining','failed','retired')),
 last_invoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 retired_at timestamptz,
 FOREIGN KEY(deployment_id,function_id,project_id,branch_id) REFERENCES function_deployments(id,function_id,project_id,branch_id),
 CHECK(vm_name='fn-'||substring(id from 5) AND service_name=vm_name AND bootstrap_secret_ref=vm_name||'-bootstrap'),
 CHECK((state='retired') = (retired_at IS NOT NULL)),
 CHECK(state NOT IN ('ready','draining') OR (boot_id IS NOT NULL AND vm_uid IS NOT NULL AND service_uid IS NOT NULL))
);
CREATE UNIQUE INDEX one_ready_function_instance ON function_instances(function_id) WHERE state='ready';
CREATE UNIQUE INDEX one_function_candidate ON function_instances(function_id) WHERE state IN ('provisioning','starting');
CREATE UNIQUE INDEX one_pending_function_operation ON operations(resource_id)
 WHERE resource_type='function' AND state IN ('queued','running','retry_wait');

CREATE FUNCTION guard_function_definition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND ROW(NEW.id,NEW.org_id,NEW.project_id,NEW.branch_id,NEW.slug,NEW.database_name,NEW.sql_schema,NEW.sql_role)
  IS DISTINCT FROM ROW(OLD.id,OLD.org_id,OLD.project_id,OLD.branch_id,OLD.slug,OLD.database_name,OLD.sql_schema,OLD.sql_role) THEN
  RAISE EXCEPTION 'function identity is immutable' USING ERRCODE='23514';
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
CREATE TRIGGER function_definition_guard BEFORE INSERT OR UPDATE ON function_definitions FOR EACH ROW EXECUTE FUNCTION guard_function_definition();

CREATE FUNCTION guard_function_deployment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE item text; previous text;
BEGIN
 IF TG_OP='UPDATE' AND ROW(NEW.id,NEW.function_id,NEW.project_id,NEW.branch_id,NEW.runtime,NEW.bundle_digest,NEW.bundle_bytes,NEW.entry,NEW.artifact_key,NEW.environment_secret_ref,NEW.environment_names,NEW.actor_id,NEW.operation_id,NEW.created_at)
  IS DISTINCT FROM ROW(OLD.id,OLD.function_id,OLD.project_id,OLD.branch_id,OLD.runtime,OLD.bundle_digest,OLD.bundle_bytes,OLD.entry,OLD.artifact_key,OLD.environment_secret_ref,OLD.environment_names,OLD.actor_id,OLD.operation_id,OLD.created_at) THEN
  RAISE EXCEPTION 'function deployment content is immutable' USING ERRCODE='23514';
 END IF;
 IF TG_OP='UPDATE' AND ((OLD.state IN ('completed','failed') AND NEW IS DISTINCT FROM OLD) OR (OLD.state='building' AND NEW.state='pending')) THEN
  RAISE EXCEPTION 'function deployment state cannot regress' USING ERRCODE='23514';
 END IF;
 FOREACH item IN ARRAY NEW.environment_names LOOP
  IF item IS NULL OR item !~ '^[a-zA-Z_][a-zA-Z0-9_]{0,63}$' OR (previous IS NOT NULL AND previous COLLATE "C">=item COLLATE "C") THEN
   RAISE EXCEPTION 'function environment names must be unique and sorted' USING ERRCODE='23514';
  END IF;
  previous:=item;
 END LOOP;
 RETURN NEW;
END $$;
CREATE TRIGGER function_deployment_guard BEFORE INSERT OR UPDATE ON function_deployments FOR EACH ROW EXECUTE FUNCTION guard_function_deployment();

-- Capacity includes failed/draining VMs. Release it only after the Driver has
-- observed the original VM UID and all owned Runner Pods absent. A process
-- restart, lease timeout or a failed health request is not release proof.
CREATE FUNCTION guard_function_instance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND ROW(NEW.id,NEW.function_id,NEW.deployment_id,NEW.project_id,NEW.branch_id,NEW.generation,NEW.vm_name,NEW.service_name,NEW.bootstrap_secret_ref,NEW.created_at)
  IS DISTINCT FROM ROW(OLD.id,OLD.function_id,OLD.deployment_id,OLD.project_id,OLD.branch_id,OLD.generation,OLD.vm_name,OLD.service_name,OLD.bootstrap_secret_ref,OLD.created_at) THEN
  RAISE EXCEPTION 'function instance identity is immutable' USING ERRCODE='23514';
 END IF;
 IF TG_OP='UPDATE' AND ((OLD.vm_uid IS NOT NULL AND NEW.vm_uid IS DISTINCT FROM OLD.vm_uid) OR
   (OLD.service_uid IS NOT NULL AND NEW.service_uid IS DISTINCT FROM OLD.service_uid) OR
   (OLD.boot_id IS NOT NULL AND NEW.boot_id IS DISTINCT FROM OLD.boot_id) OR
   (OLD.state='retired' AND NEW.state<>'retired')) THEN
  RAISE EXCEPTION 'function instance ownership cannot change' USING ERRCODE='23514';
 END IF;
 PERFORM pg_advisory_xact_lock(794210062);
 IF NEW.state<>'retired' AND (SELECT count(*) FROM function_instances WHERE id<>NEW.id AND state<>'retired')>=2 THEN
  RAISE EXCEPTION 'function VM budget exhausted' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER function_instance_guard BEFORE INSERT OR UPDATE ON function_instances FOR EACH ROW EXECUTE FUNCTION guard_function_instance();
