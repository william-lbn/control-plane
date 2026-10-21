CREATE TABLE function_output_policies (
 function_id text PRIMARY KEY,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 enabled boolean NOT NULL,
 version bigint NOT NULL CHECK(version>0),
 capture_since timestamptz NOT NULL,
 actor_id text NOT NULL REFERENCES users(id),
 last_checked_at timestamptz,
 last_error_code text,
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(function_id,project_id,branch_id) REFERENCES function_definitions(id,project_id,branch_id)
);
CREATE TABLE function_output_policy_requests (
 actor_id text NOT NULL REFERENCES users(id),
 function_id text NOT NULL REFERENCES function_definitions(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 PRIMARY KEY(actor_id,function_id,key_hash)
);
CREATE TABLE function_output_cursors (
 instance_id text NOT NULL REFERENCES function_instances(id),
 boot_id text NOT NULL CHECK(boot_id ~ '^[a-f0-9]{32}$'),
 sequence bigint NOT NULL DEFAULT 0 CHECK(sequence>=0),
 dropped_lines bigint NOT NULL DEFAULT 0 CHECK(dropped_lines>=0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,boot_id)
);
CREATE TABLE function_output_records (
 instance_id text NOT NULL,
 boot_id text NOT NULL CHECK(boot_id ~ '^[a-f0-9]{32}$'),
 sequence bigint NOT NULL CHECK(sequence>0),
 function_id text NOT NULL,
 deployment_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 generation bigint NOT NULL CHECK(generation>0),
 emitted_at timestamptz NOT NULL,
 captured_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL DEFAULT now()+interval '3 days',
 stream text NOT NULL CHECK(stream IN ('stdout','stderr','platform')),
 severity text NOT NULL CHECK(severity IN ('DEBUG','INFO','WARN','ERROR')),
 scope_name text NOT NULL CHECK(scope_name IN ('neon.function.app','neon.function.request')),
 truncated boolean NOT NULL,
 ciphertext bytea NOT NULL CHECK(octet_length(ciphertext) BETWEEN 28 AND 8192),
 PRIMARY KEY(instance_id,boot_id,sequence),
 FOREIGN KEY(instance_id,function_id,deployment_id,project_id,branch_id) REFERENCES function_instances(id,function_id,deployment_id,project_id,branch_id),
 CHECK((stream='platform')=(scope_name='neon.function.request'))
);
CREATE INDEX function_output_history ON function_output_records(function_id,captured_at DESC,instance_id DESC,sequence DESC);
CREATE INDEX function_output_expiry ON function_output_records(expires_at);
