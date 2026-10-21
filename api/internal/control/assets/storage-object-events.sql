CREATE TABLE IF NOT EXISTS neon_storage.object_events (
 id text PRIMARY KEY CHECK(id ~ '^soe_[a-f0-9]{32}$'),
 project_id text NOT NULL,
 branch_id text NOT NULL,
 timeline_id text NOT NULL CHECK(timeline_id ~ '^[a-f0-9]{32}$'),
 envelope jsonb NOT NULL CHECK(jsonb_typeof(envelope)='object' AND octet_length(envelope::text)<=65536),
 mac text NOT NULL CHECK(mac ~ '^[a-f0-9]{64}$'),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','exported','rejected')),
 error_code text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 exported_at timestamptz
);
CREATE INDEX IF NOT EXISTS object_events_pending ON neon_storage.object_events(branch_id,created_at,id) WHERE state='pending';
