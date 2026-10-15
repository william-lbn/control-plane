-- Opt-in retention only: existing records remain held until their owner
-- explicitly selects a bounded retention period. Unknown outcomes are excluded.
CREATE TABLE sql_history_preferences (
    owner_id text PRIMARY KEY REFERENCES users(id),
    capture_enabled boolean NOT NULL DEFAULT true,
    retain_days integer NOT NULL DEFAULT 0 CHECK (retain_days IN (0,7,30,90)),
    generation bigint NOT NULL CHECK (generation>0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sql_history_policy_requests (
    owner_id text NOT NULL REFERENCES users(id),
    key_hash text NOT NULL,
    request_hash text NOT NULL,
    response jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(owner_id,key_hash)
);
CREATE INDEX sql_query_history_retention ON sql_query_history(owner_id,started_at)
    WHERE finished_at IS NOT NULL AND outcome IN ('completed','failed','not_submitted');
