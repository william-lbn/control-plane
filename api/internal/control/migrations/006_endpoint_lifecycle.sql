ALTER TABLE endpoints ADD COLUMN IF NOT EXISTS idle_timeout_seconds integer NOT NULL DEFAULT 300;
ALTER TABLE endpoints ADD CONSTRAINT idle_timeout_range CHECK (idle_timeout_seconds BETWEEN 60 AND 3600);
CREATE INDEX IF NOT EXISTS scale_zero_candidates ON endpoints(updated_at)
    WHERE scale_to_zero AND deleted_at IS NULL AND workload_kind = 'neonvm';
