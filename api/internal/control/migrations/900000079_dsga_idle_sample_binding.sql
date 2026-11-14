ALTER TABLE metric_samples ADD COLUMN dsga_vm_uid text;
ALTER TABLE metric_samples ADD COLUMN dsga_pg_started_at timestamptz;
ALTER TABLE metric_samples ADD COLUMN dsga_pg_sessions bigint;
