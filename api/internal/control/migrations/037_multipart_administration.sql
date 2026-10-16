-- A durable, audited intent precedes an administrator's cross-database abort.
-- An interrupted request remains admitted and can be retried with its exact
-- original key/input. No upload bytes or signing credentials enter this ledger.
CREATE TABLE object_multipart_admin_requests (
    id text PRIMARY KEY,
    actor_id text NOT NULL REFERENCES users(id),
    project_id text NOT NULL,
    branch_id text NOT NULL,
    bucket text NOT NULL,
    upload_id text NOT NULL,
    reason text NOT NULL CHECK (length(reason) BETWEEN 8 AND 512),
    key_hash text NOT NULL,
    request_hash text NOT NULL,
    request_id text NOT NULL,
    state text NOT NULL DEFAULT 'admitted' CHECK (state IN ('admitted','confirmed')),
    response jsonb,
    status integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    CHECK ((state='admitted' AND response IS NULL AND status=0 AND finished_at IS NULL)
        OR (state='confirmed' AND response IS NOT NULL AND status BETWEEN 200 AND 499 AND finished_at IS NOT NULL)),
    UNIQUE(actor_id,key_hash)
);
CREATE INDEX multipart_admin_request_rate ON object_multipart_admin_requests(actor_id,created_at);
