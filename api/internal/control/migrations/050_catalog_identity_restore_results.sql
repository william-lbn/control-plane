-- The original native baseline is immutable. Restoration keeps the desired
-- catalog intent/version and records its own durable, exactly-once result.
CREATE TABLE catalog_identity_restore_results (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 binding_id text NOT NULL REFERENCES catalog_identity_baselines(id),
 result jsonb NOT NULL CHECK(jsonb_typeof(result)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
