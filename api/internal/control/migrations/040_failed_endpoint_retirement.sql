-- Forward-only: retain failed receipts, credentials, timelines and all data.
CREATE OR REPLACE FUNCTION guard_operation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ps text; bs text; bid text; eid text; es text; did text; original_id text;
BEGIN
 IF NEW.action IN ('delete_project','delete_branch','recover_project') THEN RETURN NEW; END IF;
 SELECT state INTO ps FROM projects WHERE id=NEW.project_id AND deleted_at IS NULL FOR UPDATE;
 IF NEW.action='create_project' AND ps IN ('provisioning','ready') THEN RETURN NEW; END IF;
 IF TG_OP='UPDATE' AND NEW.action='create_project' AND ps='error' THEN RETURN NEW; END IF;
 IF ps IS DISTINCT FROM 'ready' THEN
   RAISE EXCEPTION 'project lifecycle does not admit new work' USING ERRCODE='23514';
 END IF;
 bid := NULLIF(NEW.payload->>'branch_id','');
 eid := NULLIF(NEW.payload->>'endpoint_id','');
 IF NEW.resource_type='endpoint' THEN eid := NEW.resource_id; END IF;
 IF eid IS NOT NULL THEN
   SELECT branch_id INTO bid FROM endpoints WHERE id=eid AND project_id=NEW.project_id;
 END IF;
 IF bid IS NOT NULL THEN
   SELECT state INTO bs FROM branches WHERE id=bid AND project_id=NEW.project_id AND deleted_at IS NULL FOR UPDATE;
   IF NEW.action='create_branch' AND bs='creating' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND NEW.action='create_branch' AND bs='error' THEN RETURN NEW; END IF;
   IF bs IS DISTINCT FROM 'ready' THEN
     RAISE EXCEPTION 'branch lifecycle does not admit new work' USING ERRCODE='23514';
   END IF;
 END IF;
 IF eid IS NOT NULL THEN
   SELECT state,deletion_operation_id INTO es,did FROM endpoints
     WHERE id=eid AND project_id=NEW.project_id FOR UPDATE;
   IF NEW.action='create_endpoint' AND es='provisioning' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND NEW.action='create_endpoint' AND es='error' THEN RETURN NEW; END IF;
   IF TG_OP='UPDATE' AND NEW.action IN ('delete_endpoint','cleanup_failed_endpoint') AND did=NEW.id AND es IN ('deleting','deleted') THEN RETURN NEW; END IF;
   IF TG_OP='INSERT' AND NEW.action IN ('delete_endpoint','cleanup_failed_endpoint') THEN
     original_id := NULLIF(NEW.payload->>'failed_create_operation_id','');
     IF NEW.action='cleanup_failed_endpoint' AND NEW.resource_type='endpoint' AND NEW.resource_id=eid AND es='error' AND original_id IS NOT NULL AND
       NEW.payload->>'project_id'=NEW.project_id AND NEW.payload->>'endpoint_id'=eid AND NEW.payload->>'branch_id'=bid AND
       (SELECT count(*) FROM operations WHERE project_id=NEW.project_id AND resource_type='endpoint' AND resource_id=eid AND action='create_endpoint')=1 AND
       EXISTS(SELECT 1 FROM operations o WHERE o.id=original_id AND o.project_id=NEW.project_id
         AND o.resource_type='endpoint' AND o.resource_id=eid AND o.action='create_endpoint'
         AND o.state='failed' AND o.finished_at IS NOT NULL AND o.lease_owner IS NULL AND o.lease_expires_at IS NULL
         AND o.payload->>'project_id'=NEW.project_id AND o.payload->>'branch_id'=bid AND o.payload->>'endpoint_id'=eid) THEN
       RETURN NEW;
     END IF;
     IF NEW.action='cleanup_failed_endpoint' OR es='error' OR original_id IS NOT NULL THEN
       RAISE EXCEPTION 'unique original failed endpoint creation required' USING ERRCODE='23514';
     END IF;
   END IF;
   IF es IS DISTINCT FROM 'active' THEN
     RAISE EXCEPTION 'endpoint lifecycle does not admit new work' USING ERRCODE='23514';
   END IF;
 END IF;
 RETURN NEW;
END $$;

-- An empty observed_uid is an explicit first observation of HTTP 404. Absence
-- and a positive original UID remain immutable across unknown write outcomes.
CREATE TABLE failed_endpoint_resource_bindings (
 operation_id text NOT NULL REFERENCES operations(id),
 resource_kind text NOT NULL CHECK(resource_kind IN ('vm','service')),
 project_id text NOT NULL REFERENCES projects(id),
 endpoint_id text NOT NULL REFERENCES endpoints(id),
 namespace text NOT NULL CHECK(namespace<>''),
 resource_name text NOT NULL CHECK(resource_name<>''),
 observed_uid text NOT NULL,
 observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(operation_id,resource_kind)
);
CREATE FUNCTION guard_failed_endpoint_resource_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM operations o JOIN endpoints e ON e.id=o.resource_id AND e.project_id=o.project_id
   WHERE o.id=NEW.operation_id AND o.project_id=NEW.project_id AND o.resource_type='endpoint'
   AND o.resource_id=NEW.endpoint_id AND o.action='cleanup_failed_endpoint' AND o.state='running'
   AND o.lease_owner IS NOT NULL AND o.lease_expires_at>clock_timestamp()
   AND e.deletion_operation_id=o.id AND e.state IN ('deleting','deleted')
   AND NULLIF(o.payload->>'failed_create_operation_id','') IS NOT NULL) THEN
   RAISE EXCEPTION 'resource witness requires the leased original endpoint cleanup' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER scope_failed_endpoint_resource_binding BEFORE INSERT ON failed_endpoint_resource_bindings
 FOR EACH ROW EXECUTE FUNCTION guard_failed_endpoint_resource_binding();
CREATE FUNCTION immutable_failed_endpoint_resource_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'failed endpoint resource observation is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_failed_endpoint_resource_binding BEFORE UPDATE OR DELETE ON failed_endpoint_resource_bindings
 FOR EACH ROW EXECUTE FUNCTION immutable_failed_endpoint_resource_binding();
