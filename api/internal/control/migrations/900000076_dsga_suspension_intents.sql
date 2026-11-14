-- Keep the original VM, Runner, Host and selection fixed across Worker restarts.
CREATE TABLE dsga_suspension_intents (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 intent jsonb NOT NULL CHECK (jsonb_typeof(intent)='object' AND octet_length(intent::text)<=65536),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION guard_dsga_suspension_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' OR NOT EXISTS (
  SELECT 1 FROM operations o
   JOIN operations original ON original.id=o.payload->>'creation_operation_id'
   JOIN dsga_creation_bindings b ON b.operation_id=original.id
   WHERE o.id=NEW.operation_id AND o.action='suspend_endpoint_dsga_v1'
    AND o.resource_type='endpoint' AND o.state='running'
    AND o.lease_owner IS NOT NULL AND o.lease_expires_at>clock_timestamp()
    AND o.project_id=o.payload->>'project_id' AND o.resource_id=o.payload->>'endpoint_id'
    AND original.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1')
    AND original.state='succeeded' AND original.project_id=o.project_id
    AND original.payload->>'endpoint_id'=o.resource_id
    AND original.payload->>'branch_id'=o.payload->>'branch_id'
    AND b.vm_uid=o.payload->>'vm_uid'
    AND b.namespace=NEW.intent #>> '{selection,next,namespace}'
    AND b.vm_name=NEW.intent #>> '{selection,next,name}'
    AND b.vm_uid=NEW.intent #>> '{selection,next,instance,vm_uid}'
    AND b.compute_instance_id=NEW.intent #>> '{selection,next,instance,compute_instance_id}'
    AND b.incarnation=NEW.intent #>> '{selection,next,incarnation}'
    AND NEW.operation_id=NEW.intent #>> '{selection,next,suspension_operation}'
 ) THEN
  RAISE EXCEPTION 'suspension intent requires an active operation and its original Compute' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER dsga_suspension_intent_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_suspension_intents
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_suspension_intent();

-- This is permission to reconcile retirement of the original VM. It isn't
-- evidence that kube-apiserver accepted DELETE or that the Host has settled.
CREATE TABLE dsga_suspension_retirements (
 operation_id text PRIMARY KEY REFERENCES dsga_suspension_intents(operation_id),
 vm_uid text NOT NULL CHECK (length(vm_uid) BETWEEN 1 AND 128),
 drain_resource_version text NOT NULL CHECK (length(drain_resource_version) BETWEEN 1 AND 128),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION guard_dsga_suspension_retirement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' OR NOT EXISTS (
  SELECT 1 FROM operations o JOIN dsga_suspension_intents s ON s.operation_id=o.id
  WHERE o.id=NEW.operation_id AND o.action='suspend_endpoint_dsga_v1'
   AND o.state='running' AND o.lease_owner IS NOT NULL AND o.lease_expires_at>clock_timestamp()
   AND NEW.vm_uid=o.payload->>'vm_uid' AND NEW.vm_uid=s.intent #>> '{selection,next,instance,vm_uid}'
 ) THEN
  RAISE EXCEPTION 'retirement requires the original active suspension and is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER dsga_suspension_retirement_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_suspension_retirements
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_suspension_retirement();
