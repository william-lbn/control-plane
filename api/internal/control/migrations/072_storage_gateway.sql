-- Metadata only: native Storage formats 5/6/7 and immutable blob keys are unchanged.
CREATE TABLE storage_gateway_limits (
 project_id text PRIMARY KEY REFERENCES projects(id),
 generation bigint NOT NULL DEFAULT 1 CHECK(generation>0),
 concurrent_requests integer NOT NULL DEFAULT 4 CHECK(concurrent_requests BETWEEN 1 AND 8),
 daily_requests bigint NOT NULL DEFAULT 0 CHECK(daily_requests BETWEEN 0 AND 1000000000),
 daily_transfer_bytes bigint NOT NULL DEFAULT 0 CHECK(daily_transfer_bytes BETWEEN 0 AND 1125899906842624),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE storage_gateway_days (
 project_id text NOT NULL REFERENCES projects(id), period date NOT NULL,
 requests bigint NOT NULL DEFAULT 0 CHECK(requests>=0),
 observed_bytes bigint NOT NULL DEFAULT 0 CHECK(observed_bytes>=0),
 reserved_bytes bigint NOT NULL DEFAULT 0 CHECK(reserved_bytes>=0),
 PRIMARY KEY(project_id,period)
);
CREATE TABLE storage_gateway_requests (
 id text PRIMARY KEY CHECK(id ~ '^sgr_[a-f0-9]{24}$'),
 project_id text NOT NULL REFERENCES projects(id), branch_id text NOT NULL REFERENCES branches(id),
 period date NOT NULL,
 method text NOT NULL CHECK(method IN ('GET','HEAD','PUT','POST','DELETE','OPTIONS','PATCH')),
 protocol text NOT NULL CHECK(protocol IN ('s3','rest')),
 state text NOT NULL DEFAULT 'admitted' CHECK(state IN ('admitted','completed','interrupted')),
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(), lease_until timestamptz NOT NULL,
 finished_at timestamptz, http_status integer CHECK(http_status BETWEEN 100 AND 599),
 ingress_bytes bigint CHECK(ingress_bytes>=0), egress_bytes bigint CHECK(egress_bytes>=0),
 reserved_bytes bigint NOT NULL CHECK(reserved_bytes>0), duration_ms bigint CHECK(duration_ms>=0),
 CHECK((state='admitted' AND finished_at IS NULL AND http_status IS NULL AND ingress_bytes IS NULL AND egress_bytes IS NULL AND duration_ms IS NULL)
    OR (state<>'admitted' AND finished_at IS NOT NULL AND http_status IS NOT NULL AND ingress_bytes IS NOT NULL AND egress_bytes IS NOT NULL AND duration_ms IS NOT NULL)),
 FOREIGN KEY(project_id,period) REFERENCES storage_gateway_days(project_id,period),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id)
);
CREATE INDEX storage_gateway_requests_branch_time ON storage_gateway_requests(branch_id,started_at DESC);
CREATE INDEX storage_gateway_requests_active ON storage_gateway_requests(project_id,lease_until) WHERE state='admitted';
CREATE FUNCTION storage_gateway_request_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'storage usage receipts are retained'; END IF;
 IF NEW.id<>OLD.id OR NEW.project_id<>OLD.project_id OR NEW.branch_id<>OLD.branch_id
 OR NEW.period<>OLD.period OR NEW.method<>OLD.method OR NEW.protocol<>OLD.protocol
 OR NEW.started_at<>OLD.started_at OR NEW.lease_until<>OLD.lease_until OR NEW.reserved_bytes<>OLD.reserved_bytes
 OR OLD.state<>'admitted' OR NEW.state='admitted' THEN RAISE EXCEPTION 'immutable storage usage receipt'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER storage_gateway_request_guard BEFORE UPDATE OR DELETE ON storage_gateway_requests
 FOR EACH ROW EXECUTE FUNCTION storage_gateway_request_guard();
-- These exact row-lock helpers avoid granting lifecycle-table UPDATE merely for FOR SHARE.
CREATE FUNCTION storage_gateway_lock_project(target text, expected text) RETURNS SETOF text
 LANGUAGE sql SECURITY DEFINER SET search_path FROM CURRENT AS $$
 SELECT p.id FROM projects p JOIN branches b ON b.project_id=p.id
 WHERE b.id=target AND (expected='' OR p.id=expected) AND p.state='ready' AND p.deleted_at IS NULL FOR SHARE OF p
$$;
CREATE FUNCTION storage_gateway_lock_branch(target text, project text) RETURNS SETOF text
 LANGUAGE sql SECURITY DEFINER SET search_path FROM CURRENT AS $$
 SELECT id FROM branches WHERE id=target AND project_id=project AND state='ready' AND deleted_at IS NULL FOR SHARE
$$;
CREATE FUNCTION storage_gateway_lock_instance(target text, project text)
 RETURNS TABLE(endpoint_id text,generation bigint,spec jsonb)
 LANGUAGE sql SECURITY DEFINER SET search_path FROM CURRENT AS $$
 SELECT a.endpoint_id,a.generation,a.spec FROM object_storage_instances a
 JOIN endpoints e ON e.id=a.endpoint_id WHERE a.branch_id=target AND a.project_id=project
 AND a.state='active' AND e.state IN ('active','suspended') AND e.deleted_at IS NULL FOR SHARE OF a
$$;
REVOKE ALL ON FUNCTION storage_gateway_lock_project(text,text),storage_gateway_lock_branch(text,text),storage_gateway_lock_instance(text,text) FROM PUBLIC;
REVOKE ALL ON storage_gateway_limits,storage_gateway_days,storage_gateway_requests FROM PUBLIC;
-- Bind helpers to the installed metadata schema (tests use isolated schemas).
-- Put pg_temp last so callers cannot shadow lifecycle relations with temp tables.
DO $path$
BEGIN
 EXECUTE format('ALTER FUNCTION %I.storage_gateway_lock_project(text,text) SET search_path=pg_catalog,%I,pg_temp',current_schema(),current_schema());
 EXECUTE format('ALTER FUNCTION %I.storage_gateway_lock_branch(text,text) SET search_path=pg_catalog,%I,pg_temp',current_schema(),current_schema());
 EXECUTE format('ALTER FUNCTION %I.storage_gateway_lock_instance(text,text) SET search_path=pg_catalog,%I,pg_temp',current_schema(),current_schema());
END $path$;
