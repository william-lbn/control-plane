-- Public identifiers are independent of authentication/CSRF token hashes.
-- Preserve existing sessions and their original expiry; no credential reset.
ALTER TABLE sessions ADD COLUMN id text NOT NULL DEFAULT ('cs_' || replace(gen_random_uuid()::text, '-', ''));
ALTER TABLE sessions ADD COLUMN revoked_at timestamptz;
CREATE UNIQUE INDEX sessions_public_id ON sessions(id);
CREATE INDEX sessions_user_active ON sessions(user_id, created_at DESC) WHERE revoked_at IS NULL;
