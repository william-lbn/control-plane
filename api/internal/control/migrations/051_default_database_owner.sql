-- The original default database is separate from custom branch_databases.
-- Never replace its arbitrary native comment with a branch management marker.
CREATE TABLE default_database_owners (
 branch_id text PRIMARY KEY REFERENCES branches(id),
 project_id text NOT NULL REFERENCES projects(id),
 database_name text NOT NULL,
 database_oid bigint NOT NULL CHECK(database_oid BETWEEN 1 AND 4294967295),
 tenant_id text NOT NULL CHECK(tenant_id ~ '^[a-f0-9]{32}$'),
 timeline_id text NOT NULL CHECK(timeline_id ~ '^[a-f0-9]{32}$'),
 platform_role text NOT NULL,
 platform_role_oid bigint NOT NULL CHECK(platform_role_oid BETWEEN 1 AND 4294967295),
 initial_owner text NOT NULL,
 initial_owner_oid bigint NOT NULL CHECK(initial_owner_oid BETWEEN 1 AND 4294967295),
 comment_sha256 text NOT NULL CHECK(comment_sha256 ~ '^[a-f0-9]{64}$'),
 database_flags jsonb NOT NULL CHECK(jsonb_typeof(database_flags)='object'),
 mode text NOT NULL CHECK(mode IN ('uninitialized','platform','managed')),
 owner_name text NOT NULL,
 owner_oid bigint NOT NULL CHECK(owner_oid BETWEEN 1 AND 4294967295),
 owner_role_version bigint NOT NULL DEFAULT 0 CHECK(owner_role_version>=0),
 owner_credential_ref text NOT NULL DEFAULT '',
 owner_credential_uid text NOT NULL DEFAULT '',
 version bigint NOT NULL CHECK(version>0),
 state text NOT NULL CHECK(state IN ('ready','updating')),
 current_operation_id text REFERENCES operations(id),
 last_operation_id text REFERENCES operations(id),
 basis text NOT NULL CHECK(basis IN ('admin_initial_observation','live_fork')),
 updated_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='updating' AND current_operation_id IS NOT NULL) OR
       (state='ready' AND current_operation_id IS NULL AND mode<>'uninitialized')),
 CHECK((mode='managed' AND owner_role_version>0 AND owner_credential_ref<>'' AND owner_credential_uid<>'') OR
       (mode IN ('uninitialized','platform') AND owner_role_version=0 AND owner_credential_ref='' AND owner_credential_uid=''))
);
-- No plaintext platform password or verifier enters a payload or public receipt.
-- Purpose-derived AEAD binds the accepted operation, scope, native OID and the
-- original endpoint credential UID. Completion clears the escrow atomically.
CREATE TABLE default_owner_credentials (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 key_version text,
 ciphertext bytea,
 cleared_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((key_version IS NOT NULL AND ciphertext IS NOT NULL AND cleared_at IS NULL) OR
       (key_version IS NULL AND ciphertext IS NULL AND cleared_at IS NOT NULL))
);
CREATE TABLE default_owner_results (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 branch_id text NOT NULL REFERENCES branches(id),
 response jsonb NOT NULL CHECK(jsonb_typeof(response)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Existing lifecycle triggers lock the same parents before retry admission.
-- A failed Owner Operation retains its reservation until the original result
-- commits. An unrelated old Operation cannot bypass that reservation by retry.
CREATE FUNCTION guard_default_owner_retry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE bid text;
BEGIN
 IF NEW.action IN ('update_default_database_owner','restore_catalog_identity') THEN RETURN NEW; END IF;
 IF NEW.resource_type='branch_catalog' THEN bid:=NEW.resource_id;
 ELSIF NEW.action IN ('create_endpoint','delete_endpoint') THEN bid:=NEW.payload->>'branch_id';
 ELSIF NEW.action='create_branch' THEN bid:=NEW.payload->>'parent_branch_id';
 ELSE RETURN NEW; END IF;
 PERFORM id FROM projects WHERE id=NEW.project_id FOR UPDATE;
 PERFORM id FROM branches WHERE id=bid AND project_id=NEW.project_id FOR UPDATE;
 IF EXISTS(SELECT 1 FROM default_database_owners WHERE project_id=NEW.project_id AND branch_id=bid AND state='updating') THEN
  RAISE EXCEPTION 'Original default database Owner Operation must complete first' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER scope_default_owner_retry BEFORE UPDATE OF state ON operations
 FOR EACH ROW WHEN (OLD.state='failed' AND NEW.state='queued') EXECUTE FUNCTION guard_default_owner_retry();
