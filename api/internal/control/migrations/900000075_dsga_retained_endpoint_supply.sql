-- An admitted retained endpoint uses the same automatic supply as its project.
-- Keep the existing immutable receipts and original live-lease requirement.
CREATE OR REPLACE FUNCTION guard_dsga_supply_fact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'original supply fact immutable' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(
   SELECT 1 FROM operations o JOIN dsga_creation_bindings b ON b.operation_id=o.id
   WHERE o.id=NEW.operation_id AND o.state='running'
   AND o.lease_owner IS NOT NULL AND o.lease_expires_at>clock_timestamp() AND b.config_uid IS NOT NULL
   AND (o.action='create_project_dsga_v1' OR (
     o.action='create_endpoint_dsga_v1' AND o.resource_type='endpoint'
     AND o.resource_id=o.payload->>'endpoint_id'
     AND o.payload#>'{dsga_creation,retain_catalog}'='true'::jsonb
   ))
 ) THEN
   RAISE EXCEPTION 'supply requires original active creation binding' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
