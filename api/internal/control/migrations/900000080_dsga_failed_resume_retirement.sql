-- Each suspended boot has one original resume and each failed resume has at
-- most one successor. A successor requires a completed, immutable retirement.
DROP INDEX dsga_resume_one_operation_per_suspension;
CREATE UNIQUE INDEX dsga_resume_first_attempt ON operations ((payload->>'resume_from'))
 WHERE action='resume_endpoint_dsga_v1' AND NOT (payload ? 'resume_replaces');
CREATE UNIQUE INDEX dsga_resume_single_successor ON operations ((payload->>'resume_replaces'))
 WHERE action='resume_endpoint_dsga_v1' AND payload ? 'resume_replaces';
CREATE UNIQUE INDEX dsga_resume_single_retirement ON operations ((payload->>'failed_resume_operation_id'))
 WHERE action='retire_failed_dsga_resume_v1';

CREATE TABLE dsga_failed_resume_retirements (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 failed_resume_operation_id text NOT NULL UNIQUE REFERENCES operations(id),
 intent jsonb NOT NULL CHECK (jsonb_typeof(intent)='object' AND octet_length(intent::text)<=8192)
);
CREATE TABLE dsga_failed_resume_settlements (
 operation_id text PRIMARY KEY REFERENCES dsga_failed_resume_retirements(operation_id),
 host_sequence numeric(20,0) NOT NULL CHECK (host_sequence>0)
);

-- The new action has its own lifecycle admission for an already suspended
-- endpoint. Leave the existing trigger in place for all other operations.
DROP TRIGGER operation_lifecycle_admission ON operations;
CREATE TRIGGER operation_lifecycle_admission BEFORE INSERT ON operations FOR EACH ROW
 WHEN (NEW.action <> 'retire_failed_dsga_resume_v1') EXECUTE FUNCTION guard_operation_lifecycle();
DROP TRIGGER operation_lifecycle_retry ON operations;
CREATE TRIGGER operation_lifecycle_retry BEFORE UPDATE OF state ON operations FOR EACH ROW
 WHEN (NEW.state='queued' AND OLD.state='failed' AND NEW.action <> 'retire_failed_dsga_resume_v1')
 EXECUTE FUNCTION guard_operation_lifecycle();

CREATE FUNCTION guard_dsga_failed_resume_retirement() RETURNS trigger LANGUAGE plpgsql AS $$
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
   OR EXISTS (SELECT 1 FROM dsga_creation_initialization_observations WHERE operation_id=original.id)
   OR NOT EXISTS (SELECT 1 FROM operation_steps WHERE operation_id=original.id AND ordinal=3
       AND name='confirm_native_initialization' AND state='failed')
   OR (SELECT count(*) FROM operation_steps WHERE operation_id=original.id AND
       (ordinal=4 AND name='publish_proxy_route' OR ordinal=5 AND name='sql_probe_via_proxy' OR ordinal=6 AND name='persist_ready_state')
       AND state='queued' AND attempts=0)<>3 THEN
  RAISE EXCEPTION 'retirement requires original failed unpublished DSGA resume' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_failed_resume_retirement_admission BEFORE INSERT ON operations FOR EACH ROW
 WHEN (NEW.action='retire_failed_dsga_resume_v1') EXECUTE FUNCTION guard_dsga_failed_resume_retirement();
CREATE TRIGGER dsga_failed_resume_retirement_retry BEFORE UPDATE OF state ON operations FOR EACH ROW
 WHEN (NEW.action='retire_failed_dsga_resume_v1' AND OLD.state='failed' AND NEW.state='queued')
 EXECUTE FUNCTION guard_dsga_failed_resume_retirement();
CREATE TRIGGER dsga_failed_resume_retirement_immutable BEFORE UPDATE ON operations FOR EACH ROW
 WHEN (OLD.action='retire_failed_dsga_resume_v1' OR NEW.action='retire_failed_dsga_resume_v1')
 EXECUTE FUNCTION guard_dsga_creation_intent();

CREATE FUNCTION guard_dsga_resume_replacement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- The base DSGA resume trigger has already pinned the retained data and
 -- credentials. Use the same endpoint lock as admission and failed retry.
 IF TG_OP='UPDATE' THEN
  IF EXISTS (SELECT 1 FROM operations o WHERE o.action='retire_failed_dsga_resume_v1'
    AND o.payload->>'failed_resume_operation_id'=OLD.id) THEN
   RAISE EXCEPTION 'failed resume has a frozen retirement' USING ERRCODE='23514';
  END IF;
 ELSIF NEW.payload ? 'resume_replaces' AND NOT EXISTS (
  SELECT 1 FROM operations failed JOIN dsga_failed_resume_retirements r ON r.failed_resume_operation_id=failed.id
   JOIN dsga_failed_resume_settlements fact ON fact.operation_id=r.operation_id
   JOIN operations retirement ON retirement.id=r.operation_id
  WHERE failed.id=NEW.payload->>'resume_replaces' AND failed.action='resume_endpoint_dsga_v1'
   AND failed.state='failed' AND failed.project_id=NEW.project_id AND failed.resource_id=NEW.resource_id
   AND failed.payload->>'resume_from'=NEW.payload->>'resume_from'
   AND failed.payload#>'{dsga_creation,incarnation}'<>NEW.payload#>'{dsga_creation,incarnation}'
   AND failed.payload#>'{dsga_creation,operation_uuid}'<>NEW.payload#>'{dsga_creation,operation_uuid}'
   AND retirement.state='succeeded' AND retirement.finished_at IS NOT NULL
 ) THEN
  RAISE EXCEPTION 'successor requires settled failed resume retirement' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER z_dsga_resume_replacement BEFORE INSERT ON operations FOR EACH ROW
 WHEN (NEW.action='resume_endpoint_dsga_v1') EXECUTE FUNCTION guard_dsga_resume_replacement();
CREATE TRIGGER z_dsga_resume_retry_retirement BEFORE UPDATE OF state ON operations FOR EACH ROW
 WHEN (OLD.action='resume_endpoint_dsga_v1' AND OLD.state='failed' AND NEW.state='queued')
 EXECUTE FUNCTION guard_dsga_resume_replacement();

CREATE FUNCTION guard_dsga_resume_retirement_fact() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE op operations%ROWTYPE;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'resume retirement facts are immutable' USING ERRCODE='23514'; END IF;
 SELECT * INTO op FROM operations WHERE id=NEW.operation_id AND action='retire_failed_dsga_resume_v1'
   AND state='running' AND lease_owner IS NOT NULL AND lease_expires_at>clock_timestamp();
 IF op.id IS NULL THEN RAISE EXCEPTION 'resume retirement lease required' USING ERRCODE='23514'; END IF;
 IF TG_TABLE_NAME='dsga_failed_resume_retirements' THEN
  IF NEW.failed_resume_operation_id IS DISTINCT FROM op.payload->>'failed_resume_operation_id' OR NOT EXISTS (
   SELECT 1 FROM dsga_creation_bindings b WHERE b.operation_id=NEW.failed_resume_operation_id
    AND b.vm_uid=NEW.intent->>'vm_uid' AND b.config_uid=NEW.intent->>'config_uid') THEN
   RAISE EXCEPTION 'resume retirement intent differs from original bindings' USING ERRCODE='23514';
  END IF;
 ELSE
  IF NOT EXISTS (SELECT 1 FROM dsga_failed_resume_retirements r WHERE r.operation_id=NEW.operation_id
    AND NEW.host_sequence>(r.intent#>>'{host,sequence}')::numeric) THEN
   RAISE EXCEPTION 'retirement requires a newer Host settlement' USING ERRCODE='23514';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_failed_resume_intent_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_failed_resume_retirements
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_resume_retirement_fact();
CREATE TRIGGER dsga_failed_resume_settlement_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_failed_resume_settlements
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_resume_retirement_fact();

CREATE FUNCTION guard_dsga_resume_retirement_complete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.state<>'running' OR NEW.finished_at IS NULL OR NEW.lease_owner IS NOT NULL OR NEW.lease_expires_at IS NOT NULL
  OR NOT EXISTS (SELECT 1 FROM dsga_failed_resume_settlements WHERE operation_id=NEW.id)
  OR NOT EXISTS (SELECT 1 FROM operation_steps WHERE operation_id=NEW.id AND ordinal=0
   AND name='settle_unpublished_resume' AND state='succeeded') THEN
  RAISE EXCEPTION 'resume retirement has not settled' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_failed_resume_retirement_complete BEFORE UPDATE OF state ON operations FOR EACH ROW
 WHEN (NEW.action='retire_failed_dsga_resume_v1' AND NEW.state='succeeded' AND OLD.state IS DISTINCT FROM NEW.state)
 EXECUTE FUNCTION guard_dsga_resume_retirement_complete();
