-- Keep applied migration 014 immutable. Retry of the original failed creation
-- must remain possible without admitting unrelated work on failed/deleted data.
CREATE OR REPLACE FUNCTION guard_operation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ps text; bs text; bid text;
BEGIN
 IF NEW.action IN ('delete_project','delete_branch','recover_project') THEN RETURN NEW; END IF;
 SELECT state INTO ps FROM projects WHERE id=NEW.project_id AND deleted_at IS NULL FOR UPDATE;
 IF NEW.action='create_project' AND ps IN ('provisioning','ready') THEN RETURN NEW; END IF;
 IF TG_OP='UPDATE' AND NEW.action='create_project' AND ps='error' THEN RETURN NEW; END IF;
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
   IF TG_OP='UPDATE' AND NEW.action='create_branch' AND bs='error' THEN RETURN NEW; END IF;
   IF bs IS DISTINCT FROM 'ready' THEN
     RAISE EXCEPTION 'branch lifecycle does not admit new work' USING ERRCODE='23514';
   END IF;
 END IF;
 RETURN NEW;
END $$;
