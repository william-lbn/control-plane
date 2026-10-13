-- Explicit opt-in: existing functions stay private and manually controlled.
-- Automatic eviction ends unmetered background work; the policy owner must
-- acknowledge this and retain current project editor access at each wake.
CREATE TABLE function_http_policies (
 function_id text PRIMARY KEY REFERENCES function_definitions(id),
 public_enabled boolean NOT NULL DEFAULT false,
 idle_enabled boolean NOT NULL DEFAULT false,
 idle_suspended boolean NOT NULL DEFAULT false,
 actor_id text NOT NULL REFERENCES users(id),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK (NOT idle_enabled OR public_enabled)
);
CREATE TABLE function_policy_requests (
 actor_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 function_id text NOT NULL REFERENCES function_definitions(id),
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(actor_id,key_hash)
);
