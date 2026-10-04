ALTER TABLE endpoints ADD COLUMN IF NOT EXISTS endpoint_type text NOT NULL DEFAULT 'read_write';
ALTER TABLE endpoints ADD CONSTRAINT endpoint_type_valid CHECK (endpoint_type IN ('read_write', 'read_only'));
DROP INDEX IF EXISTS one_writer_per_branch;
CREATE UNIQUE INDEX one_writer_per_branch ON endpoints(branch_id)
    WHERE deleted_at IS NULL AND endpoint_type = 'read_write';
CREATE INDEX IF NOT EXISTS live_endpoints_by_branch_type
    ON endpoints(branch_id, endpoint_type) WHERE deleted_at IS NULL;
