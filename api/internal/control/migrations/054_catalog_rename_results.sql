-- Original rename results must survive later renames and credential rotation.
-- Do not infer a legacy result from the current old/new metadata names.
CREATE TABLE catalog_rename_results (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 kind text NOT NULL CHECK(kind IN ('role','database')),
 payload_sha256 text NOT NULL CHECK(payload_sha256 ~ '^[a-f0-9]{64}$'),
 result jsonb NOT NULL CHECK(jsonb_typeof(result)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_catalog_rename_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Original committed catalog rename result is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_catalog_rename_result BEFORE UPDATE OR DELETE ON catalog_rename_results FOR EACH ROW EXECUTE FUNCTION preserve_catalog_rename_result();
