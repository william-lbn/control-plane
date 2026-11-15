CREATE TABLE realtime_source_runtime (
 source_id text PRIMARY KEY,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 writer_id text NOT NULL,
 credential_ref text NOT NULL CHECK(credential_ref ~ '^neon-rt-[0-9a-f]{24}$'),
 credential_uid text NOT NULL CHECK(credential_uid ~ '^[a-f0-9-]{36}$'),
 max_wal_bytes bigint NOT NULL CHECK(max_wal_bytes>0),
 max_frame_bytes integer NOT NULL CHECK(max_frame_bytes BETWEEN 1 AND 16777216),
 FOREIGN KEY(source_id,project_id,branch_id) REFERENCES realtime_sources(id,project_id,branch_id),
 FOREIGN KEY(writer_id,project_id,branch_id) REFERENCES endpoints(id,project_id,branch_id)
);

CREATE TABLE realtime_demand (
 source_id text NOT NULL REFERENCES realtime_sources(id) ON DELETE CASCADE,
 id text PRIMARY KEY CHECK(id ~ '^rtd_[0-9a-f]{24}$'),
 expires_at timestamptz NOT NULL
);

CREATE INDEX realtime_demand_source_expiry ON realtime_demand(source_id,expires_at);
CREATE INDEX realtime_demand_expiry ON realtime_demand(expires_at);
