-- One public suspension intent per physical VM generation. Keep failed rows
-- covered so a retry always uses the original Operation and Owner closure.
CREATE UNIQUE INDEX dsga_suspend_one_operation_per_vm ON operations ((payload->>'vm_uid'))
 WHERE action='suspend_endpoint_dsga_v1';

CREATE FUNCTION guard_dsga_suspend_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF OLD.action IS DISTINCT FROM NEW.action OR NEW.payload IS DISTINCT FROM OLD.payload
    OR ROW(NEW.project_id,NEW.resource_type,NEW.resource_id) IS DISTINCT FROM ROW(OLD.project_id,OLD.resource_type,OLD.resource_id) THEN
   RAISE EXCEPTION 'original suspend operation is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF NEW.state<>'queued' OR NEW.resource_type<>'endpoint'
   OR NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id
   OR NEW.payload->>'endpoint_id' IS DISTINCT FROM NEW.resource_id
   OR NOT EXISTS (
    SELECT 1 FROM endpoints e JOIN branches br ON br.id=e.branch_id AND br.project_id=e.project_id
     JOIN operations original ON original.id=NEW.payload->>'creation_operation_id'
     JOIN dsga_creation_bindings b ON b.operation_id=original.id
    WHERE e.id=NEW.resource_id AND e.project_id=NEW.project_id AND e.state='active' AND e.deleted_at IS NULL
     AND e.workload_kind='neonvm' AND original.project_id=e.project_id
     AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')
     AND original.state='succeeded' AND original.finished_at IS NOT NULL
     AND original.payload->>'endpoint_id'=e.id AND original.payload->>'branch_id'=e.branch_id
     AND original.payload->>'timeline_id'=br.timeline_id AND NEW.payload->>'branch_id'=e.branch_id
     AND b.vm_uid=NEW.payload->>'vm_uid' AND b.config_uid IS NOT NULL
     AND EXISTS(SELECT 1 FROM dsga_creation_supply_facts f WHERE f.operation_id=original.id AND f.kind='runtime_selection')
   ) THEN
  RAISE EXCEPTION 'suspend operation requires the original ready Compute' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER dsga_suspend_admission BEFORE INSERT ON operations
 FOR EACH ROW WHEN (NEW.action='suspend_endpoint_dsga_v1')
 EXECUTE FUNCTION guard_dsga_suspend_operation();

CREATE TRIGGER dsga_suspend_immutable BEFORE UPDATE OF action,payload,project_id,resource_type,resource_id ON operations
 FOR EACH ROW WHEN (OLD.action='suspend_endpoint_dsga_v1' OR NEW.action='suspend_endpoint_dsga_v1')
 EXECUTE FUNCTION guard_dsga_suspend_operation();
