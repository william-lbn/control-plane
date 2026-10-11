-- Immutable private SQL credential identity. Credential bytes stay in the
-- original Kubernetes Secret, never in this receipt. This migration does not
-- enable a Function Driver, publish a route or rotate a running guest's login.
CREATE TABLE function_sql_credentials (
 function_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 credential_generation bigint NOT NULL CHECK(credential_generation=1),
 writer_id text NOT NULL,
 database_name text NOT NULL,
 sql_schema text NOT NULL,
 provisioning_role text NOT NULL CHECK(provisioning_role='control_probe'),
 namespace text NOT NULL CHECK(namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
 secret_name text NOT NULL,
 secret_uid text NOT NULL CHECK(secret_uid ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 content_mac text NOT NULL CHECK(content_mac ~ '^[0-9a-f]{64}$'),
 mac_key_version text NOT NULL CHECK(mac_key_version ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
 created_operation_id text NOT NULL REFERENCES operations(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(function_id,credential_generation),
 UNIQUE(namespace,secret_name),
 FOREIGN KEY(function_id,project_id,branch_id) REFERENCES function_definitions(id,project_id,branch_id),
 FOREIGN KEY(writer_id,project_id,branch_id) REFERENCES endpoints(id,project_id,branch_id),
 CHECK(secret_name='fn-'||substring(function_id from 5)||'-sql-'||credential_generation::text)
);

CREATE FUNCTION guard_function_sql_credential() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'function SQL credential receipt is immutable' USING ERRCODE='23514';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM function_definitions f WHERE f.id=NEW.function_id
  AND f.project_id=NEW.project_id AND f.branch_id=NEW.branch_id AND f.endpoint_id=NEW.writer_id
  AND f.database_name=NEW.database_name AND f.sql_schema=NEW.sql_schema
  AND f.sql_role='fn_'||substring(NEW.function_id from 5)) THEN
  RAISE EXCEPTION 'function SQL credential scope mismatch' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER function_sql_credential_guard BEFORE INSERT OR UPDATE ON function_sql_credentials
 FOR EACH ROW EXECUTE FUNCTION guard_function_sql_credential();
