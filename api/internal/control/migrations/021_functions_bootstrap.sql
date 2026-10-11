-- Private immutable bootstrap identity only. No credential values, guessed
-- historical UIDs, active routes or execution admission are introduced.
CREATE UNIQUE INDEX function_instance_backing_identity
 ON function_instances(id,function_id,deployment_id,project_id,branch_id);

CREATE TABLE function_instance_bootstraps (
 instance_id text PRIMARY KEY,
 function_id text NOT NULL,
 deployment_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 namespace text NOT NULL CHECK(namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
 secret_name text NOT NULL,
 secret_uid text NOT NULL CHECK(secret_uid ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 content_mac text NOT NULL CHECK(content_mac ~ '^[0-9a-f]{64}$'),
 mac_key_version text NOT NULL CHECK(mac_key_version ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,function_id,deployment_id,project_id,branch_id)
  REFERENCES function_instances(id,function_id,deployment_id,project_id,branch_id),
 CHECK(secret_name='fn-'||substring(instance_id from 5)||'-bootstrap')
);

CREATE FUNCTION guard_function_instance_bootstrap() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'function bootstrap receipt is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER function_instance_bootstrap_guard BEFORE INSERT OR UPDATE ON function_instance_bootstraps
 FOR EACH ROW EXECUTE FUNCTION guard_function_instance_bootstrap();
