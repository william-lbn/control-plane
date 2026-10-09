-- Intent only. Bucket/object manifests belong to each user database timeline.
CREATE TABLE object_storage_instances (
    branch_id text PRIMARY KEY REFERENCES branches(id),
    project_id text NOT NULL REFERENCES projects(id),
    endpoint_id text NOT NULL REFERENCES endpoints(id),
    generation bigint NOT NULL CHECK (generation > 0),
    state text NOT NULL CHECK (state IN ('provisioning','active','disabling','disabled','degraded')),
    spec jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX one_active_object_storage_operation ON operations(resource_id)
    WHERE resource_type='object_storage' AND state IN ('queued','running','retry_wait');
CREATE FUNCTION guard_object_storage_budget() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(794210043);
 IF NEW.state='provisioning' AND (TG_OP='INSERT' OR OLD.state='disabled') AND
    (SELECT count(*) FROM object_storage_instances WHERE state<>'disabled' AND branch_id<>NEW.branch_id)>=8 THEN
   RAISE EXCEPTION 'object storage instance capacity exhausted' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER object_storage_budget BEFORE INSERT OR UPDATE OF state ON object_storage_instances
    FOR EACH ROW EXECUTE FUNCTION guard_object_storage_budget();
