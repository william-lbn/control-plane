ALTER TABLE realtime_sources ADD UNIQUE (id,project_id,branch_id);

CREATE TABLE realtime_queries (
 id text PRIMARY KEY CHECK(id ~ '^rtq_[0-9a-f]{24}$'),
 source_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 writer_id text NOT NULL,
 name text NOT NULL CHECK(name ~ '^[a-z][a-z0-9_-]{0,47}$'),
 version bigint NOT NULL DEFAULT 1 CHECK(version > 0),
 statement text NOT NULL CHECK(octet_length(statement) BETWEEN 1 AND 65536),
 relations jsonb NOT NULL CHECK(jsonb_typeof(relations)='array' AND octet_length(relations::text)<=65536),
 parameter_count integer NOT NULL CHECK(parameter_count BETWEEN 0 AND 32),
 read_role text NOT NULL CHECK(read_role ~ '^rtr_[0-9a-f]{24}$'),
 credential_ref text NOT NULL CHECK(credential_ref ~ '^neon-rt-[0-9a-f]{24}$'),
 credential_uid text NOT NULL CHECK(credential_uid ~ '^[a-f0-9-]{36}$'),
 max_rows integer NOT NULL CHECK(max_rows BETWEEN 1 AND 2000),
 max_result_bytes integer NOT NULL CHECK(max_result_bytes BETWEEN 1 AND 1048576),
 max_frame_bytes integer NOT NULL CHECK(max_frame_bytes BETWEEN 1 AND 1048576),
 query_timeout_ms integer NOT NULL CHECK(query_timeout_ms BETWEEN 1 AND 30000),
 created_at timestamptz NOT NULL DEFAULT now(),
 retired_at timestamptz,
 FOREIGN KEY(source_id,project_id,branch_id) REFERENCES realtime_sources(id,project_id,branch_id),
 FOREIGN KEY(writer_id,project_id,branch_id) REFERENCES endpoints(id,project_id,branch_id)
);
CREATE UNIQUE INDEX realtime_query_name_per_branch ON realtime_queries(branch_id,name)
 WHERE retired_at IS NULL;

CREATE FUNCTION guard_realtime_query_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.id,NEW.source_id,NEW.project_id,NEW.branch_id,NEW.writer_id)
      IS DISTINCT FROM ROW(OLD.id,OLD.source_id,OLD.project_id,OLD.branch_id,OLD.writer_id)
    OR NEW.version<OLD.version OR (OLD.retired_at IS NOT NULL AND NEW.retired_at IS DISTINCT FROM OLD.retired_at) THEN
  RAISE EXCEPTION 'realtime query source or revision differs' USING ERRCODE='23514';
 END IF;
 IF (to_jsonb(NEW)-ARRAY['version','retired_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['version','retired_at'])
   AND (NEW.version<=OLD.version OR OLD.retired_at IS NOT NULL) THEN
  RAISE EXCEPTION 'realtime query changes require a new live revision' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER realtime_query_version BEFORE UPDATE ON realtime_queries
 FOR EACH ROW EXECUTE FUNCTION guard_realtime_query_version();
