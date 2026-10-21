-- Admission captures intent and existing native identity witnesses. No fresh
-- name observation is upgraded into an original identity on the source.
ALTER TABLE catalog_object_bindings DROP CONSTRAINT catalog_object_bindings_source_type_check;
ALTER TABLE catalog_object_bindings ADD CONSTRAINT catalog_object_bindings_source_type_check
 CHECK(source_type IN ('initial_observation','create_result','mutation_result','rename_result','rename_follower','inherited_identity'));
CREATE TABLE catalog_inheritance_sources (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 proof jsonb NOT NULL CHECK(jsonb_typeof(proof)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE catalog_inheritance_observations (
 endpoint_id text PRIMARY KEY REFERENCES endpoints(id),
 operation_id text NOT NULL REFERENCES catalog_inheritance_sources(operation_id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 proof jsonb NOT NULL CHECK(jsonb_typeof(proof)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE catalog_inheritance_exclusions (
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 kind text NOT NULL CHECK(kind IN ('role','database')),
 name text NOT NULL,
 operation_id text NOT NULL REFERENCES catalog_inheritance_sources(operation_id),
 source_binding_id text NOT NULL DEFAULT '',
 expected_oid bigint NOT NULL CHECK(expected_oid BETWEEN 0 AND 4294967295),
 observed_oid bigint NOT NULL CHECK(observed_oid BETWEEN 0 AND 4294967295),
 reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(branch_id,kind,name)
);
CREATE TABLE catalog_inheritance_publications (
 endpoint_id text PRIMARY KEY REFERENCES catalog_inheritance_observations(endpoint_id),
 published_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_catalog_inheritance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Original catalog inheritance witness is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_catalog_inheritance_source BEFORE UPDATE OR DELETE ON catalog_inheritance_sources FOR EACH ROW EXECUTE FUNCTION preserve_catalog_inheritance();
CREATE TRIGGER preserve_catalog_inheritance_observation BEFORE UPDATE OR DELETE ON catalog_inheritance_observations FOR EACH ROW EXECUTE FUNCTION preserve_catalog_inheritance();
CREATE TRIGGER preserve_catalog_inheritance_exclusion BEFORE UPDATE OR DELETE ON catalog_inheritance_exclusions FOR EACH ROW EXECUTE FUNCTION preserve_catalog_inheritance();
CREATE TRIGGER preserve_catalog_inheritance_publication BEFORE UPDATE OR DELETE ON catalog_inheritance_publications FOR EACH ROW EXECUTE FUNCTION preserve_catalog_inheritance();
