-- A copied SQL object whose original managed OID is known to be replaced is
-- preserved physically, but is never adopted by a name/branch-marker match.
-- These receipts do not change or delete the original immutable 049 baseline.
CREATE TABLE catalog_fork_inspections (
 branch_id text PRIMARY KEY REFERENCES branches(id),
 project_id text NOT NULL REFERENCES projects(id),
 operation_id text NOT NULL UNIQUE REFERENCES operations(id),
 source_branch_id text NOT NULL REFERENCES branches(id),
 source_tenant_id text NOT NULL CHECK(source_tenant_id ~ '^[a-f0-9]{32}$'),
 source_timeline_id text NOT NULL CHECK(source_timeline_id ~ '^[a-f0-9]{32}$'),
 timeline_id text NOT NULL CHECK(timeline_id ~ '^[a-f0-9]{32}$'),
 basis text NOT NULL CHECK(basis IN ('known_identity_check','original_probe_already_succeeded')),
 excluded_roles integer NOT NULL CHECK(excluded_roles BETWEEN 0 AND 500),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE catalog_fork_exclusions (
 branch_id text NOT NULL REFERENCES catalog_fork_inspections(branch_id),
 project_id text NOT NULL REFERENCES projects(id),
 name text NOT NULL,
 source_baseline_id text NOT NULL REFERENCES catalog_identity_baselines(id),
 source_branch_id text NOT NULL REFERENCES branches(id),
 expected_oid bigint NOT NULL CHECK(expected_oid BETWEEN 1 AND 4294967295),
 observed_oid bigint NOT NULL CHECK(observed_oid BETWEEN 1 AND 4294967295),
 credential_ref text NOT NULL CHECK(credential_ref<>''),
 credential_uid text NOT NULL CHECK(credential_uid<>''),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(branch_id,name),
 CHECK(expected_oid<>observed_oid)
);
CREATE TABLE catalog_fork_exclusion_backings (
 endpoint_id text PRIMARY KEY REFERENCES endpoints(id),
 branch_id text NOT NULL REFERENCES catalog_fork_inspections(branch_id),
 operation_id text NOT NULL REFERENCES operations(id),
 config_uid text NOT NULL,
 routes_uid text NOT NULL,
 verifier_digests jsonb NOT NULL CHECK(jsonb_typeof(verifier_digests)='object'),
 state text NOT NULL CHECK(state IN ('updating','ready')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_catalog_fork_observation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Original catalog fork observation is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_catalog_fork_inspection BEFORE UPDATE OR DELETE ON catalog_fork_inspections FOR EACH ROW EXECUTE FUNCTION preserve_catalog_fork_observation();
CREATE TRIGGER preserve_catalog_fork_exclusion BEFORE UPDATE OR DELETE ON catalog_fork_exclusions FOR EACH ROW EXECUTE FUNCTION preserve_catalog_fork_observation();
CREATE FUNCTION preserve_catalog_fork_backing_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' OR NEW.endpoint_id<>OLD.endpoint_id OR NEW.branch_id<>OLD.branch_id OR NEW.operation_id<>OLD.operation_id OR NEW.config_uid<>OLD.config_uid OR NEW.routes_uid<>OLD.routes_uid OR NEW.verifier_digests<>OLD.verifier_digests OR (OLD.state='ready' AND NEW.state<>'ready') THEN
  RAISE EXCEPTION 'Original catalog fork backing identity is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER preserve_catalog_fork_backings BEFORE UPDATE OR DELETE ON catalog_fork_exclusion_backings FOR EACH ROW EXECUTE FUNCTION preserve_catalog_fork_backing_identity();
