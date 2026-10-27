-- Append-only history evidence. This does not activate a native adapter,
-- install branch SQL instrumentation, or demote any 071 may_reference record.
CREATE TABLE storage_gc_history_evidence (
 id text PRIMARY KEY CHECK(id ~ '^sgh_[a-f0-9]{24}$'),
 baseline_id text,
 store_id text NOT NULL,
 blob_key text NOT NULL,
 project_id text NOT NULL REFERENCES projects(id),
 actor_id text NOT NULL REFERENCES users(id),
 kind text NOT NULL CHECK(kind IN ('baseline','check','intent','result','blocked')),
 outcome text NOT NULL CHECK(outcome IN ('baseline','held','eligible','delete_unknown','deleted','absent')),
 reason text NOT NULL CHECK(reason ~ '^[a-z0-9_]{1,80}$'),
 snapshot jsonb NOT NULL CHECK(jsonb_typeof(snapshot)='object' AND octet_length(snapshot::text)<=2097152),
 snapshot_sha256 text NOT NULL CHECK(snapshot_sha256 ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(id,store_id,blob_key),
 FOREIGN KEY(store_id,blob_key) REFERENCES storage_gc_allocations(store_id,blob_key),
 FOREIGN KEY(baseline_id,store_id,blob_key) REFERENCES storage_gc_history_evidence(id,store_id,blob_key),
 CHECK(split_part(blob_key,'/',1)=project_id),
 CHECK((kind='baseline' AND baseline_id IS NULL) OR kind='blocked' OR (kind IN ('check','intent','result') AND baseline_id IS NOT NULL))
);
CREATE INDEX storage_gc_history_key ON storage_gc_history_evidence(store_id,blob_key,created_at,id);
CREATE FUNCTION guard_storage_gc_history_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Storage GC history proof/unknown outcome evidence is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER storage_gc_history_immutable BEFORE UPDATE OR DELETE ON storage_gc_history_evidence
 FOR EACH ROW EXECUTE FUNCTION guard_storage_gc_history_evidence();
REVOKE ALL ON storage_gc_history_evidence FROM PUBLIC;
