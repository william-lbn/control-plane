-- Prospective allocation evidence only. Existing objects are never adopted by
-- bucket listing, age, a missing branch, or a missing current SQL reference.
CREATE TABLE storage_gc_allocations (
 store_id text NOT NULL CHECK(store_id ~ '^[a-f0-9]{64}$'),
 blob_key text NOT NULL CHECK(blob_key ~ '^prj_[a-f0-9]{16,32}/(blob|part)_[a-f0-9]{24}$'),
 allocation_id text NOT NULL UNIQUE CHECK(allocation_id ~ '^sga_[a-f0-9]{24}$'),
 project_id text NOT NULL REFERENCES projects(id),
 size bigint NOT NULL CHECK(size BETWEEN 0 AND 5368709120),
 state text NOT NULL DEFAULT 'uploading' CHECK(state IN ('uploading','may_reference','never_published')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 resolved_at timestamptz,
 PRIMARY KEY(store_id,blob_key),
 CHECK(split_part(blob_key,'/',1)=project_id),
 CHECK((state='uploading')=(resolved_at IS NULL))
);
CREATE INDEX storage_gc_orphans ON storage_gc_allocations(store_id,project_id,created_at,allocation_id) WHERE state='never_published';
CREATE TABLE storage_gc_runs (
 id text PRIMARY KEY CHECK(id ~ '^sgr_[a-f0-9]{24}$'),
 store_id text NOT NULL CHECK(store_id ~ '^[a-f0-9]{64}$'),
 project_id text NOT NULL REFERENCES projects(id),
 actor_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(actor_id,key_hash),
 UNIQUE(id,store_id,project_id)
);
CREATE TABLE storage_gc_items (
 run_id text NOT NULL,
 store_id text NOT NULL,
 project_id text NOT NULL,
 blob_key text NOT NULL,
 state text NOT NULL DEFAULT 'planned' CHECK(state IN ('planned','deleted','absent','held','delete_unknown')),
 reason text NOT NULL DEFAULT '',
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(run_id,blob_key),
 FOREIGN KEY(run_id,store_id,project_id) REFERENCES storage_gc_runs(id,store_id,project_id),
 FOREIGN KEY(store_id,blob_key) REFERENCES storage_gc_allocations(store_id,blob_key),
 CHECK(split_part(blob_key,'/',1)=project_id)
);
CREATE TABLE storage_gc_attempts (
 run_id text NOT NULL,
 blob_key text NOT NULL,
 attempt integer NOT NULL CHECK(attempt>0),
 phase text NOT NULL CHECK(phase IN ('intent','result')),
 actor_id text NOT NULL REFERENCES users(id),
 state text NOT NULL CHECK(state IN ('deleted','absent','held','delete_unknown')),
 reason text NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(run_id,blob_key,attempt,phase),
 FOREIGN KEY(run_id,blob_key) REFERENCES storage_gc_items(run_id,blob_key)
);
CREATE FUNCTION guard_storage_gc_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'storage GC evidence is retained' USING ERRCODE='23514'; END IF;
 IF TG_TABLE_NAME='storage_gc_allocations' THEN
  IF (to_jsonb(NEW)-ARRAY['state','resolved_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['state','resolved_at'])
   OR OLD.state<>'uploading' OR NEW.state NOT IN ('may_reference','never_published') OR NEW.resolved_at IS NULL THEN
   RAISE EXCEPTION 'storage allocation evidence is immutable' USING ERRCODE='23514';
  END IF;
 ELSIF TG_TABLE_NAME IN ('storage_gc_runs','storage_gc_attempts') THEN
  RAISE EXCEPTION 'storage GC plan is immutable' USING ERRCODE='23514';
 ELSIF (to_jsonb(NEW)-ARRAY['state','reason','attempts','updated_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['state','reason','attempts','updated_at']) THEN
  RAISE EXCEPTION 'storage GC item identity is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER storage_gc_allocation_evidence BEFORE UPDATE OR DELETE ON storage_gc_allocations FOR EACH ROW EXECUTE FUNCTION guard_storage_gc_evidence();
CREATE TRIGGER storage_gc_run_evidence BEFORE UPDATE OR DELETE ON storage_gc_runs FOR EACH ROW EXECUTE FUNCTION guard_storage_gc_evidence();
CREATE TRIGGER storage_gc_item_evidence BEFORE UPDATE OR DELETE ON storage_gc_items FOR EACH ROW EXECUTE FUNCTION guard_storage_gc_evidence();
CREATE TRIGGER storage_gc_attempt_evidence BEFORE UPDATE OR DELETE ON storage_gc_attempts FOR EACH ROW EXECUTE FUNCTION guard_storage_gc_evidence();
