-- Public certificate observations only: no leaf keys, HMAC keys or bootstrap
-- edits. Every observation remains bound to the original runtime identities.
CREATE TABLE function_manager_identities (
 instance_id text NOT NULL,
 function_id text NOT NULL,
 deployment_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 boot_id text NOT NULL CHECK(boot_id ~ '^[a-f0-9]{32}$'),
 vm_uid text NOT NULL CHECK(vm_uid ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 service_uid text NOT NULL CHECK(service_uid ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 bootstrap_uid text NOT NULL CHECK(bootstrap_uid ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 fingerprint text NOT NULL CHECK(fingerprint ~ '^[0-9a-f]{64}$'),
 not_before timestamptz NOT NULL,
 not_after timestamptz NOT NULL CHECK(not_after>not_before AND not_after-not_before<=interval '4 hours 1 minute'),
 renewals bigint NOT NULL CHECK(renewals>=0 AND renewals<=2147483648),
 first_verified_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 last_verified_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(instance_id,fingerprint),
 UNIQUE(instance_id,renewals),
 FOREIGN KEY(instance_id,function_id,deployment_id,project_id,branch_id)
  REFERENCES function_instances(id,function_id,deployment_id,project_id,branch_id)
);
CREATE FUNCTION guard_function_manager_identity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE i function_instances; b function_instance_bootstraps;
BEGIN
 IF TG_OP='UPDATE' AND (to_jsonb(NEW)-'last_verified_at') IS DISTINCT FROM (to_jsonb(OLD)-'last_verified_at') THEN
  RAISE EXCEPTION 'function manager identity history is immutable' USING ERRCODE='23514';
 END IF;
 SELECT * INTO STRICT i FROM function_instances WHERE id=NEW.instance_id;
 SELECT * INTO STRICT b FROM function_instance_bootstraps WHERE instance_id=NEW.instance_id;
 IF i.boot_id IS DISTINCT FROM NEW.boot_id OR i.vm_uid IS DISTINCT FROM NEW.vm_uid OR i.service_uid IS DISTINCT FROM NEW.service_uid OR b.secret_uid IS DISTINCT FROM NEW.bootstrap_uid THEN
  RAISE EXCEPTION 'function manager identity scope rejected' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER function_manager_identity_guard BEFORE INSERT OR UPDATE ON function_manager_identities
 FOR EACH ROW EXECUTE FUNCTION guard_function_manager_identity();
