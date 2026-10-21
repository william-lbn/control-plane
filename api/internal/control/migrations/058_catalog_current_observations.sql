-- A new, explicit observation of the original OID at the current intent
-- generation. Earlier baselines/results are never rewritten or backfilled.
ALTER TABLE catalog_object_bindings DROP CONSTRAINT catalog_object_bindings_source_type_check;
ALTER TABLE catalog_object_bindings ADD CONSTRAINT catalog_object_bindings_source_type_check
 CHECK(source_type IN ('initial_observation','create_result','mutation_result','rename_result','rename_follower','inherited_identity','current_observation'));
CREATE TABLE catalog_current_observations (
 id text PRIMARY KEY,
 binding_id text NOT NULL UNIQUE REFERENCES catalog_object_bindings(id),
 baseline_id text NOT NULL REFERENCES catalog_identity_baselines(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 proof jsonb NOT NULL CHECK(jsonb_typeof(proof)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_catalog_current_observation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Current catalog identity observation is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_catalog_current_observation BEFORE UPDATE OR DELETE ON catalog_current_observations FOR EACH ROW EXECUTE FUNCTION preserve_catalog_current_observation();
