-- Durable branch-local trigger foundation. Branch + stable ID is composite so
-- a later inherited copy can keep its original project-scoped identity.
CREATE TABLE function_triggers (
 branch_id text NOT NULL,
 id text NOT NULL CHECK(id ~ '^ftr_[a-f0-9]{24}$'),
 project_id text NOT NULL,
 org_id text NOT NULL,
 function_id text NOT NULL,
 actor_id text NOT NULL REFERENCES users(id),
 origin_branch_id text NOT NULL,
 type text NOT NULL CHECK(type='schedule'),
 name text NOT NULL CHECK(char_length(name) BETWEEN 1 AND 256 AND octet_length(name)<=1024),
 function_path text NOT NULL CHECK(octet_length(function_path) BETWEEN 1 AND 2048),
 cron text NOT NULL CHECK(octet_length(cron) BETWEEN 9 AND 128),
 timezone text NOT NULL DEFAULT 'UTC' CHECK(timezone='UTC'),
 enabled boolean NOT NULL,
 generation bigint NOT NULL DEFAULT 1 CHECK(generation>0),
 next_run_at timestamptz,
 state text NOT NULL DEFAULT 'ready' CHECK(state IN ('ready','deleted')),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 deleted_at timestamptz,
 PRIMARY KEY(branch_id,id),
 UNIQUE(id,project_id,branch_id),
 FOREIGN KEY(project_id,org_id) REFERENCES projects(id,org_id),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id),
 FOREIGN KEY(origin_branch_id,project_id) REFERENCES branches(id,project_id),
 FOREIGN KEY(function_id,project_id,branch_id) REFERENCES function_definitions(id,project_id,branch_id),
 CHECK((state='deleted')=(deleted_at IS NOT NULL)),
 CHECK((enabled AND state='ready')=(next_run_at IS NOT NULL))
);
CREATE UNIQUE INDEX function_trigger_name ON function_triggers(branch_id,name) WHERE state='ready';
CREATE INDEX function_trigger_due ON function_triggers(next_run_at) WHERE state='ready' AND enabled;

CREATE TABLE function_trigger_requests (
 actor_id text NOT NULL REFERENCES users(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(actor_id,project_id,branch_id,key_hash)
);

-- A queued occurrence is an immutable snapshot: disabling/deleting the trigger
-- stops future admission, but does not silently erase an already queued run.
CREATE TABLE function_trigger_invocations (
 id text PRIMARY KEY CHECK(id ~ '^fti_[a-f0-9]{64}$'),
 trigger_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 function_id text NOT NULL,
 actor_id text NOT NULL REFERENCES users(id),
 trigger_generation bigint NOT NULL CHECK(trigger_generation>0),
 scheduled_at timestamptz NOT NULL,
 snapshot jsonb NOT NULL,
 state text NOT NULL CHECK(state IN ('queued','dispatching','succeeded','failed','outcome_unknown','skipped')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 3),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 lease_owner text,
 lease_expires_at timestamptz,
 request_event_id text REFERENCES function_request_events(id),
 application_status integer CHECK(application_status BETWEEN 100 AND 599),
 response_bytes bigint NOT NULL DEFAULT 0 CHECK(response_bytes>=0),
 error_code text CHECK(error_code IS NULL OR error_code ~ '^[a-z][a-z0-9_]{0,79}$'),
 admitted_at timestamptz NOT NULL DEFAULT now(),
 started_at timestamptz,
 finished_at timestamptz,
 UNIQUE(trigger_id,branch_id,scheduled_at),
 FOREIGN KEY(trigger_id,project_id,branch_id) REFERENCES function_triggers(id,project_id,branch_id),
 FOREIGN KEY(function_id,project_id,branch_id) REFERENCES function_definitions(id,project_id,branch_id),
 CHECK((state IN ('succeeded','failed','outcome_unknown','skipped'))=(finished_at IS NOT NULL)),
 CHECK((state='dispatching')=(lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
 CHECK((lease_owner IS NULL)=(lease_expires_at IS NULL)),
 CHECK(date_trunc('minute',scheduled_at)=scheduled_at)
);
CREATE INDEX function_trigger_invocation_queue ON function_trigger_invocations(next_attempt_at,admitted_at,id) WHERE state='queued';
CREATE INDEX function_trigger_invocation_history ON function_trigger_invocations(branch_id,trigger_id,scheduled_at DESC,id DESC);

CREATE FUNCTION guard_function_trigger_invocation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.id,NEW.trigger_id,NEW.project_id,NEW.branch_id,NEW.function_id,NEW.actor_id,NEW.trigger_generation,NEW.scheduled_at,NEW.snapshot,NEW.admitted_at)
 IS DISTINCT FROM ROW(OLD.id,OLD.trigger_id,OLD.project_id,OLD.branch_id,OLD.function_id,OLD.actor_id,OLD.trigger_generation,OLD.scheduled_at,OLD.snapshot,OLD.admitted_at) THEN
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
CREATE TRIGGER immutable_function_trigger_occurrence BEFORE UPDATE ON function_trigger_invocations FOR EACH ROW EXECUTE FUNCTION guard_function_trigger_invocation();

ALTER TABLE function_request_events DROP CONSTRAINT function_request_events_channel_check;
ALTER TABLE function_request_events ADD CONSTRAINT function_request_events_channel_check CHECK(channel IN ('console','public_http','schedule'));
