CREATE TABLE realtime_query_operations (
 id text PRIMARY KEY CHECK(id ~ '^rtq_[0-9a-f]{24}$'),
 source_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 writer_id text NOT NULL,
 query_name text NOT NULL CHECK(query_name ~ '^[a-z][a-z0-9_-]{0,47}$'),
 operation_id text NOT NULL UNIQUE REFERENCES operations(id),
 state text NOT NULL CHECK(state IN ('registering','ready','retiring','retired')),
 credential_uid text CHECK(credential_uid ~ '^[a-f0-9-]{36}$'),
 FOREIGN KEY(source_id,project_id,branch_id) REFERENCES realtime_sources(id,project_id,branch_id),
 FOREIGN KEY(writer_id,project_id,branch_id) REFERENCES endpoints(id,project_id,branch_id)
);

CREATE UNIQUE INDEX realtime_one_pending_query_per_name
 ON realtime_query_operations(branch_id,query_name) WHERE state<>'retired';
CREATE INDEX realtime_query_operations_source ON realtime_query_operations(source_id);
