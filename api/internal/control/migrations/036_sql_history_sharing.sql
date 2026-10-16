-- Sharing is an explicit reference to the original encrypted personal journal,
-- not a second plaintext/ciphertext copy. Deletion/retention revokes it naturally.
ALTER TABLE sql_query_history ADD COLUMN shared_at timestamptz;
ALTER TABLE sql_query_history ADD COLUMN share_generation bigint NOT NULL DEFAULT 0 CHECK (share_generation >= 0);
CREATE INDEX sql_history_project_shared ON sql_query_history(project_id,started_at DESC,id DESC) WHERE shared_at IS NOT NULL;
CREATE TABLE sql_history_sharing_requests (
    owner_id text NOT NULL REFERENCES users(id),
    history_id text NOT NULL REFERENCES sql_query_history(id) ON DELETE CASCADE,
    key_hash text NOT NULL,
    request_hash text NOT NULL,
    response jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(owner_id,history_id,key_hash)
);
CREATE INDEX sql_history_sharing_requests_rate ON sql_history_sharing_requests(owner_id,history_id,created_at);
