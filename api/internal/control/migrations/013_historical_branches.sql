-- Point-in-time restore creates a new resource identity. The chosen LSN is
-- already retained in parent_lsn; timestamp is provenance, never re-resolved.
ALTER TABLE branches ADD COLUMN restore_source text NOT NULL DEFAULT 'current'
 CHECK (restore_source IN ('current','timestamp','lsn'));
ALTER TABLE branches ADD COLUMN parent_timestamp timestamptz;
ALTER TABLE branches ADD CONSTRAINT branch_restore_timestamp_consistent
 CHECK ((restore_source='timestamp')=(parent_timestamp IS NOT NULL));
