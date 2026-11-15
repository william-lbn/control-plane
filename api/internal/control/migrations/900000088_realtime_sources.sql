CREATE UNIQUE INDEX branches_realtime_identity ON branches(id,project_id,timeline_id);

-- The source ID identifies one generation of its publication and slot. A
-- replacement source uses a new ID and slot, so an old cursor cannot resume it.
CREATE TABLE realtime_sources (
 id text PRIMARY KEY CHECK(id ~ '^rts_[0-9a-f]{24}$'),
 project_id text NOT NULL,
 branch_id text NOT NULL,
 timeline_id text NOT NULL,
 database_name text NOT NULL CHECK(octet_length(database_name) BETWEEN 1 AND 63),
 system_id text NOT NULL CHECK(system_id ~ '^[0-9]{1,20}$'),
 pg_timeline integer NOT NULL CHECK(pg_timeline > 0),
 slot_name text NOT NULL CHECK(slot_name ~ '^[a-z0-9_]{1,63}$'),
 publication text NOT NULL CHECK(publication ~ '^[a-z0-9_]{1,63}$'),
 relations jsonb NOT NULL CHECK(jsonb_typeof(relations)='array' AND octet_length(relations::text)<=65536),
 confirmed_lsn pg_lsn NOT NULL CHECK(confirmed_lsn > '0/0'),
 retain_commits integer NOT NULL CHECK(retain_commits > 0),
 latest bigint NOT NULL DEFAULT 0 CHECK(latest >= 0),
 lease_epoch bigint NOT NULL DEFAULT 0 CHECK(lease_epoch >= 0),
 lease_until timestamptz,
 retired_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(branch_id,project_id,timeline_id) REFERENCES branches(id,project_id,timeline_id),
 UNIQUE(project_id,branch_id,slot_name),
 UNIQUE(project_id,branch_id,database_name,publication),
 CHECK(retired_at IS NULL OR lease_until IS NULL)
);

CREATE UNIQUE INDEX realtime_one_source_per_database ON realtime_sources(project_id,branch_id,database_name)
 WHERE retired_at IS NULL;

CREATE TABLE realtime_invalidations (
 source_id text NOT NULL REFERENCES realtime_sources(id) ON DELETE CASCADE,
 position bigint NOT NULL CHECK(position > 0),
 commit_lsn pg_lsn NOT NULL,
 end_lsn pg_lsn NOT NULL CHECK(end_lsn > commit_lsn),
 relations jsonb NOT NULL CHECK(jsonb_typeof(relations)='array' AND octet_length(relations::text)<=65536),
 PRIMARY KEY(source_id,position)
);
