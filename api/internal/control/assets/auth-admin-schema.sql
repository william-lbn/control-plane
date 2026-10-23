-- Additive Better Auth 1.7.7 admin fields, including ban enforcement while
-- administration is disabled. Never drop role or ban data on a policy change.
ALTER TABLE neon_auth."user" ADD COLUMN IF NOT EXISTS role text;
ALTER TABLE neon_auth."user" ADD COLUMN IF NOT EXISTS banned boolean DEFAULT false;
ALTER TABLE neon_auth."user" ADD COLUMN IF NOT EXISTS "banReason" text;
ALTER TABLE neon_auth."user" ADD COLUMN IF NOT EXISTS "banExpires" timestamptz;
ALTER TABLE neon_auth.session ADD COLUMN IF NOT EXISTS "impersonatedBy" text;
