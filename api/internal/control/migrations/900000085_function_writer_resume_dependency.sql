-- The Function and its original Writer are admitted together. Retrying either
-- operation must preserve the same project-scoped dependency.
CREATE FUNCTION guard_function_writer_resume_dependency() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE field text := 'writer_resume_operation_id';
BEGIN
 IF TG_OP='UPDATE' THEN
   IF OLD.payload ? field OR NEW.payload ? field THEN
     IF OLD.payload->field IS DISTINCT FROM NEW.payload->field
       OR (OLD.action,OLD.project_id,OLD.resource_type,OLD.resource_id,OLD.payload->>'branch_id')
          IS DISTINCT FROM (NEW.action,NEW.project_id,NEW.resource_type,NEW.resource_id,NEW.payload->>'branch_id') THEN
       RAISE EXCEPTION 'original Function Writer dependency is immutable' USING ERRCODE='23514';
     END IF;
   END IF;
   RETURN NEW;
 END IF;
 IF NOT (NEW.payload ? field) THEN RETURN NEW; END IF;
 IF NEW.action<>'deploy_function' OR NEW.resource_type<>'function'
   OR jsonb_typeof(NEW.payload->field)<>'string'
   OR NOT EXISTS (
     SELECT 1 FROM function_definitions f JOIN endpoints e ON e.id=f.endpoint_id AND e.project_id=f.project_id AND e.branch_id=f.branch_id
     JOIN operations w ON w.id=NEW.payload->>field AND w.project_id=e.project_id AND w.resource_id=e.id
     WHERE f.id=NEW.resource_id AND f.project_id=NEW.project_id AND f.branch_id=NEW.payload->>'branch_id'
       AND e.state='suspended' AND e.endpoint_type='read_write' AND e.deleted_at IS NULL
       AND w.resource_type='endpoint' AND w.action='resume_endpoint_dsga_v1' AND w.state='queued'
       AND w.payload->>'endpoint_id'=e.id AND w.payload->>'branch_id'=e.branch_id
   ) THEN
   RAISE EXCEPTION 'Function Writer dependency does not identify the original queued resume' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER function_writer_resume_dependency_guard BEFORE INSERT OR UPDATE OF action,project_id,resource_type,resource_id,payload
 ON operations FOR EACH ROW EXECUTE FUNCTION guard_function_writer_resume_dependency();
