-- Current catalog generations are append-only. An initial observation is not
-- historical proof; an acknowledged CREATE has its own original-operation OID.
CREATE TABLE catalog_object_bindings (
 id text PRIMARY KEY,
 identity_id text NOT NULL,
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 kind text NOT NULL CHECK(kind IN ('role','database')),
 name text NOT NULL,
 object_oid bigint NOT NULL CHECK(object_oid BETWEEN 1 AND 4294967295),
 intent_version bigint NOT NULL CHECK(intent_version>0),
 tenant_id text NOT NULL CHECK(tenant_id ~ '^[a-f0-9]{32}$'),
 timeline_id text NOT NULL CHECK(timeline_id ~ '^[a-f0-9]{32}$'),
 state text NOT NULL CHECK(state IN ('ready','deleted')),
 credential_ref text NOT NULL DEFAULT '',
 credential_uid text NOT NULL DEFAULT '',
 owner_oid bigint NOT NULL DEFAULT 0 CHECK(owner_oid BETWEEN 0 AND 4294967295),
 source_type text NOT NULL CHECK(source_type IN ('initial_observation','create_result','mutation_result','rename_result','rename_follower')),
 source_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(branch_id,kind,name,intent_version),
 CHECK((kind='role' AND credential_ref<>'' AND credential_uid<>'' AND owner_oid=0) OR
       (kind='database' AND credential_ref='' AND credential_uid='' AND owner_oid>0))
);
CREATE INDEX catalog_object_identity_history ON catalog_object_bindings(branch_id,identity_id,intent_version);
CREATE TABLE catalog_create_observations (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 payload_sha256 text NOT NULL CHECK(payload_sha256 ~ '^[a-f0-9]{64}$'),
 object_oid bigint NOT NULL CHECK(object_oid BETWEEN 1 AND 4294967295),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_catalog_object_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Original catalog identity generation is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_catalog_object_binding BEFORE UPDATE OR DELETE ON catalog_object_bindings FOR EACH ROW EXECUTE FUNCTION preserve_catalog_object_binding();
CREATE TRIGGER preserve_catalog_create_observation BEFORE UPDATE OR DELETE ON catalog_create_observations FOR EACH ROW EXECUTE FUNCTION preserve_catalog_object_binding();
