-- Logical deletion closes admission before the worker touches Kubernetes.
-- Native timelines, WAL, objects, Secrets and audit history are never purged by
-- this migration. A purge driver requires a separate irreversible-GC gate.
ALTER TABLE projects ADD COLUMN recover_until timestamptz;
ALTER TABLE projects ADD COLUMN deletion_operation_id text REFERENCES operations(id);
ALTER TABLE branches ADD COLUMN deletion_operation_id text REFERENCES operations(id);
ALTER TABLE endpoints ADD COLUMN deletion_operation_id text REFERENCES operations(id);
CREATE TABLE resource_tombstones (
    operation_id text PRIMARY KEY REFERENCES operations(id),
    project_id text NOT NULL REFERENCES projects(id),
    resource_type text NOT NULL CHECK (resource_type IN ('project','branch')),
    resource_id text NOT NULL,
    snapshot jsonb NOT NULL,
    deleted_at timestamptz NOT NULL DEFAULT now(),
    recover_until timestamptz,
    restored_at timestamptz,
    restored_by_operation_id text REFERENCES operations(id),
    physical_gc_state text NOT NULL DEFAULT 'held' CHECK (physical_gc_state = 'held')
);
CREATE INDEX tombstone_project ON resource_tombstones(project_id,deleted_at);
CREATE UNIQUE INDEX one_active_resource_lifecycle ON operations(resource_id)
    WHERE resource_type IN ('project_lifecycle','branch_lifecycle')
      AND state IN ('queued','running','retry_wait');

-- Serialize new intents with deletion using durable parent locks, including
-- the idle controller. A handler's earlier readiness read is not sufficient.
CREATE FUNCTION guard_operation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ps text; bs text; bid text;
BEGIN
 IF NEW.action IN ('delete_project','delete_branch','recover_project') THEN RETURN NEW; END IF;
 SELECT state INTO ps FROM projects WHERE id=NEW.project_id AND deleted_at IS NULL FOR UPDATE;
 IF NEW.action='create_project' AND ps IN ('provisioning','error','ready') THEN RETURN NEW; END IF;
 IF ps IS DISTINCT FROM 'ready' THEN
   RAISE EXCEPTION 'project lifecycle does not admit new work' USING ERRCODE='23514';
 END IF;
 bid := NEW.payload->>'branch_id';
 IF NEW.resource_type='endpoint' THEN
   SELECT branch_id INTO bid FROM endpoints WHERE id=NEW.resource_id AND deleted_at IS NULL;
 END IF;
 IF bid IS NOT NULL THEN
   SELECT state INTO bs FROM branches WHERE id=bid AND project_id=NEW.project_id AND deleted_at IS NULL FOR UPDATE;
   IF NEW.action='create_branch' AND bs='creating' THEN RETURN NEW; END IF;
   IF bs IS DISTINCT FROM 'ready' THEN
     RAISE EXCEPTION 'branch lifecycle does not admit new work' USING ERRCODE='23514';
   END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operation_lifecycle_admission BEFORE INSERT ON operations
    FOR EACH ROW EXECUTE FUNCTION guard_operation_lifecycle();
CREATE TRIGGER operation_lifecycle_retry BEFORE UPDATE OF state ON operations
    FOR EACH ROW WHEN (NEW.state='queued' AND OLD.state='failed')
    EXECUTE FUNCTION guard_operation_lifecycle();
