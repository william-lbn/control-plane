-- Auth identities live in the user branch, never in this metadata database.
-- Metadata contains desired state and references to immutable namespace Secrets.
CREATE TABLE managed_auth_instances (
    branch_id text PRIMARY KEY REFERENCES branches(id),
    project_id text NOT NULL REFERENCES projects(id),
    endpoint_id text NOT NULL REFERENCES endpoints(id),
    generation bigint NOT NULL CHECK (generation > 0),
    state text NOT NULL CHECK (state IN ('provisioning','active','disabling','disabled','degraded')),
    spec jsonb NOT NULL,
    secret_ref text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX one_active_managed_auth_operation ON operations(resource_id)
    WHERE resource_type='managed_auth' AND state IN ('queued','running','retry_wait');
-- Serialize the global bounded runtime budget even across API instances.
-- Parent lifecycle admission is still enforced by the existing Operation trigger.
CREATE FUNCTION guard_managed_auth_budget() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(794210042);
 IF NEW.state='provisioning' AND (TG_OP='INSERT' OR OLD.state='disabled') AND
    (SELECT count(*) FROM managed_auth_instances WHERE state<>'disabled' AND branch_id<>NEW.branch_id)>=8 THEN
   RAISE EXCEPTION 'managed auth capacity exhausted' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER managed_auth_budget BEFORE INSERT OR UPDATE OF state ON managed_auth_instances
    FOR EACH ROW EXECUTE FUNCTION guard_managed_auth_budget();
