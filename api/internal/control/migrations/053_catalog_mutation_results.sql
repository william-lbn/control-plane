-- A committed ordinary catalog operation is recovered from this immutable
-- receipt, before reading current SQL, credentials, configs or routes.
-- Existing operations are not inferred/backfilled from today's catalog.
CREATE TABLE catalog_mutation_results (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 action text NOT NULL CHECK(action IN ('create_role','rotate_role_password','delete_role','create_database','update_database_owner','delete_database')),
 payload_sha256 text NOT NULL CHECK(payload_sha256 ~ '^[a-f0-9]{64}$'),
 result jsonb NOT NULL CHECK(jsonb_typeof(result)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_catalog_mutation_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Original committed catalog mutation result is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_catalog_mutation_result BEFORE UPDATE OR DELETE ON catalog_mutation_results FOR EACH ROW EXECUTE FUNCTION preserve_catalog_mutation_result();
