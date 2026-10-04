CREATE TABLE IF NOT EXISTS login_failures (
    scope_key text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS login_failures_scope_time ON login_failures(scope_key,occurred_at DESC);
