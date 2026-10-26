-- The public data plane has no PostgreSQL credential. Admission is distinct
-- from confirmed dispatch; disconnects retain unknown outcomes, never success.
CREATE UNIQUE INDEX function_instances_meter_scope ON function_instances(id,function_id,deployment_id);
CREATE UNIQUE INDEX function_request_events_meter_scope ON function_request_events(id,function_id,instance_id,deployment_id);
CREATE TABLE function_edge_invocations (
 event_id text PRIMARY KEY REFERENCES function_request_events(id),
 function_id text NOT NULL REFERENCES function_definitions(id),
 instance_id text NOT NULL REFERENCES function_instances(id),
 deployment_id text NOT NULL REFERENCES function_deployments(id),
 boot_id text NOT NULL CHECK(boot_id ~ '^[a-f0-9]{32}$'),
 definition_version bigint NOT NULL CHECK(definition_version>0),
 edge_identity text NOT NULL CHECK(length(edge_identity) BETWEEN 1 AND 128),
 request_bytes bigint NOT NULL CHECK(request_bytes BETWEEN 0 AND 1048576),
 dispatched boolean,
 response_bytes bigint CHECK(response_bytes>=0),
 stream_request_bytes bigint CHECK(stream_request_bytes>=0),
 duration_ms bigint CHECK(duration_ms BETWEEN 0 AND 960000),
 outcome text NOT NULL DEFAULT 'outcome_unknown' CHECK(outcome IN ('outcome_unknown','completed','response_incomplete','client_disconnected','not_dispatched')),
 admitted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 completed_at timestamptz,
 FOREIGN KEY(instance_id,function_id,deployment_id) REFERENCES function_instances(id,function_id,deployment_id),
 FOREIGN KEY(event_id,function_id,instance_id,deployment_id) REFERENCES function_request_events(id,function_id,instance_id,deployment_id)
);
CREATE INDEX function_edge_invocations_usage ON function_edge_invocations(function_id,admitted_at);

-- CPU is cumulative per boot and never added once per concurrent request.
-- Memory is an observed current/peak value, not an invented GB-hour estimate.
CREATE TABLE function_resource_samples (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 function_id text NOT NULL REFERENCES function_definitions(id),
 instance_id text NOT NULL REFERENCES function_instances(id),
 deployment_id text NOT NULL REFERENCES function_deployments(id),
 boot_id text NOT NULL CHECK(boot_id ~ '^[a-f0-9]{32}$'),
 cpu_usec bigint NOT NULL CHECK(cpu_usec>=0),
 memory_bytes bigint NOT NULL CHECK(memory_bytes>=0),
 memory_peak_bytes bigint NOT NULL CHECK(memory_peak_bytes>=memory_bytes),
 observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 source text NOT NULL CHECK(source='supervisor_cgroup_v2'),
 UNIQUE(instance_id,boot_id,observed_at),
 FOREIGN KEY(instance_id,function_id,deployment_id) REFERENCES function_instances(id,function_id,deployment_id)
);
CREATE INDEX function_resource_samples_usage ON function_resource_samples(function_id,instance_id,boot_id,observed_at);
