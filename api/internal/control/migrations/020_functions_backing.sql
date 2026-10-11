-- Forward-only private ownership receipt. Existing deployment history is not
-- fabricated/backfilled; an absent receipt must block future Driver execution.
-- No environment values, code, credential material or public URL live here.
CREATE TABLE function_deployment_backings (
 deployment_id text PRIMARY KEY,
 function_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 environment_namespace text NOT NULL CHECK(environment_namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
 environment_secret_ref text NOT NULL,
 environment_secret_uid text NOT NULL CHECK(environment_secret_uid ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 environment_mac text NOT NULL CHECK(environment_mac ~ '^[0-9a-f]{64}$'),
 mac_key_version text NOT NULL CHECK(mac_key_version ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(deployment_id,function_id,project_id,branch_id)
  REFERENCES function_deployments(id,function_id,project_id,branch_id),
 CHECK(environment_secret_ref='fdp-'||substring(deployment_id from 5)||'-environment')
);

CREATE FUNCTION guard_function_deployment_backing() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE d function_deployments%ROWTYPE;
BEGIN
 IF TG_OP='UPDATE' AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'function backing receipt is immutable' USING ERRCODE='23514';
 END IF;
 SELECT * INTO d FROM function_deployments WHERE id=NEW.deployment_id;
 IF NOT FOUND OR d.function_id<>NEW.function_id OR d.project_id<>NEW.project_id
  OR d.branch_id<>NEW.branch_id OR d.environment_secret_ref<>NEW.environment_secret_ref
  OR d.artifact_key<>('functions/'||NEW.project_id||'/'||NEW.branch_id||'/sha256/'||d.bundle_digest||'.zip') THEN
  RAISE EXCEPTION 'function backing receipt scope mismatch' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER function_deployment_backing_guard BEFORE INSERT OR UPDATE ON function_deployment_backings
 FOR EACH ROW EXECUTE FUNCTION guard_function_deployment_backing();
