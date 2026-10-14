-- Structured request journal, not customer stdout or request-body capture.
-- Persist admission before dispatch so an API crash leaves an explicit unknown
-- outcome instead of erasing evidence or encouraging automatic replay.
CREATE UNIQUE INDEX function_instances_event_scope ON function_instances(id,function_id,deployment_id,project_id,branch_id);
CREATE TABLE function_request_events (
 id text PRIMARY KEY,
 request_id text NOT NULL UNIQUE,
 function_id text NOT NULL REFERENCES function_definitions(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 deployment_id text NOT NULL REFERENCES function_deployments(id),
 instance_id text NOT NULL REFERENCES function_instances(id),
 channel text NOT NULL CHECK (channel IN ('console','public_http')),
 method text NOT NULL,
 outcome text NOT NULL CHECK (outcome IN ('in_progress','completed','outcome_unknown','response_incomplete','client_disconnected')),
 application_status integer CHECK (application_status BETWEEN 100 AND 599),
 response_bytes bigint NOT NULL DEFAULT 0 CHECK(response_bytes>=0),
 duration_ms bigint CHECK(duration_ms>=0),
 started_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 deadline_at timestamptz NOT NULL,
 FOREIGN KEY(function_id,project_id,branch_id) REFERENCES function_definitions(id,project_id,branch_id),
 FOREIGN KEY(deployment_id,function_id,project_id,branch_id) REFERENCES function_deployments(id,function_id,project_id,branch_id),
 FOREIGN KEY(instance_id,function_id,deployment_id,project_id,branch_id) REFERENCES function_instances(id,function_id,deployment_id,project_id,branch_id)
);
CREATE INDEX function_request_events_page ON function_request_events(function_id,started_at DESC,id DESC);
