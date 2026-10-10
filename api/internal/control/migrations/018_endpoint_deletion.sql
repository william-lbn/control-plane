-- Forward migration: keep every applied migration, native timeline and Secret.
ALTER TABLE resource_tombstones DROP CONSTRAINT resource_tombstones_resource_type_check;
ALTER TABLE resource_tombstones ADD CONSTRAINT resource_tombstones_resource_type_check
    CHECK (resource_type IN ('project','branch','endpoint'));

-- The existing unique active endpoint Operation index covers delete as well.
-- Endpoint readiness is checked inside parent admission locks, so a stale API
-- read / idle-controller sample cannot enqueue work after deletion commits.
CREATE OR REPLACE FUNCTION guard_operation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ps text; bs text; bid text; eid text; es text; did text;
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
   IF TG_OP='UPDATE' AND NEW.action='delete_endpoint' AND did=NEW.id AND es IN ('deleting','deleted') THEN RETURN NEW; END IF;
   IF es IS DISTINCT FROM 'active' THEN
     RAISE EXCEPTION 'endpoint lifecycle does not admit new work' USING ERRCODE='23514';
   END IF;
 END IF;
 RETURN NEW;
END $$;
