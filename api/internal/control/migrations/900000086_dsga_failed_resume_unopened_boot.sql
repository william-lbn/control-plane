-- Admit only the known failure before qualification for a never-opened
-- initial boot. The retirement Worker rechecks the immutable identities,
-- current PostgreSQL admission and the original Host before deleting the VM.
CREATE OR REPLACE FUNCTION guard_dsga_failed_resume_retirement() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE original operations%ROWTYPE;
 branch text;
BEGIN
 PERFORM 1 FROM projects WHERE id=NEW.project_id AND deleted_at IS NULL AND state='ready' FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'retirement requires ready project' USING ERRCODE='23514'; END IF;
 branch := NEW.payload->>'branch_id';
 PERFORM 1 FROM branches WHERE id=branch AND project_id=NEW.project_id AND deleted_at IS NULL
  AND state='ready' AND expires_at IS NULL FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'retirement requires retained branch' USING ERRCODE='23514'; END IF;
 PERFORM 1 FROM endpoints WHERE id=NEW.resource_id AND project_id=NEW.project_id AND branch_id=branch
  AND workload_kind='neonvm' AND state='suspended' AND deleted_at IS NULL FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'retirement requires suspended Compute' USING ERRCODE='23514'; END IF;

 SELECT * INTO original FROM operations WHERE id=NEW.payload->>'failed_resume_operation_id'
  AND project_id=NEW.project_id AND resource_type='endpoint' AND resource_id=NEW.resource_id
  AND action='resume_endpoint_dsga_v1' AND state='failed' AND finished_at IS NOT NULL
  AND lease_owner IS NULL AND lease_expires_at IS NULL;
 IF original.id IS NULL OR NEW.state<>'queued' OR NEW.resource_type<>'endpoint'
   OR NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id
   OR NEW.payload->>'endpoint_id' IS DISTINCT FROM NEW.resource_id
   OR original.payload->>'branch_id' IS DISTINCT FROM branch
   OR NOT EXISTS (SELECT 1 FROM operations gone JOIN endpoints e ON e.id=NEW.resource_id AND e.project_id=NEW.project_id
       JOIN operations previous ON previous.id=gone.payload->>'creation_operation_id'
       WHERE gone.id=original.payload->>'resume_from' AND gone.action='suspend_endpoint_dsga_v1' AND gone.state='succeeded'
       AND gone.project_id=NEW.project_id AND gone.resource_type='endpoint' AND gone.resource_id=NEW.resource_id
       AND ((e.dsga_runtime_operation_id=previous.id AND previous.action='resume_endpoint_dsga_v1')
         OR (e.dsga_runtime_operation_id IS NULL AND previous.action IN ('create_project_dsga_v1','create_endpoint_dsga_v1'))))
   OR EXISTS (SELECT 1 FROM operations a WHERE a.project_id=NEW.project_id AND a.resource_type='endpoint'
       AND a.resource_id=NEW.resource_id AND a.id<>NEW.id AND a.state IN ('queued','running','retry_wait'))
   OR (EXISTS (SELECT 1 FROM dsga_creation_initialization_observations WHERE operation_id=original.id)
       AND NOT (
        original.error_code IS NOT DISTINCT FROM 'qualification_unconfirmed'
        AND (SELECT count(*) FROM dsga_creation_initialization_observations WHERE operation_id=original.id)=1
        AND EXISTS (SELECT 1 FROM dsga_creation_initialization_observations i
          WHERE i.operation_id=original.id
          AND i.receipt #>> '{observed,admission_open}' = 'false'
          AND i.receipt #>> '{observed,admission_revision}' = '0'
          AND i.receipt #>> '{observed,issuer_floor_epoch}' = '0'
          AND i.receipt #>> '{observed,capacity_generation}' = '1'
          AND i.receipt #> '{observed,operation}' = 'null'::jsonb)
        AND NOT EXISTS (SELECT 1 FROM dsga_creation_supply_facts f WHERE f.operation_id=original.id
          AND (f.kind IN ('qualification_result','runtime_selection')
           OR starts_with(f.kind,'qualification_retry_') OR starts_with(f.kind,'qualification_job_retry_')))
       ))
   OR NOT (
      EXISTS (SELECT 1 FROM operation_steps WHERE operation_id=original.id AND ordinal=3
        AND name='confirm_native_initialization' AND state='failed')
      OR (original.error_code='compute_not_ready'
        AND EXISTS (SELECT 1 FROM operation_steps WHERE operation_id=original.id AND ordinal=2
          AND name='wait_neonvm_running' AND state='failed')
        AND EXISTS (SELECT 1 FROM operation_steps WHERE operation_id=original.id AND ordinal=3
          AND name='confirm_native_initialization' AND state='queued' AND attempts=0))
   ) IS TRUE
   OR (SELECT count(*) FROM operation_steps WHERE operation_id=original.id AND
       (ordinal=4 AND name='publish_proxy_route' OR ordinal=5 AND name='sql_probe_via_proxy' OR ordinal=6 AND name='persist_ready_state')
       AND state='queued' AND attempts=0)<>3 THEN
  RAISE EXCEPTION 'retirement requires original failed unpublished DSGA resume' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
