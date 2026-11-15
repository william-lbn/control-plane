CREATE TABLE realtime_source_operations (
 id text PRIMARY KEY CHECK(id ~ '^rts_[0-9a-f]{24}$'),
 project_id text NOT NULL,
 branch_id text NOT NULL,
 timeline_id text NOT NULL,
 writer_id text NOT NULL,
 database_name text NOT NULL CHECK(database_name ~ '^[a-z][a-z0-9_]{0,62}$'),
 operation_id text NOT NULL UNIQUE REFERENCES operations(id),
 state text NOT NULL CHECK(state IN ('registering','ready','retiring','retired')),
 slot_create_requested boolean NOT NULL DEFAULT false,
 credential_uid text CHECK(credential_uid ~ '^[a-f0-9-]{36}$'),
 FOREIGN KEY(branch_id,project_id,timeline_id) REFERENCES branches(id,project_id,timeline_id),
 FOREIGN KEY(writer_id,project_id,branch_id) REFERENCES endpoints(id,project_id,branch_id),
 CHECK (NOT slot_create_requested OR credential_uid IS NOT NULL)
);

CREATE UNIQUE INDEX realtime_one_pending_source_per_database
 ON realtime_source_operations(project_id,branch_id,database_name) WHERE state<>'retired';

ALTER TABLE realtime_sources DROP CONSTRAINT realtime_sources_project_id_branch_id_database_name_publica_key;
CREATE UNIQUE INDEX realtime_live_publication
 ON realtime_sources(project_id,branch_id,database_name,publication) WHERE retired_at IS NULL;
