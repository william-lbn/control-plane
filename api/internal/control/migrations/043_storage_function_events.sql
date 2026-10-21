-- Object publication and its signed outbox live in the branch database. This
-- metadata marker is written while holding that database's directory lock,
-- before publication; the exporter takes the same lock before clearing it.
CREATE TABLE storage_event_branches (
 branch_id text PRIMARY KEY,
 project_id text NOT NULL,
 endpoint_id text NOT NULL REFERENCES endpoints(id),
 database_name text NOT NULL,
 timeline_id text NOT NULL CHECK(timeline_id ~ '^[a-f0-9]{32}$'),
 generation bigint NOT NULL DEFAULT 1 CHECK(generation>0),
 dirty boolean NOT NULL DEFAULT true,
 error_code text,
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id)
);
CREATE INDEX storage_event_branches_dirty ON storage_event_branches(updated_at,branch_id) WHERE dirty;

ALTER TABLE function_triggers DROP CONSTRAINT function_triggers_type_check;
ALTER TABLE function_triggers ALTER COLUMN cron DROP NOT NULL;
ALTER TABLE function_triggers ADD COLUMN bucket_name text;
ALTER TABLE function_triggers ADD COLUMN key_prefix text;
ALTER TABLE function_triggers ADD CONSTRAINT function_trigger_types CHECK(type IN ('schedule','storage_object_created'));
ALTER TABLE function_triggers ADD CONSTRAINT function_trigger_configuration CHECK(
 (type='schedule' AND cron IS NOT NULL AND bucket_name IS NULL AND key_prefix IS NULL) OR
 (type='storage_object_created' AND cron IS NULL AND bucket_name IS NOT NULL AND bucket_name ~ '^[a-z][a-z0-9-]{1,61}[a-z0-9]$' AND key_prefix IS NOT NULL AND octet_length(key_prefix)<=1024)
);
ALTER TABLE function_triggers DROP CONSTRAINT function_triggers_check1;
ALTER TABLE function_triggers ADD CONSTRAINT function_trigger_next_schedule CHECK((enabled AND state='ready' AND type='schedule')=(next_run_at IS NOT NULL));

ALTER TABLE function_trigger_invocations ADD COLUMN object_event_id text CHECK(object_event_id IS NULL OR object_event_id ~ '^soe_[a-f0-9]{32}$');
ALTER TABLE function_trigger_invocations ADD COLUMN parent_invocation_id text REFERENCES function_trigger_invocations(id);
ALTER TABLE function_trigger_invocations ADD CONSTRAINT function_trigger_invocation_scope UNIQUE(id,project_id,branch_id);
DO $$ DECLARE constraint_name text; BEGIN
 SELECT conname INTO STRICT constraint_name FROM pg_constraint WHERE conrelid='function_trigger_invocations'::regclass AND contype='u' AND pg_get_constraintdef(oid)='UNIQUE (trigger_id, branch_id, scheduled_at)';
 EXECUTE format('ALTER TABLE function_trigger_invocations DROP CONSTRAINT %I',constraint_name);
END $$;
DO $$ DECLARE constraint_name text; BEGIN
 SELECT conname INTO STRICT constraint_name FROM pg_constraint WHERE conrelid='function_trigger_invocations'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%date_trunc%minute%scheduled_at%';
 EXECUTE format('ALTER TABLE function_trigger_invocations DROP CONSTRAINT %I',constraint_name);
END $$;
ALTER TABLE function_trigger_invocations ADD CONSTRAINT scheduled_occurrence_minute CHECK(object_event_id IS NOT NULL OR date_trunc('minute',scheduled_at)=scheduled_at);
ALTER TABLE function_trigger_invocations ADD CONSTRAINT object_occurrence_parent CHECK(parent_invocation_id IS NULL OR object_event_id IS NOT NULL);
CREATE UNIQUE INDEX function_schedule_occurrence ON function_trigger_invocations(trigger_id,branch_id,scheduled_at) WHERE object_event_id IS NULL;
CREATE UNIQUE INDEX function_object_occurrence ON function_trigger_invocations(trigger_id,branch_id,object_event_id) WHERE object_event_id IS NOT NULL AND parent_invocation_id IS NULL;

-- Existing schedule terminal receipts remain immutable. An explicit object
-- redrive creates a NEW delivery receipt, retaining its original logical ID.
CREATE OR REPLACE FUNCTION guard_function_trigger_invocation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.id,NEW.trigger_id,NEW.project_id,NEW.branch_id,NEW.function_id,NEW.actor_id,NEW.trigger_generation,NEW.scheduled_at,NEW.snapshot,NEW.admitted_at,NEW.object_event_id,NEW.parent_invocation_id)
 IS DISTINCT FROM ROW(OLD.id,OLD.trigger_id,OLD.project_id,OLD.branch_id,OLD.function_id,OLD.actor_id,OLD.trigger_generation,OLD.scheduled_at,OLD.snapshot,OLD.admitted_at,OLD.object_event_id,OLD.parent_invocation_id) THEN
  RAISE EXCEPTION 'trigger occurrence identity is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.state IN ('succeeded','failed','outcome_unknown','skipped') AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'terminal trigger receipt is immutable' USING ERRCODE='23514';
 END IF;
 IF NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
 (NEW.attempts<>OLD.attempts AND NOT (OLD.state='queued' AND NEW.state='dispatching')) OR
 (OLD.request_event_id IS NOT NULL AND NEW.request_event_id IS DISTINCT FROM OLD.request_event_id) THEN
  RAISE EXCEPTION 'trigger dispatch receipt cannot be rewritten' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE TABLE function_trigger_delivery_attempts (
 invocation_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 attempt integer NOT NULL CHECK(attempt BETWEEN 1 AND 3),
 request_event_id text NOT NULL UNIQUE REFERENCES function_request_events(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(invocation_id,attempt),
 FOREIGN KEY(invocation_id,project_id,branch_id) REFERENCES function_trigger_invocations(id,project_id,branch_id)
);
CREATE TABLE function_trigger_retry_requests (
 actor_id text NOT NULL REFERENCES users(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(actor_id,project_id,branch_id,key_hash)
);
CREATE FUNCTION guard_object_event_redrive() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE original function_trigger_invocations;
BEGIN
 IF NEW.parent_invocation_id IS NULL THEN RETURN NEW; END IF;
 SELECT * INTO STRICT original FROM function_trigger_invocations WHERE id=NEW.parent_invocation_id;
 IF original.parent_invocation_id IS NOT NULL OR original.object_event_id IS NULL OR original.state NOT IN ('failed','outcome_unknown') OR
 ROW(NEW.trigger_id,NEW.project_id,NEW.branch_id,NEW.function_id,NEW.actor_id,NEW.trigger_generation,NEW.scheduled_at,NEW.snapshot,NEW.object_event_id)
 IS DISTINCT FROM ROW(original.trigger_id,original.project_id,original.branch_id,original.function_id,original.actor_id,original.trigger_generation,original.scheduled_at,original.snapshot,original.object_event_id) OR
 NEW.state<>'queued' OR NEW.attempts<>0 OR NEW.request_event_id IS NOT NULL THEN
  RAISE EXCEPTION 'redrive must preserve original object occurrence' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER bind_object_event_redrive BEFORE INSERT ON function_trigger_invocations FOR EACH ROW EXECUTE FUNCTION guard_object_event_redrive();
ALTER TABLE function_request_events DROP CONSTRAINT function_request_events_channel_check;
ALTER TABLE function_request_events ADD CONSTRAINT function_request_events_channel_check CHECK(channel IN ('console','public_http','schedule','object_storage'));
