-- No prompt, completion, bearer token, provider URL or secret is persisted.
CREATE TABLE ai_gateway_requests (
 id text PRIMARY KEY,
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 actor_id text NOT NULL REFERENCES users(id),
 credential_id text REFERENCES backend_credentials(id),
 model_id text NOT NULL,
 route_generation integer NOT NULL CHECK(route_generation>0),
 streaming boolean NOT NULL,
 state text NOT NULL CHECK(state IN ('running','completed','usage_unknown','failed','cancelled','incomplete')),
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 finished_at timestamptz,
 prompt_tokens bigint CHECK(prompt_tokens>=0),
 completion_tokens bigint CHECK(completion_tokens>=0),
 total_tokens bigint CHECK(total_tokens>=0),
 duration_ms bigint,
 first_token_ms bigint,
 upstream_status integer,
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id),
 CHECK(total_tokens IS NULL OR total_tokens=prompt_tokens+completion_tokens)
);
CREATE INDEX ai_gateway_requests_branch_time ON ai_gateway_requests(branch_id,started_at DESC);
CREATE INDEX ai_gateway_requests_active ON ai_gateway_requests(branch_id,expires_at) WHERE state='running';
